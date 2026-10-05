package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"wb2hub/internal/auth"
	"wb2hub/internal/config"
	"wb2hub/internal/pool"
	"wb2hub/internal/upstream"
)

// handleHealth answers liveness. It carries the pool fields on purpose so the
// Android launcher can tell this service apart from any other program that
// happens to occupy the port and also answers /health with JSON.
func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	total, enabled, ready := h.cfg.Pool.Counts(now())
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"service":  "wb2hub",
		"uptime_s": int(time.Since(h.startedAt).Seconds()),
		"accounts": map[string]int{
			"total":   total,
			"enabled": enabled,
			"ready":   ready,
		},
	})
}

// handleOverview is the panel's dashboard payload.
func (h *Handler) handleOverview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use GET.")
		return
	}
	total, enabled, ready := h.cfg.Pool.Counts(now())

	accounts := make([]map[string]any, 0, total)
	for _, entry := range h.cfg.Pool.List() {
		if entry.Account == nil {
			continue
		}
		view := entry.Account.Snapshot()
		realm := entry.Account.RealmConfig()
		accounts = append(accounts, map[string]any{
			"uid":        view.UID,
			"realm":      view.Realm,
			"realm_name": realm.Name,
			"enabled":    view.Enabled,
			"proxy_slot": view.ProxySlot,
			"expires_at": view.ExpiresAt,
			"cool_kind":  entry.CoolKind.String(),
			"cool_until": entry.CoolUntil,
			"last_error": entry.LastError,
			"successes":  entry.Successes,
			"failures":   entry.Failures,
			"disabled":   entry.Disabled,
		})
	}

	payload := map[string]any{
		"accounts": accounts,
		"counts": map[string]int{
			"total": total, "enabled": enabled, "ready": ready,
		},
		"web_tools": h.cfg.LocalWebTools,
		"uptime_s":  int(time.Since(h.startedAt).Seconds()),
	}

	if h.cfg.Limits != nil {
		payload["limits"] = h.cfg.Limits.Snapshot()
	}
	if h.cfg.Keys != nil {
		payload["key_count"] = len(h.cfg.Keys.List())
	}
	writeJSON(w, http.StatusOK, payload)
}

// handleAccounts lists accounts, or imports one.
func (h *Handler) handleAccounts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		views := make([]any, 0)
		for _, entry := range h.cfg.Pool.List() {
			if entry.Account != nil {
				views = append(views, entry.Account.Snapshot())
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"accounts": views})

	case http.MethodPost:
		h.importAccount(w, r)

	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use GET or POST.")
	}
}

// importAccount accepts a pasted credential blob and writes it to the auth dir.
//
// It deliberately does not talk upstream: importing is an offline operation so
// an operator can stage credentials while the upstream is unreachable.
func (h *Handler) importAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UID          string `json:"uid"`
		Realm        string `json:"realm"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ProxySlot    string `json:"proxy_slot"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if strings.TrimSpace(body.AccessToken) == "" {
		writeError(w, http.StatusBadRequest, "missing_token", "access_token is required.")
		return
	}

	acct := auth.AccountFromFields(body.UID, body.Realm, body.AccessToken, body.RefreshToken, body.ProxySlot, h.cfg.AuthDir)
	if acct == nil {
		writeError(w, http.StatusBadRequest, "bad_credential", "Could not derive a uid from the token.")
		return
	}
	if err := acct.Save(); err != nil {
		writeError(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	h.cfg.Pool.Add(acct)
	h.logLine("account %s imported (realm=%s)", acct.UID, acct.RealmID())
	writeJSON(w, http.StatusOK, acct.Snapshot())
}

// handleAccountAction routes per-account operations.
func (h *Handler) handleAccountAction(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/admin/accounts/")
	parts := splitPath(rest)
	if len(parts) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "Missing account uid.")
		return
	}
	uid := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	entry, ok := h.findEntry(uid)
	if !ok {
		writeError(w, http.StatusNotFound, "no_such_account", "No account with that uid.")
		return
	}

	switch action {
	case "":
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, entry.Account.Snapshot())
		case http.MethodDelete:
			h.cfg.Pool.Remove(uid)
			h.logLine("account %s removed", uid)
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		default:
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use GET or DELETE.")
		}

	case "enable", "disable":
		on := action == "enable"
		body := struct {
			Enabled *bool `json:"enabled"`
		}{}
		if r.Body != nil {
			_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
			r.Body.Close()
		}
		if body.Enabled != nil {
			on = *body.Enabled
		}
		entry.Account.SetEnabled(on)
		if err := entry.Account.Save(); err != nil {
			writeError(w, http.StatusInternalServerError, "save_failed", err.Error())
			return
		}
		h.logLine("account %s enabled=%v", uid, on)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": on})

	case "clear-cooling", "revive":
		h.cfg.Pool.Cool(uid, "", time.Time{}, "")
		entry.Account.SetEnabled(true)
		_ = entry.Account.Save()
		h.logLine("account %s cooling cleared", uid)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	case "checkin":
		h.accountCheckin(w, r, entry)

	case "models":
		h.accountModels(w, r, entry)

	default:
		writeError(w, http.StatusNotFound, "unknown_action", "Unknown account action: "+action)
	}
}

// findEntry looks an account up by uid.
func (h *Handler) findEntry(uid string) (pool.EntryView, bool) {
	return h.cfg.Pool.View(uid)
}

// accountCheckin performs the daily check-in for one account.
//
// The international realm has no check-in at all, so it is refused explicitly
// rather than sending a request upstream that is guaranteed to 404.
func (h *Handler) accountCheckin(w http.ResponseWriter, r *http.Request, entry pool.EntryView) {
	realm := entry.Account.RealmConfig()
	if !realm.HasCheckin {
		writeError(w, http.StatusBadRequest, "no_checkin",
			fmt.Sprintf("%s has no daily check-in.", realm.Name))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	headers := upstream.Headers(entry.Account, "billing")
	url := realm.BillingUpstream + config.PathDailyCheckin

	payload := map[string]any{}
	statusCode, err := h.cfg.Upstream.PostJSON(ctx, h.clientFor(entry), url, headers, payload)
	if err != nil {
		h.cfg.Pool.NoteError(entry.Account.UID, err.Error(), 3, 10*time.Minute)
		writeError(w, http.StatusBadGateway, "checkin_failed", err.Error())
		return
	}
	h.logLine("account %s checkin -> HTTP %d", entry.Account.UID, statusCode)
	writeJSON(w, http.StatusOK, map[string]any{"ok": statusCode < 400, "status": statusCode})
}

// accountModels fetches the model catalog visible to one account.
func (h *Handler) accountModels(w http.ResponseWriter, r *http.Request, entry pool.EntryView) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	realm := entry.Account.RealmConfig()
	headers := upstream.Headers(entry.Account, "chat")

	doc, raw, status, err := h.cfg.Upstream.DoJSON(ctx, h.clientFor(entry),
		http.MethodGet, upstream.ModelsEndpoint(realm), headers, nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_unreachable", err.Error())
		return
	}
	if status >= 400 {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "status": status, "body": string(raw),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": status, "models": doc})
}

// clientFor returns an http client that egresses the way this account should.
func (h *Handler) clientFor(entry pool.EntryView) *http.Client {
	slotID := entry.Account.Proxy()
	if slotID == "" || h.cfg.Slots == nil {
		return h.cfg.Upstream.HTTP
	}
	slot, ok := h.cfg.Slots.Get(slotID)
	if !ok {
		return h.cfg.Upstream.HTTP
	}
	rawURL, ok := slot.URLFor()
	if !ok {
		return h.cfg.Upstream.HTTP
	}
	// A client bound to a slot that has never been tested still works: the
	// proxy url is enough for the transport.
	return h.cfg.Upstream.ForAccount(rawURL)
}

// handleModels proxies the model catalog for the OpenAI-compatible surface.
func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.cfg.Pool.Pick(now(), "", nil, 0)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "no_account",
			"No account is available to serve this request.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	realm := entry.Account.RealmConfig()
	headers := upstream.Headers(entry.Account, "chat")
	doc, raw, status, err := h.cfg.Upstream.DoJSON(ctx, h.clientFor(entry),
		http.MethodGet, upstream.ModelsEndpoint(realm), headers, nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_unreachable", err.Error())
		return
	}
	if status >= 400 {
		var passthrough any
		if json.Unmarshal(raw, &passthrough) != nil {
			passthrough = string(raw)
		}
		writeJSON(w, status, passthrough)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}
