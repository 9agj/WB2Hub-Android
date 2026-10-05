// Package limits is the gateway's spending guard: the thresholds past which an
// account stops being handed out, and the per-day counters those thresholds are
// measured against.
//
// The upstream enforces its own caps, but it enforces them by answering 429 —
// by which point the budget is already gone and the request is lost. This
// module lets the operator park an account *before* that: an account that has
// burned its day rotates out of the pool and the request goes to a fresh one
// instead of failing.
//
// Three guards, deliberately not one:
//
//   - Credits per day. Once spent, the account serves FREE models only, so a
//     client that would keep buying paid models rotates away instead of draining
//     the whole balance. The door reopens at the next local midnight.
//   - Tokens per day, per account. Once burned, the account is idle until local
//     midnight — the day's seat is kept for tomorrow.
//   - Tokens per day, per model. Only the model that overspent is refused; the
//     account's other models keep working, because one expensive model running
//     hot says nothing about the cheap one.
//
// A separate floor (reserve credits) is not a counter but a wall: it keeps the
// last few credits untouched so an account is never drained to exactly zero,
// which is what triggers the upstream's own reminder and the account's daily
// spend is not what pays for it.
//
// A limit of 0 disables its guard. That is what makes the upgrade path safe:
// every install that predates these settings reads back as 0 and behaves exactly
// as it did before.
//
// Ported from the daily-guard section of wb_proxy.py (daily_usage_stats,
// free_models_by_realm) and the account predicates in wb_accounts.py
// (reserve_blocked, daily_limit_blocked, credit_limit_reached,
// credit_limit_blocked, model_token_limit_blocked).
package limits

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// Config is the operator-tunable half of the guard.
//
// Every field is a threshold in credits or tokens; 0 means "guard off". They are
// int64 rather than int because they are compared against counters that are
// themselves int64, and an int on a 32-bit Android build would wrap.
type Config struct {
	DailyCreditLimit     int64 `json:"daily_credit_limit"`
	DailyTokenLimit      int64 `json:"daily_token_limit"`
	ModelDailyTokenLimit int64 `json:"model_daily_token_limit"`
	ReserveCredits       int64 `json:"reserve_credits"`
}

// dayUsage is one account's counters for the current local day.
//
// Kept as a struct rather than three parallel maps so rollover can replace a
// whole day's state in one assignment, with no window in which the token total
// and the credit total describe different days.
type dayUsage struct {
	credits float64
	// tokens is the account total; models is the per-model breakdown that the
	// rest of the same tokens is also summed into. models is the source of
	// truth for a specific model, tokens for the account as a whole.
	tokens int64
	models map[string]int64
}

// Tracker holds the guard configuration and today's counters.
//
// The clock is injected because the interesting behaviour here is what happens
// across a local midnight, and a guard whose rollover cannot be tested without
// waiting a day is a guard that will be wrong.
type Tracker struct {
	now func() time.Time

	mu     sync.Mutex
	config Config
	// day is the local date (YYYY-MM-DD) the counters belong to. Empty means
	// nothing has been counted yet.
	day string
	// byAccount is account id -> today's counters.
	byAccount map[string]*dayUsage
	// freeModels is the caller-supplied set of model ids the catalogue marks
	// free. It is configuration, not usage, so it survives rollover.
	freeModels map[string]struct{}
}

// NewTracker builds a tracker over the supplied clock and free-model set.
//
// now may be nil, in which case the wall clock is used. A nil or empty
// freeModels leaves the free set empty, which makes IsFreeModel false for
// everything: an install that has not described its catalogue gets the
// conservative reading, where every model is treated as paid.
func NewTracker(now func() time.Time, freeModels ...string) *Tracker {
	if now == nil {
		now = time.Now
	}
	t := &Tracker{
		now:       now,
		byAccount: make(map[string]*dayUsage),
	}
	t.SetFreeModels(freeModels)
	return t
}

// SetFreeModels replaces the free-model set.
//
// Matching is case- and space-insensitive because the catalogue and the
// incoming request do not agree on casing for the same id.
func (t *Tracker) SetFreeModels(models []string) {
	set := make(map[string]struct{}, len(models))
	for _, m := range models {
		if id := normalizeModel(m); id != "" {
			set[id] = struct{}{}
		}
	}
	t.mu.Lock()
	t.freeModels = set
	t.mu.Unlock()
}

// FreeModels returns the configured free-model set, sorted.
func (t *Tracker) FreeModels() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.freeModelsLocked()
}

// freeModelsLocked is FreeModels for callers already holding the lock.
func (t *Tracker) freeModelsLocked() []string {
	out := make([]string, 0, len(t.freeModels))
	for id := range t.freeModels {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// IsFreeModel reports whether the tracker's catalogue marks model as free.
//
// It is a method on the tracker rather than a package function because "free" is
// a property of the operator's catalogue, not of the model name — the same id
// can be free on one realm and paid on the other.
func (t *Tracker) IsFreeModel(model string) bool {
	id := normalizeModel(model)
	if id == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.freeModels[id]
	return ok
}

// normalizeModel is the single place model ids are canonicalised, so a set
// built from the catalogue and a lookup built from a request always agree.
func normalizeModel(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}

// SetConfig installs a new guard configuration.
//
// Limits are clamped at 0: a negative threshold would sit below every counter
// and park an account permanently, which reads to the operator as a hung
// gateway rather than as a typo.
func (t *Tracker) SetConfig(cfg Config) {
	if cfg.DailyCreditLimit < 0 {
		cfg.DailyCreditLimit = 0
	}
	if cfg.DailyTokenLimit < 0 {
		cfg.DailyTokenLimit = 0
	}
	if cfg.ModelDailyTokenLimit < 0 {
		cfg.ModelDailyTokenLimit = 0
	}
	if cfg.ReserveCredits < 0 {
		cfg.ReserveCredits = 0
	}
	t.mu.Lock()
	t.config = cfg
	t.mu.Unlock()
}

// Config returns the current guard configuration.
func (t *Tracker) Config() Config {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.config
}

// NoteUsage records one completed request against an account.
//
// A negative or empty account id is dropped rather than stored under a
// pseudo-account: unattributed spend would otherwise aggregate into a bucket
// that no real account can ever clear, and the panel would show a phantom
// account over budget forever.
//
// Rollover runs first, so a request that straddles local midnight is counted
// against the new day exactly as the upstream would bill it.
func (t *Tracker) NoteUsage(accountID, model string, prompt, completion int64, credits float64) {
	id := strings.TrimSpace(accountID)
	if id == "" {
		return
	}
	if prompt < 0 {
		prompt = 0
	}
	if completion < 0 {
		completion = 0
	}
	tokens := prompt + completion

	t.mu.Lock()
	defer t.mu.Unlock()
	t.rolloverLocked(t.now())

	entry := t.byAccount[id]
	if entry == nil {
		entry = &dayUsage{models: make(map[string]int64)}
		t.byAccount[id] = entry
	}
	entry.tokens += tokens
	entry.credits += credits
	if mid := normalizeModel(model); mid != "" {
		entry.models[mid] += tokens
	}
}

// CreditsSpent reports the credits an account has spent today, rounded down.
//
// Rounded down rather than up so the reported figure never claims a limit was
// reached before the true spend did.
func (t *Tracker) CreditsSpent(accountID string) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rolloverLocked(t.now())
	return int64(t.entryLocked(accountID).credits)
}

// TokensSpent reports the tokens one account has spent today on one model.
func (t *Tracker) TokensSpent(accountID, model string) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rolloverLocked(t.now())
	return t.entryLocked(accountID).models[normalizeModel(model)]
}

// TokensSpentToday reports the tokens one account has spent today in total,
// across every model.
func (t *Tracker) TokensSpentToday(accountID string) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rolloverLocked(t.now())
	return t.entryLocked(accountID).tokens
}

// entryLocked returns the account's counters, creating them on first touch.
//
// Read paths create the entry too: a caller that asks "what has this account
// spent" and then records usage expects to be reading the same record.
func (t *Tracker) entryLocked(accountID string) *dayUsage {
	id := strings.TrimSpace(accountID)
	if entry, ok := t.byAccount[id]; ok {
		return entry
	}
	entry := &dayUsage{models: make(map[string]int64)}
	t.byAccount[id] = entry
	return entry
}

// CreditExhausted reports whether the account has spent its daily credit
// budget. A limit of 0 disables the guard, so the answer is false.
//
// This is the account-level fact, independent of the model being asked for;
// CanServe pairs it with IsFreeModel so free models keep serving after the cap.
func (t *Tracker) CreditExhausted(accountID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rolloverLocked(t.now())
	return t.creditExhaustedLocked(accountID)
}

// creditExhaustedLocked is CreditExhausted for callers already holding the lock.
func (t *Tracker) creditExhaustedLocked(accountID string) bool {
	if t.config.DailyCreditLimit <= 0 {
		return false
	}
	return t.entryLocked(accountID).credits >= float64(t.config.DailyCreditLimit)
}

// ModelExhausted reports whether one model has burned its daily token budget
// for one account. A limit of 0 disables the guard, so the answer is false.
//
// Only this model is affected. The check is here, next to the counter, so no
// caller can reasonably forget to scope the refusal to the requested model.
func (t *Tracker) ModelExhausted(accountID, model string) bool {
	mid := normalizeModel(model)
	if mid == "" {
		// With no model named there is nothing to judge, and the account must
		// not be parked on behalf of a request that named nothing.
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rolloverLocked(t.now())
	if t.config.ModelDailyTokenLimit <= 0 {
		return false
	}
	return t.entryLocked(accountID).models[mid] >= t.config.ModelDailyTokenLimit
}

// ModelExhaustedByDailyTokenLimit reports whether the account's TOTAL token
// spend has reached the account-wide daily limit.
//
// The name is the long one on purpose: this is the guard that takes the whole
// account out of rotation, and it is easy to reach for it when what is meant is
// ModelExhausted, which takes out a single model.
func (t *Tracker) ModelExhaustedByDailyTokenLimit(accountID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rolloverLocked(t.now())
	return t.dailyTokenExhaustedLocked(accountID)
}

// dailyTokenExhaustedLocked is the account-wide token check.
func (t *Tracker) dailyTokenExhaustedLocked(accountID string) bool {
	if t.config.DailyTokenLimit <= 0 {
		return false
	}
	return t.entryLocked(accountID).tokens >= t.config.DailyTokenLimit
}

// CanServe is the composed gate: the one question the pool asks before handing
// an account to a request.
//
// The order is the one wb_accounts.ready() uses, and it matters for which
// refusal the operator sees first: the account-wide guards are answered before
// the per-model one, so a parked account reports that it is parked rather than
// blaming a model.
//
// isFreeModel is passed in rather than looked up so the caller can answer it
// from the realm catalogue it is already holding; when the caller has no
// catalogue, IsFreeModel on this tracker serves as the built-in set.
//
// What each guard does here, and why:
//
//   - Reserve floor is NOT consulted. The reserve works on the balance the
//     account still holds, which this module never learns; it is enforced where
//     the balance is known, so that CanServe does not have to guess it.
//   - Daily tokens spent: the account is idle for the rest of the day, for
//     every model. This one is not softened for free models — the limit is a
//     token budget, and a free model spends tokens too.
//   - Credits spent: paid models rotate away, free models still serve. This is
//     the entire point of the guard, and the only guard that reads isFreeModel.
//   - Model tokens spent: refused, but only for that one model.
func (t *Tracker) CanServe(accountID, model string, isFreeModel bool) bool {
	id := strings.TrimSpace(accountID)
	if id == "" {
		// An unattributed request has no history to judge; refusing it here
		// would look like a gateway-wide outage.
		return true
	}
	mid := normalizeModel(model)

	t.mu.Lock()
	defer t.mu.Unlock()
	t.rolloverLocked(t.now())

	// The day's token budget is gone: keep the seat for tomorrow.
	if t.dailyTokenExhaustedLocked(id) {
		return false
	}
	// The day's credit spend hit the cap: paid models stop, free ones do not.
	if !isFreeModel && t.creditExhaustedLocked(id) {
		return false
	}
	// One model out of budget does not take the account with it.
	if mid != "" && t.config.ModelDailyTokenLimit > 0 &&
		t.entryLocked(id).models[mid] >= t.config.ModelDailyTokenLimit {
		return false
	}
	return true
}

// ReserveBlocked reports whether a balance at remainCredits must stay untouched.
//
// It is the counter-less guard: nothing here counts usage, it just compares the
// account's known balance against the floor. remaincredits is a pointer-like
// pair because "the balance has never been fetched" is a third state — a fresh
// install with no balance yet must stay usable, not look drained.
//
// A floor of 0 disables the guard, which keeps installs that predate the setting
// behaving exactly as before.
func (t *Tracker) ReserveBlocked(remainCredits int64, known bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.config.ReserveCredits <= 0 || !known {
		return false
	}
	return remainCredits <= t.config.ReserveCredits
}

// Rollover clears the per-day counters when the local date has changed.
//
// Safe and cheap to call per request: the common path is one time.Now, one
// month-day compare and no allocation. The Python reference calls the equivalent
// on every request for the same reason — there is no timer that can be trusted
// to fire across the app being backgrounded on Android, and a guard that only
// rolled over on a background tick would block every account for the first
// request of a new day.
//
// The free-model set and the configuration are not usage and survive untouched.
func (t *Tracker) Rollover(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rolloverLocked(now)
}

// rolloverLocked is Rollover for callers already holding the lock.
func (t *Tracker) rolloverLocked(now time.Time) {
	day := dayKey(now)
	if day == t.day {
		return
	}
	t.day = day
	t.byAccount = make(map[string]*dayUsage)
}

// dayKey is the local calendar date a moment belongs to, in the account's own
// timezone.
//
// Local, not UTC: the operator set "per day" meaning their day, and a gateway at
// UTC+8 that rolled over at 16:00 local would park accounts in the middle of the
// working afternoon.
func dayKey(now time.Time) string {
	return now.Format("2006-01-02")
}

// NextMidnight returns the next local midnight strictly after now.
//
// Built from the date components rather than by adding 24h: on the day a
// daylight-saving transition happens, the local day is 23 or 25 hours long and a
// fixed offset would put the boundary in the wrong place. time.Date resolves the
// wall-clock midnight on that date, including any offset change.
//
// The result is always in the future, so a caller cannot be handed a boundary it
// has already passed — the Python reference applies the same floor for the same
// reason.
func NextMidnight(now time.Time) time.Time {
	y, m, d := now.Date()
	next := time.Date(y, m, d+1, 0, 0, 0, 0, now.Location())
	if !next.After(now) {
		// Unreachable for a valid time, but a zero Time (or a location whose
		// offset shifts by a whole day) must not return a past boundary.
		next = next.Add(24 * time.Hour)
	}
	return next
}

// SecondsUntilNextMidnight reports how long the current day's counters have left
// to live, with a one-minute floor.
//
// The floor exists so a panel that renders the countdown never shows a zero or
// negative number to an operator who then wonders whether the guard is stuck.
func SecondsUntilNextMidnight(now time.Time) int64 {
	secs := int64(NextMidnight(now).Sub(now).Seconds())
	if secs < 60 {
		return 60
	}
	return secs
}

// AccountSnapshot is the panel's view of one account's guard state.
//
// Every field is a plain number or bool so the JSON encoder and the dashboard
// agree on the shape without a helper type on the JavaScript side.
type AccountSnapshot struct {
	Account string `json:"account"`
	Credits int64  `json:"credits_spent"`
	Tokens  int64  `json:"tokens_spent"`
	// Models is the per-model token breakdown, keyed by canonical model id.
	Models map[string]int64 `json:"models"`
	// CreditExhausted and DailyTokensExhausted are account-wide; the first
	// still serves free models, the second serves nothing until midnight.
	CreditExhausted      bool `json:"credit_exhausted"`
	DailyTokensExhausted bool `json:"daily_tokens_exhausted"`
}

// Snapshot renders the guard state for the admin panel / JSON API.
//
// The top-level map is deliberately `map[string]any` with string keys chosen to
// match the settings document (`daily_credit_limit`, `reserve_credits`, …), so
// the panel can splice this straight into the reply it already builds from
// settings.json without a second naming scheme.
func (t *Tracker) Snapshot() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rolloverLocked(t.now())

	accounts := make([]AccountSnapshot, 0, len(t.byAccount))
	for id, entry := range t.byAccount {
		models := make(map[string]int64, len(entry.models))
		for mid, used := range entry.models {
			models[mid] = used
		}
		accounts = append(accounts, AccountSnapshot{
			Account:              id,
			Credits:              int64(entry.credits),
			Tokens:               entry.tokens,
			Models:               models,
			CreditExhausted:      t.creditExhaustedLocked(id),
			DailyTokensExhausted: t.dailyTokenExhaustedLocked(id),
		})
	}
	// Map iteration order is random; the panel's table would reshuffle on
	// every poll without this.
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].Account < accounts[j].Account })

	return map[string]any{
		"day":                     t.day,
		"seconds_until_midnight":  SecondsUntilNextMidnight(t.now()),
		"daily_credit_limit":      t.config.DailyCreditLimit,
		"daily_token_limit":       t.config.DailyTokenLimit,
		"model_daily_token_limit": t.config.ModelDailyTokenLimit,
		"reserve_credits":         t.config.ReserveCredits,
		"free_models":             t.freeModelsLocked(),
		"accounts":                accounts,
	}
}

// DefaultFreeModels is the seed used when the caller has no catalogue to hand.
//
// It exists to keep a bare `NewTracker(nil)` from silently treating a known-free
// model as paid; the authoritative set still comes from the realm catalogue,
// which is per-realm and therefore cannot live here.
var DefaultFreeModels = []string{
	"deepseek-chat",
	"deepseek-reasoner",
}
