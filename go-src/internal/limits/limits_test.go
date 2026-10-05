package limits

import (
	"encoding/json"
	"sort"
	"sync"
	"testing"
	"time"
)

// fixedClock is a hand-driven clock, so the rollover boundary is a value the
// test chooses rather than something the suite has to wait for.
type fixedClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock(t time.Time) *fixedClock { return &fixedClock{now: t} }

func (c *fixedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fixedClock) Set(t time.Time) {
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
}

// at builds a local wall-clock time on the given day of a fixed month.
func at(day, hour, min int) time.Time {
	return time.Date(2026, time.March, day, hour, min, 0, 0, time.Local)
}

func TestCreditExhaustedTriggersAtTheLimitAndNotBefore(t *testing.T) {
	clock := newClock(at(10, 9, 0))
	tr := NewTracker(clock.Now)
	tr.SetConfig(Config{DailyCreditLimit: 100})

	if tr.CreditExhausted("acct") {
		t.Fatal("a fresh account already looks exhausted")
	}
	tr.NoteUsage("acct", "paid-model", 1, 1, 99)
	if tr.CreditExhausted("acct") {
		t.Fatal("99 of 100 credits reported exhausted")
	}
	tr.NoteUsage("acct", "paid-model", 1, 1, 1)
	if !tr.CreditExhausted("acct") {
		t.Fatal("exactly at the limit must be exhausted")
	}
	if got := tr.CreditsSpent("acct"); got != 100 {
		t.Fatalf("CreditsSpent = %d, want 100", got)
	}
}

func TestZeroLimitDisablesEveryGuard(t *testing.T) {
	clock := newClock(at(10, 9, 0))
	tr := NewTracker(clock.Now)
	tr.SetConfig(Config{}) // every limit 0: the pre-upgrade behaviour

	tr.NoteUsage("acct", "paid-model", 100_000, 100_000, 10_000)

	if tr.CreditExhausted("acct") {
		t.Fatal("credit guard fired with a 0 limit")
	}
	if tr.ModelExhausted("acct", "paid-model") {
		t.Fatal("model guard fired with a 0 limit")
	}
	if tr.ModelExhaustedByDailyTokenLimit("acct") {
		t.Fatal("daily token guard fired with a 0 limit")
	}
	if !tr.CanServe("acct", "paid-model", false) {
		t.Fatal("CanServe refused with every guard disabled")
	}
}

func TestDailyCreditLimitServesFreeModelsOnly(t *testing.T) {
	clock := newClock(at(10, 9, 0))
	tr := NewTracker(clock.Now, "free-model")
	tr.SetConfig(Config{DailyCreditLimit: 50})

	tr.NoteUsage("acct", "paid-model", 10, 10, 50)

	if !tr.CreditExhausted("acct") {
		t.Fatal("the account should be at its credit cap")
	}
	// The entire point of the guard: the cap rotates paid traffic away, not
	// the whole account.
	if tr.CanServe("acct", "paid-model", false) {
		t.Fatal("a paid model was served after the credit cap")
	}
	if !tr.CanServe("acct", "free-model", true) {
		t.Fatal("a free model was refused after the credit cap")
	}
	if !tr.IsFreeModel("free-model") {
		t.Fatal("IsFreeModel does not know the configured free model")
	}
	if tr.IsFreeModel("paid-model") {
		t.Fatal("IsFreeModel claims a paid model is free")
	}
}

func TestModelLimitBlocksOnlyTheOffendingModel(t *testing.T) {
	clock := newClock(at(10, 9, 0))
	tr := NewTracker(clock.Now)
	tr.SetConfig(Config{ModelDailyTokenLimit: 1000})

	tr.NoteUsage("acct", "hot-model", 600, 400, 1) // hits 1000 exactly
	tr.NoteUsage("acct", "cool-model", 10, 10, 1)

	if !tr.ModelExhausted("acct", "hot-model") {
		t.Fatal("the model at its budget is not reported exhausted")
	}
	if tr.ModelExhausted("acct", "cool-model") {
		t.Fatal("an untouched model was reported exhausted")
	}
	if tr.CanServe("acct", "hot-model", false) {
		t.Fatal("the overspent model was still served")
	}
	if !tr.CanServe("acct", "cool-model", false) {
		t.Fatal("one model's budget took the account's other models with it")
	}

	// Same account, other model: the counters must be independent.
	if got := tr.TokensSpent("acct", "hot-model"); got != 1000 {
		t.Fatalf("hot-model tokens = %d, want 1000", got)
	}
	if got := tr.TokensSpent("acct", "cool-model"); got != 20 {
		t.Fatalf("cool-model tokens = %d, want 20", got)
	}
	if got := tr.TokensSpentToday("acct"); got != 1020 {
		t.Fatalf("account total = %d, want 1020", got)
	}
}

func TestAccountDailyTokenLimitParksEverything(t *testing.T) {
	clock := newClock(at(10, 9, 0))
	tr := NewTracker(clock.Now, "free-model")

	// With the guard disabled the same spend changes nothing.
	tr.SetConfig(Config{})
	tr.NoteUsage("acct", "a", 250, 250, 0)
	if tr.ModelExhaustedByDailyTokenLimit("acct") {
		t.Fatal("the daily token guard fired while it was disabled")
	}
	if !tr.CanServe("acct", "a", false) {
		t.Fatal("the account was parked while the guard was disabled")
	}
	if got := tr.TokensSpentToday("acct"); got != 500 {
		t.Fatalf("account total = %d, want 500", got)
	}

	// Turning the guard on must apply to spend that already happened today.
	tr.SetConfig(Config{DailyTokenLimit: 500, ModelDailyTokenLimit: 10_000})

	if !tr.ModelExhaustedByDailyTokenLimit("acct") {
		t.Fatal("the account total reached the daily token limit")
	}
	// A token budget is spent by free models too, so this guard is not
	// softened the way the credit one is.
	if tr.CanServe("acct", "free-model", true) {
		t.Fatal("a free model was served after the daily token cap")
	}
	if tr.CanServe("acct", "other", false) {
		t.Fatal("a paid model was served after the daily token cap")
	}
}

func TestNoModelNamedIsNeverParkedByTheModelGuard(t *testing.T) {
	clock := newClock(at(10, 9, 0))
	tr := NewTracker(clock.Now)
	tr.SetConfig(Config{ModelDailyTokenLimit: 5})
	tr.NoteUsage("acct", "", 1000, 0, 0)

	if tr.ModelExhausted("acct", "") {
		t.Fatal("an unnamed model was parked")
	}
	if !tr.CanServe("acct", "", false) {
		t.Fatal("an account was parked on behalf of a request naming no model")
	}
	if got := tr.TokensSpent("acct", ""); got != 0 {
		t.Fatalf("tokens were attributed to the empty model id: %d", got)
	}
}

func TestRolloverClearsCountersAtLocalMidnight(t *testing.T) {
	clock := newClock(at(10, 23, 59))
	tr := NewTracker(clock.Now, "free-model")
	tr.SetConfig(Config{DailyCreditLimit: 10, DailyTokenLimit: 100, ModelDailyTokenLimit: 100})

	tr.NoteUsage("acct", "hot-model", 60, 60, 10)
	if !tr.CreditExhausted("acct") || !tr.ModelExhausted("acct", "hot-model") {
		t.Fatal("the guards should be tripped at the end of the day")
	}

	// One minute before midnight the day has not rolled over.
	tr.Rollover(at(10, 23, 59))
	if tr.CreditsSpent("acct") != 10 {
		t.Fatal("the counters were cleared before midnight")
	}

	// Rollover must also happen on the read path, since nothing ticks a timer
	// reliably on Android.
	clock.Set(at(11, 0, 0))
	if tr.CreditsSpent("acct") != 0 {
		t.Fatal("CreditsSpent did not roll over")
	}
	if tr.TokensSpentToday("acct") != 0 || tr.TokensSpent("acct", "hot-model") != 0 {
		t.Fatal("token counters did not roll over")
	}
	if tr.CreditExhausted("acct") || tr.ModelExhausted("acct", "hot-model") {
		t.Fatal("the guards are still tripped on the new day")
	}
	if !tr.CanServe("acct", "hot-model", false) {
		t.Fatal("the account did not come back into rotation")
	}

	// A rollover call that lands on the same day must not clear anything.
	tr.NoteUsage("acct", "hot-model", 1, 1, 1)
	tr.Rollover(at(11, 8, 0))
	tr.Rollover(at(11, 8, 0))
	if got := tr.CreditsSpent("acct"); got != 1 {
		t.Fatalf("a same-day rollover cleared the counters: credits = %d", got)
	}

	// The configuration and the free-model set are not usage and survive.
	if cfg := tr.Config(); cfg.DailyCreditLimit != 10 || cfg.DailyTokenLimit != 100 {
		t.Fatalf("rollover changed the config: %+v", cfg)
	}
	if !tr.IsFreeModel("free-model") {
		t.Fatal("rollover dropped the free-model set")
	}
}

func TestNextMidnightIsStrictlyInTheFuture(t *testing.T) {
	cases := []struct {
		now           time.Time
		wantDate      int
		wantRemaining time.Duration
	}{
		{at(10, 0, 0), 11, 24 * time.Hour},
		{at(10, 12, 0), 11, 12 * time.Hour},
		{at(10, 23, 59), 11, time.Minute},
	}
	for _, tc := range cases {
		got := NextMidnight(tc.now)
		if !got.After(tc.now) {
			t.Fatalf("NextMidnight(%v) = %v, which is not in the future", tc.now, got)
		}
		if got.Day() != tc.wantDate || got.Hour() != 0 || got.Minute() != 0 {
			t.Fatalf("NextMidnight(%v) = %v, want midnight on the %dth", tc.now, got, tc.wantDate)
		}
		if d := got.Sub(tc.now); d != tc.wantRemaining {
			t.Fatalf("NextMidnight(%v) is %v away, want %v", tc.now, d, tc.wantRemaining)
		}
	}

	// The month boundary is crossed by the date arithmetic, not by an offset.
	eom := time.Date(2026, time.March, 31, 23, 0, 0, 0, time.Local)
	if got := NextMidnight(eom); got.Month() != time.April || got.Day() != 1 {
		t.Fatalf("NextMidnight(%v) = %v, want 1 April", eom, got)
	}

	if secs := SecondsUntilNextMidnight(at(10, 12, 0)); secs != 12*3600 {
		t.Fatalf("SecondsUntilNextMidnight = %d, want %d", secs, 12*3600)
	}
	// The one-minute floor keeps the panel from ever rendering a zero.
	if secs := SecondsUntilNextMidnight(at(10, 23, 59)); secs != 60 {
		t.Fatalf("SecondsUntilNextMidnight = %d, want the 60s floor", secs)
	}
}

func TestReserveBlockedNeedsAKnownBalance(t *testing.T) {
	tr := NewTracker(newClock(at(10, 9, 0)).Now)
	tr.SetConfig(Config{ReserveCredits: 50})

	if !tr.ReserveBlocked(10, true) {
		t.Fatal("a balance below the reserve should be blocked")
	}
	if !tr.ReserveBlocked(50, true) {
		t.Fatal("a balance exactly at the reserve should be blocked")
	}
	if tr.ReserveBlocked(51, true) {
		t.Fatal("a balance above the reserve should serve")
	}
	// Never fetched is not the same as zero: a fresh install must stay usable.
	if tr.ReserveBlocked(0, false) {
		t.Fatal("an unknown balance was treated as drained")
	}

	tr.SetConfig(Config{ReserveCredits: 0})
	if tr.ReserveBlocked(0, true) {
		t.Fatal("a 0 reserve should disable the guard")
	}
}

func TestConfigIsClampedAndCopied(t *testing.T) {
	tr := NewTracker(nil)
	tr.SetConfig(Config{DailyCreditLimit: -5, DailyTokenLimit: -1, ModelDailyTokenLimit: -3, ReserveCredits: -9})

	cfg := tr.Config()
	if cfg.DailyCreditLimit != 0 || cfg.DailyTokenLimit != 0 ||
		cfg.ModelDailyTokenLimit != 0 || cfg.ReserveCredits != 0 {
		t.Fatalf("negative limits were not clamped: %+v", cfg)
	}

	tr.SetConfig(Config{DailyCreditLimit: 7})
	returned := tr.Config()
	returned.DailyCreditLimit = 9999
	if tr.Config().DailyCreditLimit != 7 {
		t.Fatal("Config handed out a live reference to its own state")
	}
}

func TestCanServeIsPerAccount(t *testing.T) {
	clock := newClock(at(10, 9, 0))
	tr := NewTracker(clock.Now)
	tr.SetConfig(Config{DailyCreditLimit: 10, ModelDailyTokenLimit: 100})

	tr.NoteUsage("drained", "m", 0, 0, 10)
	tr.NoteUsage("fresh", "m", 1, 1, 0)

	if tr.CanServe("drained", "m", false) {
		t.Fatal("the drained account was still served")
	}
	if !tr.CanServe("fresh", "m", false) {
		t.Fatal("a second account was parked because the first one spent its budget")
	}
	if tr.CreditsSpent("fresh") != 0 {
		t.Fatalf("counter leaked between accounts: %d", tr.CreditsSpent("fresh"))
	}
	// An unknown account has no history and must not be parked.
	if !tr.CanServe("never-seen", "m", false) {
		t.Fatal("an account with no history was refused")
	}
	// An empty account id has no history to judge either.
	if !tr.CanServe("   ", "m", false) {
		t.Fatal("an unattributed request was refused")
	}
}

func TestSnapshotShapeAndStability(t *testing.T) {
	clock := newClock(at(10, 9, 0))
	tr := NewTracker(clock.Now, "free-model")
	tr.SetConfig(Config{DailyCreditLimit: 10, DailyTokenLimit: 100, ModelDailyTokenLimit: 100, ReserveCredits: 5})

	tr.NoteUsage("b-acct", "m", 1, 1, 10)
	tr.NoteUsage("a-acct", "m", 1, 1, 0)

	snap := tr.Snapshot()
	for _, field := range []string{"day", "seconds_until_midnight", "daily_credit_limit",
		"daily_token_limit", "model_daily_token_limit", "reserve_credits", "free_models", "accounts"} {
		if _, ok := snap[field]; !ok {
			t.Errorf("snapshot is missing %q", field)
		}
	}
	// The keys are the settings document's names, so the panel can splice the
	// snapshot into its reply without translating field names.
	if _, err := json.Marshal(snap); err != nil {
		t.Fatalf("snapshot does not encode: %v", err)
	}

	accounts, ok := snap["accounts"].([]AccountSnapshot)
	if !ok {
		t.Fatalf("accounts has type %T, want []AccountSnapshot", snap["accounts"])
	}
	if len(accounts) != 2 {
		t.Fatalf("snapshot holds %d accounts, want 2", len(accounts))
	}
	// Sorted, so the panel's table does not reshuffle on every poll.
	names := []string{accounts[0].Account, accounts[1].Account}
	if !sort.StringsAreSorted(names) {
		t.Fatalf("accounts are not sorted: %v", names)
	}
	if !accounts[1].CreditExhausted || accounts[0].CreditExhausted {
		t.Fatalf("credit_exhausted is wrong: %+v", accounts)
	}
	if got := accounts[1].Models["m"]; got != 2 {
		t.Fatalf("per-model breakdown = %d, want 2", got)
	}

	// The snapshot must not hand back the tracker's own maps.
	accounts[1].Models["m"] = 9999
	if again := tr.Snapshot()["accounts"].([]AccountSnapshot); again[1].Models["m"] != 2 {
		t.Fatal("the snapshot aliased the tracker's internal map")
	}
}

func TestRolloverIsSafeUnderConcurrency(t *testing.T) {
	clock := newClock(at(10, 23, 59))
	tr := NewTracker(clock.Now, "free")
	tr.SetConfig(Config{DailyCreditLimit: 1000, DailyTokenLimit: 100_000, ModelDailyTokenLimit: 100_000})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				tr.NoteUsage("acct", "m", 1, 1, 0)
				tr.CanServe("acct", "m", false)
				tr.CreditsSpent("acct")
				tr.Snapshot()
				if j == 100 && i == 0 {
					// Force the boundary while the others are mid-flight.
					clock.Set(at(11, 0, 0))
				}
			}
		}(i)
	}
	wg.Wait()

	// Whatever interleaving happened, the counters must describe exactly one
	// day and be internally consistent.
	tr.Rollover(clock.Now())
	total := tr.TokensSpentToday("acct")
	if per := tr.TokensSpent("acct", "m"); per != total {
		t.Fatalf("the per-model total (%d) disagrees with the account total (%d)", per, total)
	}
	if total%2 != 0 {
		t.Fatalf("every request contributes 2 tokens, got %d", total)
	}
}

func TestFreeModelMatchingIsCanonical(t *testing.T) {
	tr := NewTracker(nil, "  Free-Model  ", "", "other/Model")

	if !tr.IsFreeModel("free-model") || !tr.IsFreeModel("  FREE-MODEL  ") {
		t.Fatal("free-model matching is not case/space insensitive")
	}
	if tr.IsFreeModel("") {
		t.Fatal("the empty model id was reported free")
	}
	if tr.IsFreeModel("paid") {
		t.Fatal("an unknown model was reported free")
	}
	if got := tr.FreeModels(); len(got) != 2 {
		t.Fatalf("FreeModels = %v, want two entries with the blank dropped", got)
	}

	// An unseeded tracker treats everything as paid: the conservative reading,
	// because an unknown model must not slip past the credit guard.
	strict := NewTracker(nil)
	if strict.IsFreeModel("deepseek-chat") {
		t.Fatal("an unseeded tracker claimed to know a free model")
	}
	seeded := NewTracker(nil, DefaultFreeModels...)
	if !seeded.IsFreeModel("deepseek-chat") {
		t.Fatal("DefaultFreeModels did not seed the free set")
	}
}
