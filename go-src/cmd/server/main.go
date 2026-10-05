// cmd/server is the gateway entry point.
//
// It wires the pool, the key store, the proxy-slot store and the quota tracker
// together and serves HTTP. Configuration comes from TW2H_* environment
// variables first, then a JSON file, then built-in defaults — the environment
// wins because that is how the Android host app passes the writable paths in,
// and those must never be overridden by a stale file left in the app's data
// directory by a previous version.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"wb2hub/internal/auth"
	"wb2hub/internal/config"
	"wb2hub/internal/growth"
	"wb2hub/internal/limits"
	"wb2hub/internal/logring"
	"wb2hub/internal/multikey"
	"wb2hub/internal/pool"
	"wb2hub/internal/proxy"
	"wb2hub/internal/scheduler"
	"wb2hub/internal/server"
	"wb2hub/internal/trial"
	"wb2hub/internal/upstream"
)

// version is stamped at build time with -ldflags -X main.version=...
var version = "dev"

func main() {
	configPath := flag.String("config", "", "path to config json (optional)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("wb2hub", version)
		return
	}

	cfg, err := Load(*configPath)
	if err != nil {
		log.Fatalf("[fatal] load config: %v", err)
	}

	// The ring is created before anything else logs, and the standard logger is
	// tee'd into it. Without the tee, a panic traceback or a startup failure
	// would be visible only on stderr, which the Android host app discards.
	logRing := logring.New(cfg.LogCapacity)
	log.SetOutput(io.MultiWriter(os.Stderr, logRing))

	app, err := build(cfg, logRing)
	if err != nil {
		log.Fatalf("[fatal] %v", err)
	}
	defer app.Close()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           app.Handler.Routes(),
		ReadHeaderTimeout: 30 * time.Second,
		// No global write timeout: a streamed completion legitimately stays
		// open for minutes, and a deadline here would cut long answers off.
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	// The listener is created before printing the banner so a port clash is
	// reported as a plain error rather than a panic inside ListenAndServe.
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		log.Fatalf("[fatal] listen %s: %v", cfg.Listen, err)
	}

	log.Printf("[start] wb2hub %s listening on %s", version, cfg.Listen)
	log.Printf("[start] accounts=%d auth_dir=%s", app.PoolCount(), cfg.AuthDir)

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("[fatal] serve: %v", err)
	}
	log.Printf("[stop] wb2hub exited")
}

// App holds the wired components so main stays readable.
type App struct {
	Handler   *server.Handler
	Pool      *pool.Pool
	Keys      *multikey.Store
	Slots     *proxy.Store
	Limits    *limits.Tracker
	Scheduler *scheduler.Scheduler

	stopFlush chan struct{}
	cancel    context.CancelFunc
}

// PoolCount is a small convenience for the startup banner.
func (a *App) PoolCount() int { return len(a.Pool.List()) }

// Close stops background work.
func (a *App) Close() {
	if a.Scheduler != nil {
		a.Scheduler.Stop()
	}
	if a.cancel != nil {
		a.cancel()
	}
	if a.stopFlush != nil {
		close(a.stopFlush)
	}
	_ = a.Pool.Flush()
}

// build assembles every component from the resolved configuration.
func build(cfg *Config, logRing *logring.Ring) (*App, error) {
	if err := os.MkdirAll(cfg.AuthDir, 0o700); err != nil {
		return nil, fmt.Errorf("create auth dir: %w", err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	accounts, problems := auth.LoadDir(cfg.AuthDir)
	for _, problem := range problems {
		log.Printf("[warn] skipped credential: %v", problem)
	}
	log.Printf("[auth] loaded %d account(s) from %s", len(accounts), cfg.AuthDir)

	p := pool.New(filepath.Join(cfg.DataDir, "state.json"))
	_ = p.Restore(accounts)

	keys, err := multikey.NewStore(filepath.Join(cfg.DataDir, "api_keys.json"))
	if err != nil {
		return nil, fmt.Errorf("open key store: %w", err)
	}
	// Seed from the pre-existing single key so clients configured against the
	// old build keep working across the upgrade.
	if err := keys.EnsureSeed(cfg.LegacyAPIKey); err != nil {
		log.Printf("[warn] key seed: %v", err)
	}

	slots, err := proxy.NewStore(filepath.Join(cfg.DataDir, "proxy_slots.json"))
	if err != nil {
		return nil, fmt.Errorf("open proxy slot store: %w", err)
	}

	tracker := limits.NewTracker(time.Now, cfg.FreeModels...)
	tracker.SetConfig(limits.Config{
		DailyCreditLimit:     cfg.Limits.DailyCreditLimit,
		DailyTokenLimit:      cfg.Limits.DailyTokenLimit,
		ModelDailyTokenLimit: cfg.Limits.ModelDailyTokenLimit,
		ReserveCredits:       cfg.Limits.ReserveCredits,
	})

	up := upstream.New()
	up.HTTP.Timeout = time.Duration(cfg.UpstreamTimeoutSeconds) * time.Second

	// The scheduler shares the pool and the upstream client with the request
	// path on purpose: a run must see the same account state a request would,
	// and a check-in that renews a token has to update the account the request
	// path is about to use.
	sched := scheduler.New(scheduler.Config{
		Pool:           p,
		Growth:         growth.New(up),
		Trial:          trial.New(up),
		CheckinHours:   cfg.Scheduler.CheckinHours,
		TravelHours:    cfg.Scheduler.TravelHours,
		KeepaliveHours: cfg.Scheduler.KeepaliveHours,
		CatHours:       cfg.Scheduler.CatHours,
		Log:            log.Printf,
	})

	handler := server.NewHandler(server.Config{
		Pool:          p,
		Upstream:      up,
		Keys:          keys,
		Slots:         slots,
		Limits:        tracker,
		AuthDir:       cfg.AuthDir,
		UsageDir:      cfg.UsageDir,
		DefaultModel:  cfg.DefaultModel,
		RequireKey:    cfg.RequireKey,
		LocalWebTools: cfg.LocalWebTools,
		Logs:          logRing,
		Scheduler:     sched,
	})

	app := &App{
		Handler:   handler,
		Pool:      p,
		Keys:      keys,
		Slots:     slots,
		Limits:    tracker,
		Scheduler: sched,
	}
	app.stopFlush = make(chan struct{})
	p.StartFlusher(app.stopFlush, 30*time.Second)

	if cfg.Scheduler.Enabled {
		runCtx, cancel := context.WithCancel(context.Background())
		app.cancel = cancel
		go sched.Run(runCtx)
		log.Printf("[scheduler] 定时巡检已启用: %s", sched.Status().Mode)
	} else {
		log.Printf("[scheduler] 定时巡检已关闭 (TW2H_SCHEDULER=0)")
	}
	return app, nil
}

// Config is the resolved runtime configuration.
type Config struct {
	Listen       string
	AuthDir      string
	DataDir      string
	UsageDir     string
	DefaultModel string

	// RequireKey forces bearer auth on the API surface. It is implied when the
	// listener is not loopback-only.
	RequireKey bool

	// LegacyAPIKey is the single key earlier builds hard-coded. It is only used
	// to seed the new store on first run.
	LegacyAPIKey string

	LocalWebTools          bool
	UpstreamTimeoutSeconds int
	FreeModels             []string

	// LogCapacity is how many log lines the panel can scroll back through.
	LogCapacity int

	Scheduler struct {
		Enabled        bool  `json:"enabled"`
		CheckinHours   []int `json:"checkin_hours"`
		TravelHours    []int `json:"travel_hours"`
		KeepaliveHours []int `json:"keepalive_hours"`
		CatHours       []int `json:"cat_hours"`
	} `json:"scheduler"`

	Limits struct {
		DailyCreditLimit     int64 `json:"daily_credit_limit"`
		DailyTokenLimit      int64 `json:"daily_token_limit"`
		ModelDailyTokenLimit int64 `json:"model_daily_token_limit"`
		ReserveCredits       int64 `json:"reserve_credits"`
	} `json:"limits"`
}

// Load resolves configuration: JSON file (if any) over defaults, then env vars.
func Load(path string) (*Config, error) {
	cfg := defaults()

	if path == "" {
		path = os.Getenv("TW2H_CONFIG")
	}
	if path != "" {
		raw, err := os.ReadFile(path)
		switch {
		case err == nil:
			var fileCfg Config
			if err := json.Unmarshal(raw, &fileCfg); err != nil {
				return nil, fmt.Errorf("parse %s: %w", path, err)
			}
			mergeFile(cfg, &fileCfg)
		case os.IsNotExist(err):
			// A missing file is fine: env + defaults are a valid configuration.
		default:
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
	}

	applyEnv(cfg)
	normalise(cfg)
	return cfg, nil
}

func defaults() *Config {
	home, _ := os.UserHomeDir()
	if home == "" {
		home = "."
	}
	base := filepath.Join(home, "wb2hub")

	cfg := &Config{
		Listen:                 "127.0.0.1:7863",
		AuthDir:                filepath.Join(base, "auths"),
		DataDir:                filepath.Join(base, "data"),
		UsageDir:               filepath.Join(base, "usage"),
		DefaultModel:           "auto",
		UpstreamTimeoutSeconds: 120,
		LegacyAPIKey:           "wb2hub-local-key",
		LogCapacity:            500,
	}
	// Scheduled welfare runs are opt-in: they touch every account on a timer, so
	// an operator should have to say yes to that rather than discover it.
	cfg.Scheduler.Enabled = false
	// Only the CN realm has check-in, but the free-model list is about quota
	// accounting, not check-in, so it stays realm-agnostic.
	cfg.FreeModels = []string{}
	return cfg
}

// mergeFile overlays only the fields the file actually set, so a partial config
// file does not silently blank the rest.
func mergeFile(dst *Config, src *Config) {
	if src.Listen != "" {
		dst.Listen = src.Listen
	}
	if src.AuthDir != "" {
		dst.AuthDir = src.AuthDir
	}
	if src.DataDir != "" {
		dst.DataDir = src.DataDir
	}
	if src.UsageDir != "" {
		dst.UsageDir = src.UsageDir
	}
	if src.DefaultModel != "" {
		dst.DefaultModel = src.DefaultModel
	}
	if src.UpstreamTimeoutSeconds > 0 {
		dst.UpstreamTimeoutSeconds = src.UpstreamTimeoutSeconds
	}
	if len(src.FreeModels) > 0 {
		dst.FreeModels = src.FreeModels
	}
	if src.LogCapacity > 0 {
		dst.LogCapacity = src.LogCapacity
	}
	dst.Limits = src.Limits
	dst.LocalWebTools = src.LocalWebTools
	dst.RequireKey = src.RequireKey

	// The scheduler block is taken whole when the file mentions it, because a
	// partial overlay would make "set only the check-in hours" silently reset
	// the others to their defaults.
	dst.Scheduler = src.Scheduler
}

func applyEnv(cfg *Config) {
	// Host-app supplied paths. These take precedence over the file because the
	// app's writable directory is not predictable from inside the file.
	setStr(&cfg.Listen, "TW2H_LISTEN")
	setStr(&cfg.AuthDir, "TW2H_AUTH_DIR")
	setStr(&cfg.DataDir, "TW2H_DATA_DIR")
	setStr(&cfg.UsageDir, "TW2H_USAGE_DIR")
	setStr(&cfg.DefaultModel, "TW2H_DEFAULT_MODEL")
	setStr(&cfg.LegacyAPIKey, "TW2H_API_KEY")

	setBool(&cfg.RequireKey, "TW2H_REQUIRE_KEY")
	setBool(&cfg.LocalWebTools, "TW2H_LOCAL_WEB_TOOLS")
	setBool(&cfg.Scheduler.Enabled, "TW2H_SCHEDULER")

	if v := os.Getenv("TW2H_LOG_CAPACITY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.LogCapacity = n
		}
	}

	// Hour lists are comma-separated: TW2H_CHECKIN_HOURS=9,21
	cfg.Scheduler.CheckinHours = parseHours(os.Getenv("TW2H_CHECKIN_HOURS"),
		cfg.Scheduler.CheckinHours)
	cfg.Scheduler.TravelHours = parseHours(os.Getenv("TW2H_TRAVEL_HOURS"),
		cfg.Scheduler.TravelHours)
	cfg.Scheduler.KeepaliveHours = parseHours(os.Getenv("TW2H_KEEPALIVE_HOURS"),
		cfg.Scheduler.KeepaliveHours)
	cfg.Scheduler.CatHours = parseHours(os.Getenv("TW2H_CAT_HOURS"),
		cfg.Scheduler.CatHours)

	if v := os.Getenv("TW2H_UPSTREAM_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.UpstreamTimeoutSeconds = n
		}
	}
	setInt64(&cfg.Limits.DailyCreditLimit, "TW2H_DAILY_CREDIT_LIMIT")
	setInt64(&cfg.Limits.DailyTokenLimit, "TW2H_DAILY_TOKEN_LIMIT")
	setInt64(&cfg.Limits.ModelDailyTokenLimit, "TW2H_MODEL_DAILY_TOKEN_LIMIT")
	setInt64(&cfg.Limits.ReserveCredits, "TW2H_RESERVE_CREDITS")

	if v := os.Getenv("TW2H_FREE_MODELS"); v != "" {
		var out []string
		for _, part := range strings.Split(v, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
		cfg.FreeModels = out
	}
}

// parseHours reads a comma-separated hour list, keeping the previous value when
// the variable is unset or contains nothing usable.
//
// Out-of-range entries are dropped rather than rejected: an hour of 25 would
// never match a wall clock, so accepting it would disable a job while the panel
// still showed it as configured.
func parseHours(raw string, fallback []int) []int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	out := make([]int, 0, 4)
	for _, part := range strings.Split(raw, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 0 || n > 23 {
			continue
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return fallback
	}
	sort.Ints(out)
	return out
}

func setStr(dst *string, key string) {
	if v := os.Getenv(key); v != "" {
		*dst = v
	}
}

func setBool(dst *bool, key string) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		*dst = true
	case "0", "false", "no", "off":
		*dst = false
	}
}

func setInt64(dst *int64, key string) {
	v := os.Getenv(key)
	if v == "" {
		return
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		*dst = n
	}
}

// normalise clamps values into a usable range and fixes derived settings.
func normalise(cfg *Config) {
	if cfg.UpstreamTimeoutSeconds <= 0 {
		cfg.UpstreamTimeoutSeconds = 120
	}
	if cfg.LogCapacity <= 0 {
		cfg.LogCapacity = 500
	}
	if cfg.LogCapacity > 20000 {
		cfg.LogCapacity = 20000
	}
	for _, n := range []*int64{
		&cfg.Limits.DailyCreditLimit,
		&cfg.Limits.DailyTokenLimit,
		&cfg.Limits.ModelDailyTokenLimit,
		&cfg.Limits.ReserveCredits,
	} {
		if *n < 0 {
			*n = 0
		}
	}

	// A listener that is not loopback-only means other hosts can reach the
	// gateway, so authentication can no longer be optional.
	if !isLoopbackListen(cfg.Listen) {
		cfg.RequireKey = true
	}

	// Keep the realm table honest even though this build only routes what the
	// account files declare.
	_ = config.RealmIntl
}

func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return false // ":7863" binds every interface
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
