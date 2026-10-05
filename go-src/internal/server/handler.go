// Package server exposes the gateway over HTTP.
//
// Two surfaces live here: the OpenAI-compatible /v1/* API that clients talk to,
// and the /admin/* panel API that the Android UI drives. The panel is a native
// view in this build (not a WebView), so /admin/* is JSON-only — there is no
// HTML to serve.
//
// Ported from the routing half of the Python reference (wb_proxy.py).
package server

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"wb2hub/internal/auth"
	"wb2hub/internal/codearts"
	"wb2hub/internal/config"
	"wb2hub/internal/limits"
	"wb2hub/internal/logring"
	"wb2hub/internal/multikey"
	"wb2hub/internal/pool"
	"wb2hub/internal/proxy"
	"wb2hub/internal/scheduler"
	"wb2hub/internal/upstream"
	"wb2hub/internal/webtools"
)

// Config wires the handler to its collaborators.
type Config struct {
	Pool         *pool.Pool
	Upstream     *upstream.Client
	Keys         *multikey.Store
	Slots        *proxy.Store
	Limits       *limits.Tracker
	AuthDir      string
	UsageDir     string
	DefaultModel string

	// RequireKey forces bearer authentication on /v1/*. It is on whenever the
	// listener is reachable from anywhere but loopback.
	RequireKey bool

	// LocalWebTools enables the gateway-run web_search / web_fetch executor.
	LocalWebTools bool

	// Logs is the shared log ring. The gateway attaches the standard logger to
	// it as well, so a panic or a startup line lands in the same view the panel
	// reads.
	Logs *logring.Ring

	// Scheduler is the daily welfare scheduler. Nil disables the panel's
	// schedule view and its manual triggers.
	Scheduler *scheduler.Scheduler
}

// Handler is the root HTTP handler.
type Handler struct {
	cfg Config

	// logs keeps the most recent lines for the panel without growing without
	// bound. The panel polls with a sequence cursor, so the ring has to hand out
	// stable sequence numbers rather than just the last N strings.
	logs *logring.Ring

	startedAt time.Time

	chatMu  sync.Mutex
	chatSeq int64

	// codearts login state. Only one interactive login runs at a time: the flow
	// waits on a browser redirect to a fixed loopback port, so a second attempt
	// would fight the first for that port. Holding the state here lets a
	// concurrent caller observe the running attempt instead of failing
	// obscurely.
	codeartsMu        sync.Mutex
	codeartsAuthURL   string
	codeartsStage     string
	codeartsError     string
	codeartsPKCE      codearts.PkcePair
	codeartsState     string
	codeartsStartedAt time.Time

	// codeartsCallbackPort is where the browser redirect lands.
	codeartsCallbackPort int
}

// callbackPort returns the configured OAuth callback port, defaulting to 18081.
func (h *Handler) callbackPort() int {
	if h.codeartsCallbackPort > 0 {
		return h.codeartsCallbackPort
	}
	return 18081
}

// NewHandler builds a handler.
func NewHandler(cfg Config) *Handler {
	if cfg.Pool == nil {
		cfg.Pool = pool.New("")
	}
	if cfg.Upstream == nil {
		cfg.Upstream = upstream.New()
	}
	if cfg.Logs == nil {
		cfg.Logs = logring.New(500)
	}
	return &Handler{cfg: cfg, logs: cfg.Logs, startedAt: time.Now()}
}

// now is indirected so tests can pin the clock.
var now = time.Now

// Routes registers every endpoint and returns the mux.
func (h *Handler) Routes() *http.ServeMux {
	mux := http.NewServeMux()

	// ---- OpenAI-compatible surface -------------------------------------
	mux.HandleFunc("/health", h.handleHealth)
	mux.HandleFunc("/v1/models", h.withKey(h.handleModels))
	mux.HandleFunc("/v1/chat/completions", h.withKey(h.handleChatCompletions))
	mux.HandleFunc("/v1/responses", h.withKey(h.handleChatCompletions))
	mux.HandleFunc("/v1/completions", h.withKey(h.handleChatCompletions))

	// ---- Account administration ----------------------------------------
	mux.HandleFunc("/admin/overview", h.withKey(h.handleOverview))
	mux.HandleFunc("/admin/accounts", h.withKey(h.handleAccounts))
	mux.HandleFunc("/admin/accounts/", h.withKey(h.handleAccountAction))

	// ---- Proxy slots (ported feature) ----------------------------------
	mux.HandleFunc("/admin/proxy/slots", h.withKey(h.handleProxySlots))
	mux.HandleFunc("/admin/proxy/slots/", h.withKey(h.handleProxySlotAction))
	mux.HandleFunc("/admin/proxy/discover", h.withKey(h.handleProxyDiscover))

	// ---- Multi-key management (ported feature) -------------------------
	mux.HandleFunc("/admin/keys", h.withKey(h.handleKeys))
	mux.HandleFunc("/admin/keys/", h.withKey(h.handleKeyAction))

	// ---- Daily limits (ported feature) ---------------------------------
	mux.HandleFunc("/admin/limits", h.withKey(h.handleLimits))

	// ---- Web tools (ported feature) ------------------------------------
	mux.HandleFunc("/admin/webtools", h.withKey(h.handleWebTools))

	// ---- CodeArts upstream (Huawei) ------------------------------------
	mux.HandleFunc("/admin/codearts/login/start", h.withKey(h.handleCodeartsLoginStart))
	mux.HandleFunc("/admin/codearts/login/status", h.withKey(h.handleCodeartsLoginStatus))
	mux.HandleFunc("/admin/codearts/import", h.withKey(h.handleCodeartsImport))
	mux.HandleFunc("/admin/codearts/status", h.withKey(h.handleCodeartsStatus))
	mux.HandleFunc("/admin/codearts/models", h.withKey(h.handleCodeartsModels))
	mux.HandleFunc("/admin/codearts/checkin", h.withKey(h.handleCodeartsCheckin))

	// ---- Growth centre (China realm only) ------------------------------
	mux.HandleFunc("/admin/growth", h.withKey(h.handleGrowthView))
	mux.HandleFunc("/admin/growth/", h.withKey(h.handleGrowthAction))

	// ---- Trial / quota -------------------------------------------------
	mux.HandleFunc("/admin/trial", h.withKey(h.handleTrialView))
	mux.HandleFunc("/admin/trial/", h.withKey(h.handleTrialAction))

	// ---- Scheduled welfare runs ----------------------------------------
	mux.HandleFunc("/admin/scheduler", h.withKey(h.handleSchedulerStatus))
	mux.HandleFunc("/admin/scheduler/run", h.withKey(h.handleSchedulerRun))
	mux.HandleFunc("/admin/scheduler/config", h.withKey(h.handleSchedulerConfig))

	// ---- Logs / diagnostics --------------------------------------------
	mux.HandleFunc("/admin/logs", h.withKey(h.handleLogs))
	mux.HandleFunc("/admin/gateway/restart", h.withKey(h.handleRestart))

	return mux
}

// ---------------------------------------------------------------- middleware

// withKey enforces bearer authentication when the gateway is exposed.
//
// The loopback case is deliberately exempt only when RequireKey is false: that
// is the single-device Android deployment where the app and the server are the
// same process boundary.
func (h *Handler) withKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.RequireKey {
			if _, ok := h.authenticate(r); !ok {
				writeError(w, http.StatusUnauthorized, "invalid_api_key",
					"Missing or invalid API key.")
				return
			}
		}
		next(w, r)
	}
}

// authenticate resolves the caller's API key.
//
// The multi-key store is consulted first; when it is empty the request is
// accepted only on loopback so a fresh install is usable before the operator
// creates a key.
func (h *Handler) authenticate(r *http.Request) (*multikey.Key, bool) {
	secret := bearerToken(r)
	if secret == "" {
		secret = r.Header.Get("x-api-key")
	}
	if secret == "" {
		return nil, !h.cfg.RequireKey && isLoopback(r)
	}
	if h.cfg.Keys != nil {
		if key, ok := h.cfg.Keys.Authenticate(secret); ok {
			return key, true
		}
	}
	return nil, false
}

func bearerToken(r *http.Request) string {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if header == "" {
		return ""
	}
	if len(header) > 7 && strings.EqualFold(header[:7], "bearer ") {
		return strings.TrimSpace(header[7:])
	}
	return header
}

func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// ------------------------------------------------------------------ helpers

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(payload)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    code,
			"code":    code,
		},
	})
}

func decodeBody(r *http.Request, into any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 32<<20))
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

// logLine appends to the ring the panel reads.
func (h *Handler) logLine(format string, args ...any) {
	if h.logs == nil {
		return
	}
	h.logs.Write([]byte(fmt.Sprintf(format, args...) + "\n"))
}

// Logs returns the ring contents, oldest first.
//
// The timestamp is re-attached because the ring stores it separately for the
// panel's structured view; this flat form is what a caller pasting logs into a
// bug report wants.
func (h *Handler) Logs() []string {
	if h.logs == nil {
		return []string{}
	}
	out := make([]string, 0, h.logs.Len())
	for _, line := range h.logs.Snapshot() {
		out = append(out, line.At+" "+line.Text)
	}
	return out
}

// LogsSince returns lines newer than seq, for the panel's incremental poll.
func (h *Handler) LogsSince(seq uint64) []logring.Line {
	if h.logs == nil {
		return []logring.Line{}
	}
	return h.logs.SnapshotSince(seq)
}

// LogRing exposes the ring so the gateway can attach the standard logger to it.
func (h *Handler) LogRing() *logring.Ring { return h.logs }

// nextMidnight returns the next local midnight, when daily quotas reset.
func nextMidnight(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location()).AddDate(0, 0, 1)
}

// ensure the imported packages are all referenced by this file's siblings
var (
	_ = config.RealmIntl
	_ = auth.Filename
	_ = webtools.WebSearchName
)
