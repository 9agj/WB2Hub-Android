package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"wb2hub/internal/multikey"
	"wb2hub/internal/pool"
	"wb2hub/internal/upstream"
	"wb2hub/internal/webtools"
)

// handleKeys lists API keys and creates new ones.
//
// Secrets are masked in the listing; the full secret is returned exactly once,
// at creation, because there is nowhere to recover it from afterwards.
func (h *Handler) handleKeys(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Keys == nil {
		writeJSON(w, http.StatusOK, map[string]any{"keys": []any{}, "enabled": false})
		return
	}

	switch r.Method {
	case http.MethodGet:
		keys := h.cfg.Keys.List()
		out := make([]map[string]any, 0, len(keys))
		for _, k := range keys {
			out = append(out, keyView(k))
		}
		writeJSON(w, http.StatusOK, map[string]any{"keys": out, "enabled": true})

	case http.MethodPost:
		var body struct {
			Name  string `json:"name"`
			Realm string `json:"realm"`
		}
		if err := decodeBody(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
		key, err := h.cfg.Keys.Add(body.Name, body.Realm)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "create_failed", err.Error())
			return
		}
		h.logLine("api key %s created (name=%q realm=%q)", key.ID, key.Name, key.Realm)

		// The one and only time the plaintext secret leaves the server.
		view := keyView(key)
		view["secret"] = key.Secret
		writeJSON(w, http.StatusOK, view)

	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use GET or POST.")
	}
}

// keyView renders a key for the panel with its secret masked.
func keyView(k multikey.Key) map[string]any {
	return map[string]any{
		"id":           k.ID,
		"name":         k.Name,
		"masked":       k.Masked().Secret,
		"realm":        k.Realm,
		"enabled":      k.Enabled,
		"created_at":   k.CreatedAt,
		"last_used_at": k.LastUsedAt,
		"usage":        k.Usage,
	}
}

// handleKeyAction routes per-key operations.
func (h *Handler) handleKeyAction(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Keys == nil {
		writeError(w, http.StatusServiceUnavailable, "keys_disabled", "Key management is not configured.")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/admin/keys/")
	parts := splitPath(rest)
	if len(parts) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "Missing key id.")
		return
	}
	id := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch {
	case r.Method == http.MethodDelete || action == "delete":
		if err := h.cfg.Keys.Remove(id); err != nil {
			writeError(w, http.StatusInternalServerError, "remove_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	case action == "enable", action == "disable":
		on := action == "enable"
		if err := h.cfg.Keys.SetEnabled(id, on); err != nil {
			writeError(w, http.StatusNotFound, "no_such_key", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": on})

	case action == "reset-usage":
		if err := h.cfg.Keys.ResetUsage(id); err != nil {
			writeError(w, http.StatusNotFound, "no_such_key", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	case r.Method == http.MethodPut || r.Method == http.MethodPost:
		var body struct {
			Name  string `json:"name"`
			Realm string `json:"realm"`
		}
		if err := decodeBody(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
		if body.Name != "" {
			if err := h.cfg.Keys.Rename(id, body.Name); err != nil {
				writeError(w, http.StatusNotFound, "no_such_key", err.Error())
				return
			}
		}
		if body.Realm != "" {
			if err := h.cfg.Keys.SetRealm(id, body.Realm); err != nil {
				writeError(w, http.StatusNotFound, "no_such_key", err.Error())
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Unsupported method.")
	}
}

// handleLimits reads and writes the daily quota configuration.
func (h *Handler) handleLimits(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Limits == nil {
		writeError(w, http.StatusServiceUnavailable, "limits_disabled", "Limits are not configured.")
		return
	}

	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, h.cfg.Limits.Snapshot())

	case http.MethodPost, http.MethodPut:
		var body struct {
			DailyCreditLimit     *int64 `json:"daily_credit_limit"`
			DailyTokenLimit      *int64 `json:"daily_token_limit"`
			ModelDailyTokenLimit *int64 `json:"model_daily_token_limit"`
			ReserveCredits       *int64 `json:"reserve_credits"`
		}
		if err := decodeBody(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
		cfg := h.cfg.Limits.Config()
		if body.DailyCreditLimit != nil {
			cfg.DailyCreditLimit = nonNegative(*body.DailyCreditLimit)
		}
		if body.DailyTokenLimit != nil {
			cfg.DailyTokenLimit = nonNegative(*body.DailyTokenLimit)
		}
		if body.ModelDailyTokenLimit != nil {
			cfg.ModelDailyTokenLimit = nonNegative(*body.ModelDailyTokenLimit)
		}
		if body.ReserveCredits != nil {
			cfg.ReserveCredits = nonNegative(*body.ReserveCredits)
		}
		h.cfg.Limits.SetConfig(cfg)
		h.logLine("limits updated: %+v", cfg)
		writeJSON(w, http.StatusOK, h.cfg.Limits.Snapshot())

	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use GET or POST.")
	}
}

// nonNegative clamps a quota to zero-or-more; a negative quota is meaningless
// and would otherwise disable the check in a surprising direction.
func nonNegative(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// handleLogs returns the in-memory log ring.
func (h *Handler) handleLogs(w http.ResponseWriter, r *http.Request) {
	lines := h.Logs()
	limit := queryInt(r, "limit", 200)
	if limit > 0 && len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"lines": lines,
		"total": len(h.Logs()),
	})
}

// handleRestart asks the supervisor to cycle the gateway.
//
// The process cannot restart itself in place on Android — only the host app can
// spawn it — so this records the intent and exits, and the launcher brings it
// back up.
func (h *Handler) handleRestart(w http.ResponseWriter, r *http.Request) {
	h.logLine("restart requested")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": "Restart requested; the host app will cycle the process.",
	})
}

// handleChatCompletions is the OpenAI-compatible entry point.
//
// It resolves an account, forwards the request, and streams the response back.
// When the gateway-run web tools are enabled it also intercepts web_search /
// web_fetch tool calls and executes them locally, because the upstream has no
// search service of its own.
func (h *Handler) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use POST.")
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	r.Body.Close()
	if err != nil {
		writeError(w, http.StatusBadRequest, "read_failed", err.Error())
		return
	}

	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "Invalid JSON body.")
		return
	}

	model, _ := body["model"].(string)
	if strings.TrimSpace(model) == "" {
		model = h.cfg.DefaultModel
	}
	freeModel := h.cfg.Limits.IsFreeModel(model)

	key, _ := h.authenticate(r)
	keyID := ""
	if key != nil {
		keyID = key.ID
	}

	entry, ok := h.cfg.Pool.Pick(now(), pool.ModelOf(model), nil, 0)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "no_account",
			"No account is available to serve this request right now.")
		return
	}
	uid := entry.Account.UID

	// The daily quota gate sits before the upstream call so a capped account is
	// never charged for a request the gateway already knows it must reject.
	if h.cfg.Limits != nil && !h.cfg.Limits.CanServe(uid, pool.ModelOf(model), freeModel) {
		h.cfg.Pool.Cool(uid, pool.CoolCredits, nextMidnight(now()), "")
		writeError(w, http.StatusTooManyRequests, "quota_exhausted",
			"This account has reached its daily quota; the gateway will switch accounts.")
		return
	}

	h.chatMu.Lock()
	h.chatSeq++
	seq := h.chatSeq
	h.chatMu.Unlock()

	if h.cfg.LocalWebTools {
		body = installWebTools(body)
	}

	realm := entry.Account.RealmConfig()
	headers := upstream.Headers(entry.Account, "chat")
	headers.Set("Accept", "text/event-stream")
	headers.Set("X-Request-ID", fmt.Sprintf("%s-%d", upstream.NewRequestID(), seq))

	h.cfg.Pool.Acquire(uid, now())
	defer h.cfg.Pool.Release(uid, true)

	stream := body["stream"] == true
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	result, err := h.cfg.Upstream.StreamChat(ctx, h.clientFor(entry),
		upstream.ChatEndpoint(realm), headers, body, w, stream)
	if err != nil {
		h.cfg.Pool.NoteError(uid, err.Error(), 3, 10*time.Minute)
		h.logLine("chat uid=%s err=%v", uid, err)
		if !result.WroteAnything {
			writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
		}
		return
	}

	if h.cfg.Limits != nil && result.Usage != nil {
		h.cfg.Limits.NoteUsage(uid, pool.ModelOf(model),
			result.Usage.PromptTokens, result.Usage.CompletionTokens, result.Usage.Credits)
	}
	if h.cfg.Keys != nil && keyID != "" && result.Usage != nil {
		_ = h.cfg.Keys.RecordUsage(keyID,
			result.Usage.PromptTokens, result.Usage.CompletionTokens, result.Usage.Credits)
	}
	h.logLine("chat uid=%s model=%s stream=%v", uid, model, stream)
}

// installWebTools swaps any client-declared web tools for the gateway's own.
//
// The client may declare web_search as a server-side tool, as its own function,
// or may echo back the definition we returned last turn. All three collide by
// name, and leaving more than one in place makes the model call the wrong one.
func installWebTools(body map[string]any) map[string]any {
	tools, _ := body["tools"].([]any)
	wantSearch, wantFetch := webtools.ClientWantsWeb(tools)
	if !wantSearch && !wantFetch {
		return body
	}
	body["tools"] = webtools.InstallToolDefs(tools, wantSearch, wantFetch)
	return body
}

// maxRoundsOrZero reports the configured web-tool round budget, or 0 when the
// executor is switched off.
func maxRoundsOrZero(enabled bool) int {
	if !enabled {
		return 0
	}
	return webtools.MaxRounds()
}
