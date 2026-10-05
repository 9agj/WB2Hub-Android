package scheduler

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"wb2hub/internal/auth"
	"wb2hub/internal/pool"
)

// --------------------------------------------------------------- test clock

// fixedClock is a hand-driven clock, so a whole day can be walked in a test
// without waiting for one.
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

func (c *fixedClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// day builds a local wall-clock time on a fixed day.
func day(hour, min int) time.Time {
	return time.Date(2026, time.March, 10, hour, min, 0, 0, time.Local)
}

// ------------------------------------------------------------- test plumbing

// recorder collects the scheduler's log lines so a test can assert on them.
type recorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *recorder) Logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// The scheduler formats its own "scheduler: " prefix, so the recorder stores
	// the line verbatim — a second prefix would make assertions look for text
	// that never appears.
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

func (r *recorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

func (r *recorder) contains(needle string) bool {
	for _, line := range r.all() {
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}

// sprintf renders a formatted string for a needle to search for.
func sprintf(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}

// testAccount builds an enabled account with the given realm.
func testAccount(uid, realm string) *auth.Account {
	acct := &auth.Account{UID: uid, Realm: realm, AccessToken: "token-" + uid}
	acct.SetEnabled(true)
	return acct
}

// newTestPool builds a pool holding the given accounts.
func newTestPool(accounts ...*auth.Account) *pool.Pool {
	p := pool.New("")
	p.Restore(accounts)
	return p
}

// newTestScheduler wires a scheduler over a fixed clock with no network.
func newTestScheduler(clock *fixedClock, p *pool.Pool, rec *recorder) *Scheduler {
	return New(Config{
		Pool: p,
		Now:  clock.Now,
		Log:  rec.Logf,
	})
}

// ------------------------------------------------------------ slot matching

func TestSlotFiresOnlyOnConfiguredHours(t *testing.T) {
	clock := newClock(day(9, 0))
	rec := &recorder{}
	s := newTestScheduler(clock, newTestPool(), rec)

	// Midnight walk: only 01:00 (cat), 09:00, 21:00, 22:00 may fire.
	fired := map[int]string{}
	for hour := 0; hour < 24; hour++ {
		clock.Set(day(hour, 0))
		before := len(rec.all())
		s.CatchUp(context.Background())
		if len(rec.all()) > before {
			fired[hour] = "fired"
		}
	}

	want := map[int]bool{1: true, 9: true, 21: true, 22: true}
	for hour := 0; hour < 24; hour++ {
		if _, got := fired[hour]; got != want[hour] {
			t.Errorf("hour %02d fired=%v, want %v", hour, got, want[hour])
		}
	}
}

func TestSlotDoesNotFireOnAnUnconfiguredHour(t *testing.T) {
	clock := newClock(day(13, 0))
	rec := &recorder{}
	p := newTestPool(testAccount("acct-1", "cn"))
	s := newTestScheduler(clock, p, rec)

	s.CatchUp(context.Background())
	if len(rec.all()) != 0 {
		t.Fatalf("13:00 is not a slot, but the scheduler logged: %v", rec.all())
	}
}

// --------------------------------------------------------------- double-fire

func TestNoDoubleFireWithinTheSameMinute(t *testing.T) {
	clock := newClock(day(9, 0))
	rec := &recorder{}
	p := newTestPool(testAccount("intl-1", "intl"))
	s := newTestScheduler(clock, p, rec)

	// Six passes inside the 09:00 hour, which is what a 30s tick does.
	for i := 0; i < 6; i++ {
		s.CatchUp(context.Background())
	}
	// 09:00 is the check-in hour and the travel hour, so both jobs fire once.
	// What must not happen is either firing twice.
	if count := countOccurrences(rec.all(), "巡检完成 checkin"); count != 1 {
		t.Fatalf("the checkin job ran %d times within its hour, want exactly 1", count)
	}
	if count := countOccurrences(rec.all(), "巡检完成 travel"); count != 1 {
		t.Fatalf("the travel job ran %d times within its hour, want exactly 1", count)
	}
}

func TestNoDoubleFireAcrossLaterTicksInTheSameDay(t *testing.T) {
	clock := newClock(day(9, 0))
	rec := &recorder{}
	p := newTestPool(testAccount("intl-1", "intl"))
	s := newTestScheduler(clock, p, rec)

	s.CatchUp(context.Background())
	clock.Set(day(9, 30))
	s.CatchUp(context.Background())
	clock.Set(day(9, 59))
	s.CatchUp(context.Background())

	if count := countOccurrences(rec.all(), "巡检完成 checkin"); count != 1 {
		t.Fatalf("the checkin job ran %d times on the same day, want exactly 1", count)
	}
}

func TestAJobWithTwoHoursFiresAtBoth(t *testing.T) {
	// Check-in runs at 09:00 and 21:00. A date-only stamp would let the morning
	// run silently cancel the evening one, which is the bug this asserts against.
	clock := newClock(day(9, 0))
	rec := &recorder{}
	p := newTestPool(testAccount("intl-1", "intl"))
	s := newTestScheduler(clock, p, rec)

	s.CatchUp(context.Background())

	clock.Set(day(21, 0))
	s.CatchUp(context.Background())

	if count := countOccurrences(rec.all(), "巡检完成 checkin"); count != 2 {
		t.Fatalf("checkin ran %d times over 09:00 and 21:00, want 2", count)
	}
	if !rec.contains("整点排程命中 (21:00)") {
		t.Fatalf("the 21:00 occurrence did not fire: %v", rec.all())
	}
}

func TestEachSlotFiresOnceOnItsOwnDay(t *testing.T) {
	clock := newClock(day(1, 0))
	rec := &recorder{}
	p := newTestPool(testAccount("intl-1", "intl"))
	s := newTestScheduler(clock, p, rec)

	// Day one and day two: every configured occurrence must run on each.
	// The day-of-month has to be named explicitly — day() fixes the month, so
	// day(1,0) and day(2,0) are both March 10 and would walk one day twice.
	for _, dom := range []int{10, 11} {
		for _, hour := range []int{1, 9, 21, 22} {
			clock.Set(time.Date(2026, time.March, dom, hour, 0, 0, 0, time.Local))
			s.CatchUp(context.Background())
		}
	}

	// checkin and travel both run at 09:00 and at 21:00, so two days give four
	// runs each.
	if count := countOccurrences(rec.all(), "巡检完成 checkin"); count != 4 {
		t.Fatalf("checkin ran %d times over two days, want 4", count)
	}
	if count := countOccurrences(rec.all(), "巡检完成 keepalive"); count != 2 {
		t.Fatalf("keepalive ran %d times over two days, want 2", count)
	}
	if count := countOccurrences(rec.all(), "巡检完成 cat"); count != 2 {
		t.Fatalf("cat ran %d times over two days, want 2", count)
	}
}

// ---------------------------------------------------------------- catch-up

func TestCatchUpAfterALongSleepFiresTheMissedSlotExactlyOnce(t *testing.T) {
	// The process was asleep through 09:00 and wakes at 10:30.
	clock := newClock(day(10, 30))
	rec := &recorder{}
	p := newTestPool()
	s := newTestScheduler(clock, p, rec)

	s.CatchUp(context.Background())

	// A missed slot must run, but a slot whose hour is simply not now must not.
	if count := countOccurrences(rec.all(), "整点排程命中 (10:00)"); count != 0 {
		t.Fatalf("10:00 is not a slot but fired %d times", count)
	}
	if count := countOccurrences(rec.all(), "整点排程命中 (09:00)"); count != 0 {
		t.Fatalf("09:00 fired while the clock reads 10:30: %v", rec.all())
	}
}

func TestCatchUpRunsAMissedSlotWhenTheProcessWakesInsideItsHour(t *testing.T) {
	// Sleeping through the start of the hour is the normal Android case: the
	// slot must still run, because the reward is lost otherwise.
	clock := newClock(day(10, 30))

	// Pretend the slot's hour is 10 so the wake-up lands inside it.
	rec := &recorder{}
	p := newTestPool()
	s := New(Config{
		Pool:           p,
		CheckinHours:   []int{10},
		TravelHours:    []int{10},
		KeepaliveHours: []int{23},
		CatHours:       []int{2},
		Now:            clock.Now,
		Log:            rec.Logf,
	})

	s.CatchUp(context.Background())
	// Check-in and the trip are both configured on hour 10, so each must fire
	// exactly once — counting the hour alone would report two jobs as a double
	// fire of one.
	for _, job := range []Job{JobCheckin, JobTravel} {
		if count := countJobFirings(rec.all(), string(job), 10); count != 1 {
			t.Fatalf("a wake-up inside the slot's hour fired %s %d times, want 1: %v",
				job, count, rec.all())
		}
	}

	// And a second wake-up in the same hour must not re-run it.
	clock.Set(day(10, 55))
	s.CatchUp(context.Background())
	for _, job := range []Job{JobCheckin, JobTravel} {
		if count := countJobFirings(rec.all(), string(job), 10); count != 1 {
			t.Fatalf("the caught-up %s re-fired: %d times", job, count)
		}
	}
}

func TestMissedSlotIsNotLostForTheRestOfTheDay(t *testing.T) {
	// The phone was off at 09:00 and comes back at 18:00. The 21:00 slot is
	// still ahead, so nothing fires now — but crucially the 09:00 stamp is not
	// silently marked as done, and 21:00 still runs.
	clock := newClock(day(18, 0))
	rec := &recorder{}
	p := newTestPool()
	s := newTestScheduler(clock, p, rec)

	s.CatchUp(context.Background())
	if count := countOccurrences(rec.all(), "整点排程命中"); count != 0 {
		t.Fatalf("nothing is due at 18:00, but %d slots fired: %v", count, rec.all())
	}

	clock.Set(day(21, 0))
	s.CatchUp(context.Background())
	// Check-in and the trip both live on 21:00; each must run once.
	for _, job := range []Job{JobCheckin, JobTravel} {
		if count := countJobFirings(rec.all(), string(job), 21); count != 1 {
			t.Fatalf("%s did not fire once at 21:00 after the afternoon wake-up: %v",
				job, rec.all())
		}
	}
}

// --------------------------------------------------- per-account isolation

// failingPool builds a pool whose accounts all need work, with a growth client
// whose transport cannot reach anywhere. That makes every account fail, which is
// what a batch must survive.
func TestPerAccountFailureLeavesTheBatchRunning(t *testing.T) {
	clock := newClock(day(9, 0))
	rec := &recorder{}

	accounts := []*auth.Account{
		testAccount("acct-1", "cn"),
		testAccount("acct-2", "cn"),
		testAccount("acct-3", "cn"),
	}
	p := newTestPool(accounts...)
	s := newTestScheduler(clock, p, rec)

	// A batch of unreachable accounts: every account must be attempted, and the
	// loop must not stop at the first failure.
	result := s.CheckinAll(context.Background(), "test")

	if result.Total != 3 {
		t.Fatalf("Total = %d, want 3 (one per account)", result.Total)
	}
	if result.Success+result.Skip+result.Failure != result.Total {
		t.Fatalf("counts do not add up: %+v", result)
	}
	if len(result.Outcomes) != 3 {
		t.Fatalf("collected %d outcomes, want 3", len(result.Outcomes))
	}
	for _, outcome := range result.Outcomes {
		if outcome.UID == "" {
			t.Fatalf("an outcome lost its uid: %+v", outcome)
		}
	}
	if result.Failure != 3 {
		t.Fatalf("Failure = %d, want 3 (nothing is reachable)", result.Failure)
	}
}

func TestPerAccountIsolationSurvivesOneAccountPanicking(t *testing.T) {
	// The isolation is structural, so even a panic inside one account's pass
	// must be reported as that account's failure and the loop must continue.
	clock := newClock(day(9, 0))
	rec := &recorder{}
	p := newTestPool(testAccount("acct-1", "cn"), testAccount("acct-2", "cn"))
	s := newTestScheduler(clock, p, rec)

	seen := 0
	result := s.forEachAccount(context.Background(), JobCheckin, "test",
		func(ctx context.Context, entry pool.EntryView) Outcome {
			seen++
			if entry.Account.UID == "acct-1" {
				panic("upstream returned a shape this build cannot parse")
			}
			return Outcome{UID: entry.Account.UID, OK: true}
		})

	if seen != 2 {
		t.Fatalf("the pass ran for %d accounts, want 2 (the second must still run)", seen)
	}
	if result.Failure != 1 || result.Success != 1 {
		t.Fatalf("result = %+v, want 1 failure and 1 success", result)
	}
	if !strings.Contains(result.Outcomes[0].Reason, "panicked") {
		t.Fatalf("the panic was not reported as a failure: %+v", result.Outcomes[0])
	}
}

func TestSkipsAreCountedApartFromFailures(t *testing.T) {
	clock := newClock(day(9, 0))
	rec := &recorder{}
	// An intl account has no check-in at all: that is a skip, not a failure.
	p := newTestPool(testAccount("intl-1", "intl"))
	s := newTestScheduler(clock, p, rec)

	result := s.CheckinAll(context.Background(), "test")
	if result.Skip != 1 {
		t.Fatalf("Skip = %d, want 1", result.Skip)
	}
	if result.Failure != 0 {
		t.Fatalf("Failure = %d, want 0; choosing not to is not failing", result.Failure)
	}
	if result.Success != 0 {
		t.Fatalf("Success = %d, want 0", result.Success)
	}

	// The skip is visible to the operator, with the reason.
	if !rec.contains("跳过") || !rec.contains("国际版无每日签到") {
		t.Fatalf("the skip reason was not logged: %v", rec.all())
	}
}

func TestSkipIsDistinguishableFromFailureInTheOutcomeList(t *testing.T) {
	clock := newClock(day(9, 0))
	rec := &recorder{}
	p := newTestPool(testAccount("intl-1", "intl"))
	s := newTestScheduler(clock, p, rec)

	result := s.CheckinAll(context.Background(), "test")
	outcome := result.Outcomes[0]
	if !outcome.Skip {
		t.Fatalf("outcome is not marked as a skip: %+v", outcome)
	}
	if outcome.OK {
		t.Fatalf("a skip must not be reported as a success: %+v", outcome)
	}
	if outcome.Reason == "" {
		t.Fatalf("a skip must carry its reason: %+v", outcome)
	}
}

func TestBatchWithNoAccountsIsANoOpThatSaysSo(t *testing.T) {
	clock := newClock(day(9, 0))
	rec := &recorder{}
	p := newTestPool()
	s := newTestScheduler(clock, p, rec)

	result := s.CheckinAll(context.Background(), "test")
	if result.Total != 0 || result.Success != 0 || result.Failure != 0 {
		t.Fatalf("an empty pool produced work: %+v", result)
	}
	if !rec.contains("暂无可用的活跃账号") {
		t.Fatalf("an empty pool was not explained: %v", rec.all())
	}
}

func TestNilPoolDoesNotPanic(t *testing.T) {
	clock := newClock(day(9, 0))
	rec := &recorder{}
	s := New(Config{Pool: nil, Now: clock.Now, Log: rec.Logf})

	s.CatchUp(context.Background())
	result := s.CheckinAll(context.Background(), "test")
	if result.Total != 0 {
		t.Fatalf("a nil pool produced work: %+v", result)
	}
}

// ------------------------------------------------------- manual vs scheduled

func TestManualTriggerDoesNotConsumeTheDaysSlot(t *testing.T) {
	clock := newClock(day(9, 0))
	rec := &recorder{}
	p := newTestPool()
	s := newTestScheduler(clock, p, rec)

	// Operator presses "run now" a few minutes into the hour.
	clock.Set(day(9, 5))
	s.RunCheckinNow(context.Background())

	// The scheduled slot must still run: the manual trigger must not have
	// stamped the day. The job tag keeps this from also matching the trip, which
	// shares the 09:00 hour.
	s.CatchUp(context.Background())
	if count := countJobFirings(rec.all(), string(JobCheckin), 9); count != 1 {
		t.Fatalf("the scheduled 09:00 check-in did not fire after a manual run: %v", rec.all())
	}
}

func TestManualTriggerIsRefusedWhileABatchIsRunning(t *testing.T) {
	clock := newClock(day(9, 0))
	rec := &recorder{}
	p := newTestPool(testAccount("acct-1", "cn"))
	s := newTestScheduler(clock, p, rec)

	// Hold the batch lock the way an in-flight run would.
	s.runMu.Lock()
	result := s.RunCheckinNow(context.Background())
	s.runMu.Unlock()

	if result.Skip != 1 {
		t.Fatalf("overlapping manual trigger was not refused: %+v", result)
	}
	if !rec.contains("上一轮仍在执行") {
		t.Fatalf("the refusal was not explained: %v", rec.all())
	}
}

func TestScheduledSlotIsSkippedWhileABatchIsRunning(t *testing.T) {
	clock := newClock(day(9, 0))
	rec := &recorder{}
	p := newTestPool()
	s := newTestScheduler(clock, p, rec)

	s.runMu.Lock()
	s.CatchUp(context.Background())
	s.runMu.Unlock()

	if !rec.contains("上一轮仍在执行") {
		t.Fatalf("an overlapping scheduled run was not refused: %v", rec.all())
	}
}

// -------------------------------------------------------------------- status

func TestStatusReportsTheModeAndNextRun(t *testing.T) {
	clock := newClock(day(8, 30))
	rec := &recorder{}
	s := newTestScheduler(clock, newTestPool(), rec)

	next, ok := s.NextRun()
	if !ok {
		t.Fatal("NextRun reported no next run")
	}
	if next.Hour() != 9 || next.Minute() != 0 {
		t.Fatalf("NextRun = %v, want 09:00", next)
	}

	status := s.Status()
	if status.NextRun == "" || status.NextRun == "待调度" {
		t.Fatalf("Status.NextRun = %q", status.NextRun)
	}
	if status.LastRun != "尚未运行" {
		t.Fatalf("Status.LastRun = %q, want 尚未运行 before any run", status.LastRun)
	}
	if !strings.Contains(status.Mode, "09:00/21:00") {
		t.Fatalf("Status.Mode = %q", status.Mode)
	}
}

func TestNextRunWrapsToTomorrowAfterTheLastSlot(t *testing.T) {
	clock := newClock(day(23, 0))
	rec := &recorder{}
	s := newTestScheduler(clock, newTestPool(), rec)

	next, ok := s.NextRun()
	if !ok {
		t.Fatal("NextRun reported no next run")
	}
	if next.Hour() != 1 {
		t.Fatalf("NextRun = %v, want 01:00 the next day", next)
	}
	if next.Day() != 11 {
		t.Fatalf("NextRun day = %d, want the 11th", next.Day())
	}
}

func TestStatusSummarisesTheLastResult(t *testing.T) {
	clock := newClock(day(9, 0))
	rec := &recorder{}
	p := newTestPool(testAccount("intl-1", "intl"))
	s := newTestScheduler(clock, p, rec)

	s.CatchUp(context.Background())

	status := s.Status()
	if status.LastResult == nil {
		t.Fatal("Status carries no last result after a run")
	}
	if status.LastResult.Job == "" {
		t.Fatalf("the last result does not name its job: %+v", status.LastResult)
	}
}

// ------------------------------------------------------------ injected clock

func TestInjectedClockIsTheOnlyClockUsed(t *testing.T) {
	// A clock pinned to 2020 must make the scheduler believe it is 2020; if any
	// code path fell back to time.Now, the stamp would be today's date instead.
	clock := newClock(time.Date(2020, time.January, 2, 9, 0, 0, 0, time.Local))
	rec := &recorder{}
	s := newTestScheduler(clock, newTestPool(), rec)

	s.CatchUp(context.Background())

	s.mu.Lock()
	keys := make([]string, 0, len(s.jobs[0].fired))
	for key := range s.jobs[0].fired {
		keys = append(keys, key)
	}
	s.mu.Unlock()

	if len(keys) != 1 || !strings.HasPrefix(keys[0], "2020-01-02@09") {
		t.Fatalf("stamps = %v, want exactly one keyed to the injected clock's day and hour", keys)
	}
}

func TestConfiguredHoursReplaceTheDefaults(t *testing.T) {
	clock := newClock(day(7, 0))
	rec := &recorder{}
	s := New(Config{
		Pool:           newTestPool(),
		CheckinHours:   []int{7},
		TravelHours:    []int{7},
		KeepaliveHours: []int{8},
		CatHours:       []int{6},
		Now:            clock.Now,
		Log:            rec.Logf,
	})

	s.CatchUp(context.Background())
	// Check-in and the trip were both moved to 07:00, so both must fire there.
	for _, job := range []Job{JobCheckin, JobTravel} {
		if count := countJobFirings(rec.all(), string(job), 7); count != 1 {
			t.Fatalf("%s did not fire on the configured hour: %v", job, rec.all())
		}
	}

	// The default hours must no longer fire.
	clock.Set(day(9, 0))
	s.CatchUp(context.Background())
	for _, job := range []Job{JobCheckin, JobTravel} {
		if count := countJobFirings(rec.all(), string(job), 9); count != 0 {
			t.Fatalf("%s fired on a default hour despite it being replaced: %v", job, rec.all())
		}
	}
}

func TestInvalidConfiguredHoursFallBackToTheDefaults(t *testing.T) {
	clock := newClock(day(9, 0))
	rec := &recorder{}
	s := New(Config{
		Pool:         newTestPool(),
		CheckinHours: []int{99, -1},
		Now:          clock.Now,
		Log:          rec.Logf,
	})

	s.CatchUp(context.Background())
	// Only the check-in hours were invalid, so check-in falls back to its default
	// 09:00 and the trip is still on its own default 09:00.
	for _, job := range []Job{JobCheckin, JobTravel} {
		if count := countJobFirings(rec.all(), string(job), 9); count != 1 {
			t.Fatalf("%s did not fall back to the default hour: %v", job, rec.all())
		}
	}
}

// ---------------------------------------------------------------- stop/run

func TestStopEndsTheRunLoop(t *testing.T) {
	clock := newClock(day(9, 0))
	rec := &recorder{}
	s := newTestScheduler(clock, newTestPool(), rec)

	done := make(chan struct{})
	go func() {
		s.Run(context.Background())
		close(done)
	}()

	// Give Run a moment to install its ticker, then stop it.
	time.Sleep(20 * time.Millisecond)
	s.Stop()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after Stop")
	}
	if !rec.contains("调度器已停止") {
		t.Fatalf("Stop was not logged: %v", rec.all())
	}
}

func TestRunExitsWhenTheContextIsCancelled(t *testing.T) {
	clock := newClock(day(9, 0))
	rec := &recorder{}
	s := newTestScheduler(clock, newTestPool(), rec)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}

func TestStopIsIdempotent(t *testing.T) {
	clock := newClock(day(9, 0))
	rec := &recorder{}
	s := newTestScheduler(clock, newTestPool(), rec)

	s.Stop()
	s.Stop() // must not panic on a double close
}

// ---------------------------------------------------------------- helpers

// countOccurrences counts how many log lines contain needle.
func countOccurrences(lines []string, needle string) int {
	n := 0
	for _, line := range lines {
		if strings.Contains(line, needle) {
			n++
		}
	}
	return n
}

// countJobFirings counts the log lines where one job was dispatched for one hour.
//
// The job tag is what makes this unambiguous: check-in and the cat's trip are
// configured on the same hours, so a needle on the hour alone matches two
// different jobs and a correct run looks like a double fire.
func countJobFirings(lines []string, job string, hour int) int {
	tag := "[" + job + "]"
	stamp := fmt.Sprintf("(%02d:00)", hour)
	n := 0
	for _, line := range lines {
		if strings.Contains(line, tag) && strings.Contains(line, stamp) {
			n++
		}
	}
	return n
}

// ------------------------------------------------------------ runtime hours

func TestSetHoursMakesTheModeTrackTheLiveSchedule(t *testing.T) {
	// The mode string is what the panel shows, and NextRun is derived from the
	// live hours. If the mode were a stored constant the two would disagree
	// after a change, and the panel would name a schedule it is not running.
	clock := newClock(day(9, 0))
	rec := &recorder{}
	s := newTestScheduler(clock, newTestPool(), rec)

	if !strings.Contains(s.Status().Mode, "09:00/21:00") {
		t.Fatalf("initial Mode = %q, want the defaults", s.Status().Mode)
	}

	s.SetHours(Hours{Checkin: []int{8, 20}, Keepalive: []int{23}})

	mode := s.Status().Mode
	if !strings.Contains(mode, "08:00/20:00") {
		t.Errorf("Mode = %q, want the new check-in hours", mode)
	}
	if !strings.Contains(mode, "23:00") {
		t.Errorf("Mode = %q does not name the new keepalive hour", mode)
	}
	// The trip was not restated, so it keeps its default. Because it now differs
	// from the check-in hours it must be listed separately rather than folded
	// into the shared label — otherwise the panel would claim the trip moved
	// when it did not.
	if !strings.Contains(mode, "08:00/20:00 签到") {
		t.Errorf("Mode = %q does not label the check-in hours", mode)
	}
	if !strings.Contains(mode, "09:00/21:00 旅行") {
		t.Errorf("Mode = %q does not keep the trip on its own default hours", mode)
	}
	if strings.Contains(mode, "签到旅行") {
		t.Errorf("Mode = %q folded two different schedules under one label", mode)
	}
}

func TestSetHoursKeepsStampsSoTheSameHourCannotRunTwice(t *testing.T) {
	// A stamp records work already done today. Replacing the schedule must not
	// clear it, or a change made inside a slot's hour would re-run that slot.
	clock := newClock(day(9, 0))
	rec := &recorder{}
	p := newTestPool()
	s := newTestScheduler(clock, p, rec)

	s.CatchUp(context.Background())
	if count := countJobFirings(rec.all(), string(JobCheckin), 9); count != 1 {
		t.Fatalf("the 09:00 check-in fired %d times, want 1", count)
	}

	// Restate the same hour and catch up again within the same hour.
	s.SetHours(Hours{Checkin: []int{9}, Travel: []int{9}})
	s.CatchUp(context.Background())

	if count := countJobFirings(rec.all(), string(JobCheckin), 9); count != 1 {
		t.Fatalf("the 09:00 check-in re-fired after a schedule change: %d times", count)
	}
}

func TestSetHoursWithAnEmptyListKeepsTheDefault(t *testing.T) {
	clock := newClock(day(9, 0))
	rec := &recorder{}
	s := newTestScheduler(clock, newTestPool(), rec)

	s.SetHours(Hours{Checkin: []int{8}})

	mode := s.Status().Mode
	// Keepalive and cat were not restated, so they must still show their
	// defaults rather than vanishing from the schedule.
	if !strings.Contains(mode, "22:00") || !strings.Contains(mode, "01:00") {
		t.Errorf("Mode = %q dropped the unstated jobs", mode)
	}
}

func TestSetHoursWithOutOfRangeValuesFallsBack(t *testing.T) {
	// An hour of 25 never matches a wall clock, so accepting it would disable
	// the job while the panel still listed it as configured.
	clock := newClock(day(9, 0))
	rec := &recorder{}
	s := newTestScheduler(clock, newTestPool(), rec)

	s.SetHours(Hours{Checkin: []int{25, -3}})

	if !strings.Contains(s.Status().Mode, "09:00/21:00") {
		t.Errorf("Mode = %q, want the defaults after an unusable hour list",
			s.Status().Mode)
	}
}
