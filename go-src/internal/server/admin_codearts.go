package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"wb2hub/internal/auth"
	"wb2hub/internal/codearts"
)

// Operator-facing failures with a specific message, as opposed to a generic 400.
var (
	errCredentialsUnusable    = errors.New("the credential has no usable key material")
	errNoAccountForCredential = errors.New("no account matched the credential; pass uid to pick one")
	errNoCodeartsAccount      = errors.New("no account carries a CodeArts credential")
)

// handleCodeartsLoginStart begins the PKCE flow and returns the authorize URL.
func (h *Handler) handleCodeartsLoginStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use POST.")
		return
	}

	var body struct {
		CallbackPort int `json:"callback_port"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
		r.Body.Close()
	}
	if body.CallbackPort == 0 {
		body.CallbackPort = h.callbackPort()
	}
	if body.CallbackPort == 0 {
		body.CallbackPort = 18081
	}

	redirect := "http://127.0.0.1:" + itoa(body.CallbackPort) + "/callback"
	authURL, pkce, state, err := codearts.StartOAuthFlow(redirect)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "pkce_failed", err.Error())
		return
	}

	h.codeartsMu.Lock()
	h.codeartsAuthURL = authURL
	h.codeartsStage = "waiting_for_browser"
	h.codeartsError = ""
	h.codeartsPKCE = pkce
	h.codeartsState = state
	h.codeartsStartedAt = time.Now()
	h.codeartsMu.Unlock()

	h.logLine("codearts login started; authorize URL issued")

	// The callback listener and the exchange run in the background: the operator
	// must be able to open the URL while the gateway waits.
	go h.codeartsAwaitCallback(body.CallbackPort, redirect, pkce, state)

	writeJSON(w, http.StatusOK, map[string]any{
		"auth_url": authURL,
		"stage":    "waiting_for_browser",
		"note":     "Open auth_url in a browser and approve. The gateway is listening on the callback port.",
	})
}

// codeartsAwaitCallback waits for the redirect, exchanges the code, and stores
// the resulting credential on the account.
func (h *Handler) codeartsAwaitCallback(port int, redirect string, pkce codearts.PkcePair, state string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	h.setCodeartsStage("awaiting_callback")
	code, err := codearts.ListenOnCallbackPort(ctx, port, state, 8*time.Minute)
	if err != nil {
		h.failCodearts(err.Error())
		return
	}

	h.setCodeartsStage("exchanging")
	cred, err := codearts.ExchangeCodeForToken(ctx, h.cfg.Upstream.Direct, code, pkce, redirect)
	if err != nil {
		h.failCodearts(err.Error())
		return
	}

	if err := h.storeCodeartsCredential(cred); err != nil {
		h.failCodearts(err.Error())
		return
	}

	h.codeartsMu.Lock()
	h.codeartsStage = "done"
	h.codeartsError = ""
	h.codeartsMu.Unlock()
	h.logLine("codearts login ok: ak=%s", maskSecret(cred.AccessKeyID))
}

// setCodeartsStage records progress for the polling endpoint.
func (h *Handler) setCodeartsStage(stage string) {
	h.codeartsMu.Lock()
	h.codeartsStage = stage
	h.codeartsMu.Unlock()
}

func (h *Handler) failCodearts(message string) {
	h.codeartsMu.Lock()
	h.codeartsStage = "failed"
	h.codeartsError = message
	h.codeartsMu.Unlock()
	h.logLine("codearts login failed: %s", message)
}

// storeCodeartsCredential attaches a credential to the account it belongs to.
//
// The UID comes from the credential's own user id when present; otherwise the
// caller must have exactly one account, and that one is used. Guessing otherwise
// would silently attach a credential to the wrong identity.
func (h *Handler) storeCodeartsCredential(cred *codearts.Credential) error {
	if cred == nil || !cred.Usable() {
		return errCredentialsUnusable
	}

	var target *auth.Account
	if cred.UserID != "" {
		for _, entry := range h.cfg.Pool.Views() {
			if entry.Account != nil && entry.Account.UID == cred.UserID {
				target = entry.Account
				break
			}
		}
	}
	if target == nil {
		views := h.cfg.Pool.Views()
		if len(views) == 1 && views[0].Account != nil {
			target = views[0].Account
		}
	}
	if target == nil {
		return errNoAccountForCredential
	}

	raw, err := json.Marshal(cred)
	if err != nil {
		return err
	}
	target.SetCodearts(raw)
	if err := target.Save(); err != nil {
		return err
	}
	h.logLine("codearts credential stored on account %s", target.UID)
	return nil
}

// handleCodeartsLoginStatus reports progress of the interactive login.
func (h *Handler) handleCodeartsLoginStatus(w http.ResponseWriter, r *http.Request) {
	h.codeartsMu.Lock()
	defer h.codeartsMu.Unlock()

	payload := map[string]any{
		"stage":      h.codeartsStage,
		"auth_url":   h.codeartsAuthURL,
		"error":      h.codeartsError,
		"started_at": h.codeartsStartedAt,
	}
	switch h.codeartsStage {
	case "":
		payload["stage"] = "idle"
	case "done":
		payload["ok"] = true
	}
	writeJSON(w, http.StatusOK, payload)
}

// handleCodeartsImport takes an AK/SK pair directly, bypassing the browser.
//
// This is the path for an operator who already holds credentials. The security
// token is optional: without one the account is marked as needing a refresh,
// which the manager attempts on first use.
func (h *Handler) handleCodeartsImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use POST.")
		return
	}

	var body struct {
		UID             string `json:"uid"`
		AccessKeyID     string `json:"access_key_id"`
		SecretAccessKey string `json:"secret_access_key"`
		SecurityToken   string `json:"security_token"`
		ExpiresAt       string `json:"expires_at"`
		UserName        string `json:"user_name"`
		UserID          string `json:"user_id"`
		DomainID        string `json:"domain_id"`
		TicketID        string `json:"ticket_id"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}

	// A ticket id is the browser-flow short cut: the credential is fetched from
	// snap-manager rather than supplied.
	if strings.TrimSpace(body.TicketID) != "" && strings.TrimSpace(body.AccessKeyID) == "" {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		cred, err := codearts.PollForCredential(ctx, h.cfg.Upstream.Direct,
			body.TicketID, 2*time.Second, 4*time.Minute)
		if err != nil {
			writeError(w, http.StatusBadGateway, "ticket_failed", err.Error())
			return
		}
		if err := h.storeCodeartsCredential(cred); err != nil {
			writeError(w, http.StatusBadRequest, "store_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         true,
			"access_key": maskSecret(cred.AccessKeyID),
			"has_token":  cred.SecurityToken != "",
			"expires_at": cred.ExpiresAt,
		})
		return
	}

	if strings.TrimSpace(body.AccessKeyID) == "" || strings.TrimSpace(body.SecretAccessKey) == "" {
		writeError(w, http.StatusBadRequest, "missing_keys",
			"access_key_id and secret_access_key are required (or a ticket_id).")
		return
	}

	cred := &codearts.Credential{
		AccessKeyID:     strings.TrimSpace(body.AccessKeyID),
		SecretAccessKey: strings.TrimSpace(body.SecretAccessKey),
		SecurityToken:   strings.TrimSpace(body.SecurityToken),
		ExpiresAt:       strings.TrimSpace(body.ExpiresAt),
		UserName:        strings.TrimSpace(body.UserName),
		UserID:          strings.TrimSpace(body.UserID),
		DomainID:        strings.TrimSpace(body.DomainID),
	}
	if cred.UserID == "" {
		cred.UserID = strings.TrimSpace(body.UID)
	}

	if err := h.storeCodeartsCredential(cred); err != nil {
		writeError(w, http.StatusBadRequest, "store_failed", err.Error())
		return
	}

	// Report what is still missing rather than claiming success outright: an
	// AK/SK pair with no security token cannot sign a request until the first
	// refresh succeeds.
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"access_key":    maskSecret(cred.AccessKeyID),
		"has_token":     cred.SecurityToken != "",
		"needs_refresh": cred.SecurityToken == "",
		"expires_at":    cred.ExpiresAt,
		"hint":          "A security token is minted on first use; watch /admin/logs if it fails.",
	})
}

// handleCodeartsModels lists the models a CodeArts account can reach.
func (h *Handler) handleCodeartsModels(w http.ResponseWriter, r *http.Request) {
	mgr, err := h.codeartsManager()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "codearts_unavailable", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	models, err := mgr.FetchModels(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, "fetch_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models, "count": len(models)})
}

// handleCodeartsCheckin claims the CodeArts daily check-in.
func (h *Handler) handleCodeartsCheckin(w http.ResponseWriter, r *http.Request) {
	mgr, err := h.codeartsManager()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "codearts_unavailable", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	result, err := mgr.ClaimDailyCheckin(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, "checkin_failed", err.Error())
		return
	}
	h.logLine("codearts checkin ok")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": result})
}

// handleCodeartsStatus reports which account has a usable credential.
func (h *Handler) handleCodeartsStatus(w http.ResponseWriter, r *http.Request) {
	out := []map[string]any{}
	for _, entry := range h.cfg.Pool.Views() {
		if entry.Account == nil {
			continue
		}
		item := map[string]any{
			"uid":          entry.Account.UID,
			"has_codearts": entry.Account.HasCodearts(),
		}
		if raw := entry.Account.CodeartsRaw(); raw != nil {
			var cred codearts.Credential
			if json.Unmarshal(raw, &cred) == nil {
				item["access_key"] = maskSecret(cred.AccessKeyID)
				item["has_token"] = cred.SecurityToken != ""
				item["expires_at"] = cred.ExpiresAt
				if expiry := cred.Expiry(); !expiry.IsZero() {
					item["expires_in_s"] = int(time.Until(expiry).Seconds())
				}
				item["needs_refresh"] = cred.NeedsRefresh(time.Now(), 5*time.Minute)
				item["user_name"] = cred.UserName
			}
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

// codeartsManager builds a manager for the first account that has a credential.
//
// CodeArts is a per-account upstream but the panel operations act on one at a
// time; picking the first usable account keeps the endpoints simple, and the
// status endpoint tells the operator when there is more than one.
func (h *Handler) codeartsManager() (*codearts.Manager, error) {
	for _, entry := range h.cfg.Pool.Views() {
		if entry.Account == nil || !entry.Account.HasCodearts() {
			continue
		}
		acct := entry.Account
		return codearts.NewManager(codearts.Options{
			Credential: credentialFromRaw(acct.CodeartsRaw()),
			Realm:      acct.RealmID(),
			Client:     h.clientFor(entry),
			OnSave: func(cred *codearts.Credential) error {
				raw, err := json.Marshal(cred)
				if err != nil {
					return err
				}
				acct.SetCodearts(raw)
				return acct.Save()
			},
		})
	}
	return nil, errNoCodeartsAccount
}

// credentialFromRaw decodes a stored blob, tolerating an unreadable one by
// returning an empty credential so the caller reports "not usable" rather than
// panicking on nil.
func credentialFromRaw(raw json.RawMessage) *codearts.Credential {
	if raw == nil {
		return &codearts.Credential{}
	}
	var cred codearts.Credential
	if err := json.Unmarshal(raw, &cred); err != nil {
		return &codearts.Credential{}
	}
	return &cred
}

// maskSecret renders a secret for display: enough to recognise, not to use.
func maskSecret(secret string) string {
	s := strings.TrimSpace(secret)
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return strings.Repeat("*", len(s))
	}
	return s[:4] + strings.Repeat("*", 4) + s[len(s)-4:]
}

// itoa avoids importing strconv for one call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
