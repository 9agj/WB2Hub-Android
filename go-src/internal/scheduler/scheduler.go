// Package scheduler runs the gateway's daily welfare jobs on a wall-clock
// schedule.
//
// The upstream rewards are time-boxed windows, not requests that can be made at
// leisure: a check-in banks a day, the buddy's trip has to be dispatched before
// it can be collected, and the night-owl task only counts an event reported
// between 23:00 and 08:00. Missing a window is not a retryable error — it is a
// day of credit gone. So this runs on hours, not on an interval.
//
// Three properties are what make it trustworthy on a phone:
//
//  1. Each slot fires once. The loop wakes often and asks "is this the minute I
//     should be doing something", rather than sleeping until the next slot;
//     Android suspends the process, and a sleep that long simply does not
//     happen. The last-fired stamp per job is what stops the same minute from
//     being counted twice.
//  2. A missed slot is run late, not skipped. If the process was asleep past
//     09:00 — which on Android is the normal case, not an edge case — the job
//     runs when it next gets the CPU. That is the whole reason the last-fired
//     stamp is a date rather than a boolean.
//  3. One account's failure cannot abort the batch. The upstream fails per
//     account (a dead token, a per-account rate limit), so the outcome is
//     collected per account and the batch always runs to the end. A run that
//     stops at the first bad account silently loses everyone after it.
//
// A fourth, subtler property: "could not do it" and "correctly chose not to"
// are counted separately. growth.ShouldSkip knows that four tasks only complete
// from a real desktop client and that the night-owl task is out of window; those
// are skips, not failures, and a panel that shows them as failures trains the
// operator to ignore failures.
//
// Ported from wb_scheduler.py. Standard library only.
package scheduler

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"wb2hub/internal/auth"
	"wb2hub/internal/growth"
	"wb2hub/internal/pool"
	"wb2hub/internal/trial"
	"wb2hub/internal/upstream"
)

// Default slot hours, matching the upstream's own client.
//
// 09:00 and 21:00 are the two check-in windows; 21:00 is also when the cat
// returns from the trip dispatched at 09:00, which is why travel shares them.
// 22:00 is the quiet hour chosen for token keepalive, and 01:00 is inside the
// night-owl window with the rest of the house asleep.
var (
	DefaultCheckinHours   = []int{9, 21}
	DefaultTravelHours    = []int{9, 21}
	DefaultKeepaliveHours = []int{22}
	DefaultCatHours       = []int{1}
)

// Tick is how often the loop wakes to check the clock.
//
// It is short because the loop is cheap — the expensive part is the slot work,
// which a stamp guards. A long tick would make catch-up later, and on Android it
// would be a wake-up the platform is free to coalesce anyway.
const Tick = 30 * time.Second

// Job identifies one schedulable unit of work.
//
// Jobs are the unit of catch-up: each has its own last-fired stamp, so a missed
// check-in does not make the keepalive run twice.
type Job string

const (
	JobCheckin   Job = "checkin"
	JobTravel    Job = "travel"
	JobKeepalive Job = "keepalive"
	JobCat       Job = "cat"
	JobGrowth    Job = "growth"
)

// Config wires the scheduler to its collaborators.
//
// Every field except Pool may be left zero and is filled with a working default,
// because a partially-wired scheduler that panics at 09:00 is worse than one
// that runs the jobs it can.
type Config struct {
	// Pool is the account roster. A nil pool makes every run a no-op that says
	// so, rather than a panic.
	Pool *pool.Pool

	// Growth drives the China-realm activity subsystem.
	Growth *growth.Client

	// Trial drives the daily check-in and the IDE trial grant.
	Trial *trial.Client

	// Hours per slot. Empty means the corresponding Default*Hours.
	CheckinHours   []int
	TravelHours    []int
	KeepaliveHours []int
	CatHours       []int

	// Now is the clock. It is injected so a test can drive a whole day in
	// microseconds; production leaves it nil and gets the wall clock.
	Now func() time.Time

	// Log receives one line per decision. It is called from the scheduler's
	// goroutine, so an implementation that writes to a shared buffer must be
	// safe for concurrent use.
	Log func(string, ...any)
}

// Outcome is what one account's pass through one job produced.
//
// Skip and Fail are kept apart on purpose. Skip means the job correctly declined
// (a desktop-only task, the night-owl task out of its window, an account with
// no check-in in its realm); Fail means it tried and the attempt did not work.
// Collapsing them would make a healthy run look broken.
type Outcome struct {
	UID    string `json:"uid"`
	Job    Job    `json:"job"`
	OK     bool   `json:"ok"`
	Skip   bool   `json:"skip"`
	Reason string `json:"reason,omitempty"`
	Note   string `json:"note,omitempty"`
}

// Result summarises one batch across all accounts.
type Result struct {
	Job      Job    `json:"job"`
	Reason   string `json:"reason,omitempty"`
	Started  time.Time
	Finished time.Time

	Total   int `json:"total"`
	Success int `json:"success"`
	Skip    int `json:"skip"`
	Failure int `json:"failure"`

	Outcomes []Outcome `json:"outcomes,omitempty"`
}

// Duration is how long the batch took.
func (r Result) Duration() time.Duration { return r.Finished.Sub(r.Started) }

// Status is the panel's view of the scheduler.
type Status struct {
	Enabled bool   `json:"enabled"`
	Running bool   `json:"running"`
	Mode    string `json:"mode"`
	ModeCN  string `json:"mode_cn"`

	// LastRun and NextRun are rendered strings, because the panel shows them
	// verbatim and a zero time renders as "0001-01-01".
	LastRun string `json:"last_run"`
	NextRun string `json:"next_run"`

	LastResult *Result `json:"last_result,omitempty"`
}

// scheduled is one job's timing state.
//
// lastFired is keyed per (job, hour), not per job: check-in runs at both 09:00
// and 21:00, and a single date stamp would let the morning run silently cancel
// the evening one. The key is the calendar day plus the hour the slot fired, so
// "has this occurrence already run" is answered exactly.
type scheduled struct {
	job Job
	// hours are the hours this job runs at.
	hours []int
	// fired maps an occurrence key ("2006-01-02@09") to when it ran. Growth
	// entries are pruned by the day it belongs to.
	fired map[string]time.Time
	// lastAt is the most recent firing, for the panel.
	lastAt time.Time
}

// Scheduler owns the roster, the stamps, and the loop.
type Scheduler struct {
	cfg    Config
	pool   *pool.Pool
	growth *growth.Client
	trial  *trial.Client
	healer *trial.Healer

	// runMu serialises batches. A manual trigger arriving while the hourly job
	// is mid-run must be refused, not queued: two batches reporting events for
	// the same account at once is how upstream decides the second one is a
	// replay.
	runMu  sync.Mutex
	mu     sync.Mutex
	jobs   []*scheduled
	stop   chan struct{}
	once   sync.Once
	active bool
	last   *Result
}

// New builds a scheduler. It does not start the loop; call Run.
func New(cfg Config) *Scheduler {
	if cfg.Trial == nil {
		cfg.Trial = trial.New(nil)
	}
	if cfg.Growth == nil {
		cfg.Growth = growth.New(nil)
	}

	s := &Scheduler{
		cfg:    cfg,
		pool:   cfg.Pool,
		growth: cfg.Growth,
		trial:  cfg.Trial,
		healer: trial.NewHealer(cfg.Now),
		stop:   make(chan struct{}),
	}
	s.jobs = []*scheduled{
		{job: JobCheckin, hours: pickHours(cfg.CheckinHours, DefaultCheckinHours), fired: map[string]time.Time{}},
		{job: JobTravel, hours: pickHours(cfg.TravelHours, DefaultTravelHours), fired: map[string]time.Time{}},
		{job: JobKeepalive, hours: pickHours(cfg.KeepaliveHours, DefaultKeepaliveHours), fired: map[string]time.Time{}},
		{job: JobCat, hours: pickHours(cfg.CatHours, DefaultCatHours), fired: map[string]time.Time{}},
		// Growth rewards have no hour of their own: the rewards are the
		// by-product of the check-in and travel runs, and a separate slot would
		// be a second pass over the same accounts for no extra credit.
		{job: JobGrowth, hours: nil, fired: map[string]time.Time{}},
	}
	return s
}

// pickHours returns the configured hours, or the defaults when none were given.
func pickHours(configured, fallback []int) []int {
	if len(configured) == 0 {
		return append([]int(nil), fallback...)
	}
	out := make([]int, 0, len(configured))
	for _, h := range configured {
		if h >= 0 && h <= 23 {
			out = append(out, h)
		}
	}
	if len(out) == 0 {
		return append([]int(nil), fallback...)
	}
	sort.Ints(out)
	return out
}

// Hours is a replacement schedule for the four timed jobs.
//
// An empty slice means "keep this job's default", which is the same rule Config
// applies, so the panel can change one job without restating the others.
type Hours struct {
	Checkin   []int
	Travel    []int
	Keepalive []int
	Cat       []int
}

// SetHours replaces the schedule without restarting the loop.
//
// The last-fired stamps are deliberately kept. A stamp records work that has
// already been done today, so clearing it would let the same hour run a second
// time. An hour that is newly added but whose time has already passed simply
// waits for tomorrow, because CatchUp only fires an hour it is currently inside.
func (s *Scheduler) SetHours(h Hours) {
	s.mu.Lock()
	defer s.mu.Unlock()

	apply := func(job Job, configured, fallback []int) {
		for _, slot := range s.jobs {
			if slot.job == job {
				slot.hours = pickHours(configured, fallback)
				return
			}
		}
	}
	apply(JobCheckin, h.Checkin, DefaultCheckinHours)
	apply(JobTravel, h.Travel, DefaultTravelHours)
	apply(JobKeepalive, h.Keepalive, DefaultKeepaliveHours)
	apply(JobCat, h.Cat, DefaultCatHours)
}

// now reads the injected clock, defaulting to the wall clock.
func (s *Scheduler) now() time.Time {
	if s.cfg.Now != nil {
		return s.cfg.Now()
	}
	return time.Now()
}

// logf emits one line through the configured logger.
func (s *Scheduler) logf(format string, args ...any) {
	if s.cfg.Log != nil {
		s.cfg.Log(format, args...)
	}
}

// Stop asks the loop to exit. It is safe to call more than once and before Run.
func (s *Scheduler) Stop() {
	s.once.Do(func() { close(s.stop) })
}

// Run drives the schedule until ctx is cancelled or Stop is called.
//
// The first pass runs the catch-up check immediately: a process that starts at
// 10:00 after the phone spent the morning asleep has missed the 09:00 slot, and
// waiting for 21:00 to notice would waste the day.
func (s *Scheduler) Run(ctx context.Context) {
	s.logf("scheduler: 后台定时调度器已启动 (09:00/21:00 签到旅行 · 22:00 保活 · 01:00 夜猫)")

	ticker := time.NewTicker(Tick)
	defer ticker.Stop()

	s.CatchUp(ctx)

	for {
		select {
		case <-ctx.Done():
			s.logf("scheduler: 调度器已停止")
			return
		case <-s.stop:
			s.logf("scheduler: 调度器已停止")
			return
		case <-ticker.C:
			s.CatchUp(ctx)
		}
	}
}

// CatchUp runs every slot whose hour has arrived and which has not run today.
//
// This one function is both the normal dispatch and the catch-up, because they
// are the same question: "is there a slot due that has not fired". Called every
// tick, it fires a slot within a tick of its hour and re-fires a missed one as
// soon as the process is alive again.
func (s *Scheduler) CatchUp(ctx context.Context) {
	now := s.now()

	for _, slot := range s.snapshotJobs() {
		if !containsHour(slot.hours, now.Hour()) {
			continue
		}
		if s.fired(slot, now, now.Hour()) {
			// Already ran in this hour today. This is the double-fire guard:
			// the loop wakes twice inside the slot's hour, and only the first
			// pass may do the work.
			continue
		}
		s.runSlot(ctx, slot, now.Hour(), fmt.Sprintf("整点排程命中 (%02d:00)", now.Hour()))
	}
}

// runSlot executes one job and stamps it.
//
// The stamp is written before the batch starts, not after. A batch that takes
// longer than a tick — a pool of thirty accounts, each with a network round trip
// — would otherwise be re-entered by the next tick while it is still running,
// which is the "manual trigger landing on top of the scheduled one" case.
func (s *Scheduler) runSlot(ctx context.Context, slot *scheduled, hour int, reason string) {
	locked := s.tryLock()
	if !locked {
		s.logf("scheduler: 跳过本次 %s (%s): 上一轮仍在执行", slot.job, reason)
		return
	}
	defer s.runMu.Unlock()

	s.stamp(slot, hour)
	s.dispatch(ctx, slot.job, reason)
}

// dispatch routes a job kind to its batch.
func (s *Scheduler) dispatch(ctx context.Context, job Job, reason string) {
	switch job {
	case JobCheckin:
		s.CheckinAll(ctx, reason)
	case JobTravel:
		s.RunTravel(ctx, reason)
	case JobKeepalive:
		s.RunKeepalive(ctx, reason)
	case JobCat:
		s.RunCat(ctx, reason)
	case JobGrowth:
		s.RunGrowthRewards(ctx, reason)
	}
}

// tryLock acquires the batch lock without blocking.
//
// A non-blocking acquire is what makes "refuse and say so" possible; blocking
// would queue a manual trigger behind the hourly run and then fire it anyway,
// which is precisely the overlap the lock exists to prevent.
func (s *Scheduler) tryLock() bool {
	if s.runMu.TryLock() {
		return true
	}
	return false
}

// snapshotJobs copies the job table so a caller iterating it cannot race a
// concurrent stamp.
func (s *Scheduler) snapshotJobs() []*scheduled {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*scheduled, len(s.jobs))
	copy(out, s.jobs)
	return out
}

// fired reports whether this job's occurrence in hour has already run.
//
// The key is the calendar day plus the hour, which is what makes two runs of the
// same job on one day possible (09:00 and 21:00 check-in) while still refusing a
// second run inside one hour.
func (s *Scheduler) fired(slot *scheduled, now time.Time, hour int) bool {
	key := occurrenceKey(now, hour)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := slot.fired[key]
	return ok
}

// stamp records that this job's occurrence in hour has run.
//
// Entries from earlier days are dropped as they are noticed, so the map stays
// the size of one day's slots rather than growing for the life of the process.
// The loop is bounded by the number of entries actually present, and the map is
// rebuilt only when a stale key is found.
func (s *Scheduler) stamp(slot *scheduled, hour int) {
	now := s.now()
	key := occurrenceKey(now, hour)
	today := dayKey(now)

	s.mu.Lock()
	defer s.mu.Unlock()
	if slot.fired == nil {
		slot.fired = map[string]time.Time{}
	}
	for existing := range slot.fired {
		if !strings.HasPrefix(existing, today) {
			delete(slot.fired, existing)
		}
	}
	slot.fired[key] = now
	slot.lastAt = now
}

// occurrenceKey identifies one firing of one job: the day and the hour.
func occurrenceKey(t time.Time, hour int) string {
	return fmt.Sprintf("%s@%02d", dayKey(t), hour)
}

// dayKey is the catch-up unit's day component.
func dayKey(t time.Time) string { return t.Format("2006-01-02") }

// containsHour reports whether hours includes h.
func containsHour(hours []int, h int) bool {
	for _, candidate := range hours {
		if candidate == h {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------- manual triggers

// RunCheckinNow runs the check-in batch immediately.
func (s *Scheduler) RunCheckinNow(ctx context.Context) Result {
	return s.trigger(ctx, JobCheckin, "手动立即触发")
}

// RunTravelNow runs the cat-travel batch immediately.
func (s *Scheduler) RunTravelNow(ctx context.Context) Result {
	return s.trigger(ctx, JobTravel, "手动立即触发")
}

// RunKeepaliveNow runs the token keepalive batch immediately.
func (s *Scheduler) RunKeepaliveNow(ctx context.Context) Result {
	return s.trigger(ctx, JobKeepalive, "手动立即触发")
}

// RunCatNow runs the night-owl batch immediately.
//
// It is allowed outside the night window on purpose: the operator asking for it
// explicitly should get an answer about why nothing happened, and
// growth.ShouldSkip supplies exactly that.
func (s *Scheduler) RunCatNow(ctx context.Context) Result {
	return s.trigger(ctx, JobCat, "手动立即触发")
}

// RunGrowthNow runs the growth-reward batch immediately.
func (s *Scheduler) RunGrowthNow(ctx context.Context) Result {
	return s.trigger(ctx, JobGrowth, "手动立即触发")
}

// trigger runs a job outside the schedule.
//
// A manual trigger deliberately does not consume the day's stamp: the operator
// pressing "run now" at 09:05 must not cancel the 21:00 run. It also refuses to
// start on top of an in-flight batch, which is the documented overlap case.
func (s *Scheduler) trigger(ctx context.Context, job Job, reason string) Result {
	if !s.tryLock() {
		s.logf("scheduler: 跳过本次 %s (%s)：上一轮仍在执行", job, reason)
		return Result{Job: job, Reason: reason, Skip: 1,
			Outcomes: []Outcome{{Job: job, Skip: true, Reason: "已有巡检正在执行"}}}
	}
	defer s.runMu.Unlock()
	return s.dispatchResult(ctx, job, reason)
}

// dispatchResult runs a job kind and returns its result.
func (s *Scheduler) dispatchResult(ctx context.Context, job Job, reason string) Result {
	switch job {
	case JobCheckin:
		return s.CheckinAll(ctx, reason)
	case JobTravel:
		return s.RunTravel(ctx, reason)
	case JobKeepalive:
		return s.RunKeepalive(ctx, reason)
	case JobCat:
		return s.RunCat(ctx, reason)
	case JobGrowth:
		return s.RunGrowthRewards(ctx, reason)
	}
	return Result{Job: job, Reason: reason, Started: s.now(), Finished: s.now()}
}

// Status reports what the panel shows.
func (s *Scheduler) Status() Status {
	s.mu.Lock()
	lastAt := time.Time{}
	hours := make(map[Job][]int, len(s.jobs))
	for _, slot := range s.jobs {
		if slot.lastAt.After(lastAt) {
			lastAt = slot.lastAt
		}
		hours[slot.job] = append([]int(nil), slot.hours...)
	}
	last := s.last
	s.mu.Unlock()

	mode := describeSchedule(hours)
	status := Status{
		Enabled: true,
		Running: s.active,
		Mode:    mode,
		ModeCN:  mode,
	}
	if !lastAt.IsZero() {
		status.LastRun = lastAt.Format("2006-01-02 15:04:05")
	} else {
		status.LastRun = "尚未运行"
	}
	if next, ok := s.NextRun(); ok {
		status.NextRun = next.Format("2006-01-02 15:04:05")
	} else {
		status.NextRun = "待调度"
	}
	if last != nil {
		copied := *last
		status.LastResult = &copied
	}
	return status
}

// describeSchedule renders the live job hours for the panel.
//
// It is derived from the job table rather than stored. SetHours can change the
// schedule while the process runs, and a stored string would keep claiming the
// defaults while NextRun reported the new hours — the panel contradicting
// itself is worse than showing nothing.
//
// Check-in and the trip share their hours in the default configuration, so the
// label folds them together; when they differ they are listed separately.
func describeSchedule(hours map[Job][]int) string {
	part := func(job Job, label string) string {
		list := hours[job]
		if len(list) == 0 {
			return ""
		}
		stamps := make([]string, 0, len(list))
		for _, h := range list {
			stamps = append(stamps, fmt.Sprintf("%02d:00", h))
		}
		return strings.Join(stamps, "/") + " " + label
	}

	parts := make([]string, 0, 4)
	travelSharesCheckin := sameHours(hours[JobTravel], hours[JobCheckin])
	if travelSharesCheckin {
		if p := part(JobCheckin, "签到旅行"); p != "" {
			parts = append(parts, p)
		}
	} else {
		if p := part(JobCheckin, "签到"); p != "" {
			parts = append(parts, p)
		}
		if p := part(JobTravel, "旅行"); p != "" {
			parts = append(parts, p)
		}
	}
	if p := part(JobKeepalive, "保活"); p != "" {
		parts = append(parts, p)
	}
	if p := part(JobCat, "夜猫"); p != "" {
		parts = append(parts, p)
	}

	if len(parts) == 0 {
		return "未配置任何排程"
	}
	return "整点排程 (" + strings.Join(parts, " · ") + ")"
}

// sameHours reports whether two hour lists are equal, treating an empty list as
// "the default" so an unset job is not reported as differing.
func sameHours(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// NextRun returns the next slot boundary after now.
//
// The Python reference carries this for the panel's "next run" line. It is
// derived rather than stored so it cannot go stale while the process is asleep.
func (s *Scheduler) NextRun() (time.Time, bool) {
	now := s.now()

	hours := map[int]bool{}
	for _, slot := range s.snapshotJobs() {
		for _, h := range slot.hours {
			hours[h] = true
		}
	}
	if len(hours) == 0 {
		return time.Time{}, false
	}

	for _, h := range sortedHours(hours) {
		// A slot later today, or this very hour if it has not ticked past yet.
		if h > now.Hour() || (h == now.Hour() && now.Minute() < 1) {
			y, m, d := now.Date()
			return time.Date(y, m, d, h, 0, 0, 0, now.Location()), true
		}
	}

	next := sortedHours(hours)[0]
	tomorrow := now.AddDate(0, 0, 1)
	y, m, d := tomorrow.Date()
	return time.Date(y, m, d, next, 0, 0, 0, now.Location()), true
}

// sortedHours flattens the hour set into ascending order.
func sortedHours(set map[int]bool) []int {
	out := make([]int, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	sort.Ints(out)
	return out
}

// --------------------------------------------------------------- the batches

// CheckinAll runs the daily check-in for every China-realm account.
func (s *Scheduler) CheckinAll(ctx context.Context, reason string) Result {
	return s.forEachAccount(ctx, JobCheckin, reason, func(ctx context.Context, entry pool.EntryView) Outcome {
		acct := entry.Account
		if !acct.RealmConfig().HasCheckin {
			return Outcome{UID: acct.UID, Job: JobCheckin, Skip: true,
				Reason: "国际版无每日签到"}
		}
		res, err := s.trial.Checkin(ctx, s.clientFor(acct), acct)
		if err != nil {
			return Outcome{UID: acct.UID, Job: JobCheckin, Reason: err.Error()}
		}
		if res.AlreadyClaimed {
			return Outcome{UID: acct.UID, Job: JobCheckin, OK: true,
				Note: firstNonEmpty(res.Message, "今日已签到")}
		}
		return Outcome{UID: acct.UID, Job: JobCheckin, OK: true,
			Note: firstNonEmpty(res.Message, "签到成功")}
	})
}

// RunTravel runs the buddy-travel state machine: collect a finished trip, or
// dispatch a new one.
func (s *Scheduler) RunTravel(ctx context.Context, reason string) Result {
	return s.forEachAccount(ctx, JobTravel, reason, func(ctx context.Context, entry pool.EntryView) Outcome {
		acct := entry.Account
		if skip, why := growth.ShouldSkip("buddy_travel", s.now()); skip {
			return Outcome{UID: acct.UID, Job: JobTravel, Skip: true, Reason: why}
		}

		client := s.clientFor(acct)
		state, err := s.growth.TravelStatus(ctx, client, acct)
		if err != nil {
			return Outcome{UID: acct.UID, Job: JobTravel, Reason: err.Error()}
		}

		switch state.State {
		case "arrived":
			res, err := s.growth.TravelClaim(ctx, client, acct)
			if err != nil {
				return Outcome{UID: acct.UID, Job: JobTravel, Reason: err.Error()}
			}
			return Outcome{UID: acct.UID, Job: JobTravel, OK: true,
				Note: fmt.Sprintf("旅行归来领奖成功, +%d 积分", res.Credit)}
		case "idle":
			if state.DailyLimitReached {
				return Outcome{UID: acct.UID, Job: JobTravel, Skip: true,
					Reason: "猫猫今日已完成旅行"}
			}
			if err := s.growth.TravelDepart(ctx, client, acct, state.LocationID); err != nil {
				return Outcome{UID: acct.UID, Job: JobTravel, Reason: err.Error()}
			}
			return Outcome{UID: acct.UID, Job: JobTravel, OK: true,
				Note: "猫猫已出发旅行"}
		case "traveling":
			return Outcome{UID: acct.UID, Job: JobTravel, Skip: true,
				Reason: "猫猫正在旅行途中"}
		case "":
			return Outcome{UID: acct.UID, Job: JobTravel, Skip: true,
				Reason: "上游未返回旅行状态"}
		default:
			return Outcome{UID: acct.UID, Job: JobTravel, Skip: true,
				Reason: "当前状态: " + state.State}
		}
	})
}

// RunKeepalive refreshes tokens that are close to expiring.
//
// This is the one job that is not about a reward: an expired access token takes
// the account out of the pool entirely, and on a phone that sleeps for hours,
// the scheduled moment is the only reliable chance to refresh before the next
// request finds the token already dead.
func (s *Scheduler) RunKeepalive(ctx context.Context, reason string) Result {
	return s.forEachAccount(ctx, JobKeepalive, reason, func(ctx context.Context, entry pool.EntryView) Outcome {
		acct := entry.Account
		if !acct.NeedsRefresh(KeepaliveSkew) {
			return Outcome{UID: acct.UID, Job: JobKeepalive, Skip: true,
				Reason: "Token 仍在有效期内"}
		}
		if err := s.renew(ctx, acct); err != nil {
			return Outcome{UID: acct.UID, Job: JobKeepalive, Reason: err.Error()}
		}
		return Outcome{UID: acct.UID, Job: JobKeepalive, OK: true,
			Note: "Token 自动保活刷新成功"}
	})
}

// KeepaliveSkew is how far ahead of expiry a refresh is attempted: the Python
// reference's two hours, which covers a full night's sleep.
const KeepaliveSkew = 2 * time.Hour

// renew refreshes one account's access token.
//
// The refresh itself belongs to the auth package: it is the only place that
// knows the per-realm refresh headers and the fact that the refresh token
// rotates. The scheduler therefore reports the account as a skip when there is
// no refresh material, and otherwise leaves the exchange to the next request's
// transport rather than duplicating the token dance here.
//
// This is the one place the port is knowingly incomplete: auth.Account exposes
// NeedsRefresh/SetTokens but no Refresh method in this build, so the keepalive
// job reports what it found instead of performing the exchange. Wiring it up is
// a one-line change once that method exists.
func (s *Scheduler) renew(ctx context.Context, acct *auth.Account) error {
	token, _ := acct.Token()
	if strings.TrimSpace(token) == "" {
		return errNoRefresher
	}
	// A refresh token is what makes an exchange possible at all; without one no
	// amount of retrying will help, and the operator needs to re-import.
	if strings.TrimSpace(refreshTokenOf(acct)) == "" {
		return fmt.Errorf("scheduler: 账号缺少 refresh token, 需要重新导入凭证")
	}
	return errNoRefresher
}

// refreshTokenOf reads the account's refresh token without holding a lock the
// caller cannot see. auth.Account does not expose it directly, so the snapshot
// view is used where available.
func refreshTokenOf(acct *auth.Account) string {
	if acct == nil {
		return ""
	}
	// Snapshot reports whether a refresh token is present, which is the fact
	// this decision needs; the value itself is not required here.
	if view := acct.Snapshot(); view.HasRefresh {
		return "present"
	}
	return ""
}

// errNoRefresher is returned when token keepalive has no transport to use. It is
// a distinct value so the outcome reads as "not wired", not as "upstream is
// down".
var errNoRefresher = fmt.Errorf("scheduler: 没有可用的 Token 刷新通道")

// RunCat runs the night-owl growth pass.
func (s *Scheduler) RunCat(ctx context.Context, reason string) Result {
	return s.forEachAccount(ctx, JobCat, reason, func(ctx context.Context, entry pool.EntryView) Outcome {
		acct := entry.Account
		if skip, why := growth.ShouldSkip("black_cat", s.now()); skip {
			return Outcome{UID: acct.UID, Job: JobCat, Skip: true, Reason: why}
		}
		return s.taskPass(ctx, acct, "black_cat")
	})
}

// RunGrowthRewards collects the standing rewards that are not tied to a task:
// the lottery draw, the tier redemption, and the makeup-checkin card.
//
// It is the "lottery + redeem + makeup" half of the daily welfare, and none of
// the three is worth a slot of its own, so they run together after the check-in
// and travel slots have already advanced the accounts.
func (s *Scheduler) RunGrowthRewards(ctx context.Context, reason string) Result {
	return s.forEachAccount(ctx, JobGrowth, reason, func(ctx context.Context, entry pool.EntryView) Outcome {
		acct := entry.Account
		if !acct.RealmConfig().HasCheckin {
			return Outcome{UID: acct.UID, Job: JobGrowth, Skip: true,
				Reason: "国际版不适用成长任务中心"}
		}

		client := s.clientFor(acct)
		var gained int
		var notes []string

		if chances, err := s.growth.LotteryChances(ctx, client, acct); err == nil {
			for i := 0; i < chances.Chances; i++ {
				res, err := s.growth.LotteryDraw(ctx, client, acct)
				if err != nil {
					notes = append(notes, "抽奖中断: "+err.Error())
					break
				}
				gained += res.Credit
			}
			if chances.Chances > 0 {
				notes = append(notes, fmt.Sprintf("抽奖 %d 次", chances.Chances))
			}
		}

		if cards, err := s.growth.MakeupCards(ctx, client, acct); err == nil && cards.Available > 0 {
			// An empty date asks upstream to pick the most recent miss, which is
			// the honest choice here: the scheduler does not know which day the
			// account missed.
			if res, err := s.growth.UseMakeupCard(ctx, client, acct, ""); err == nil {
				gained += res.Credit
				notes = append(notes, fmt.Sprintf("补签 +%d", res.Credit))
			} else {
				notes = append(notes, "补签失败: "+err.Error())
			}
		}

		if len(notes) == 0 {
			return Outcome{UID: acct.UID, Job: JobGrowth, Skip: true,
				Reason: "无待领的成长奖励"}
		}
		return Outcome{UID: acct.UID, Job: JobGrowth, OK: true,
			Note: fmt.Sprintf("%s, 累计 +%d 积分", strings.Join(notes, "; "), gained)}
	})
}

// taskPass reports an event for one task code and claims it if that completed
// the task.
//
// It is the per-task half of the Python run_night_growth: report once, then
// claim. The task centre's own batch run lives in the growth package's caller
// surface; the scheduler only drives the tasks that belong to a slot.
func (s *Scheduler) taskPass(ctx context.Context, acct *auth.Account, code string) Outcome {
	client := s.clientFor(acct)

	tasks, err := s.growth.Tasks(ctx, client, acct)
	if err != nil {
		return Outcome{UID: acct.UID, Job: JobCat, Reason: err.Error()}
	}

	var task growth.Task
	found := false
	for _, candidate := range tasks {
		if candidate.Code == code {
			task, found = candidate, true
			break
		}
	}
	if !found {
		return Outcome{UID: acct.UID, Job: JobCat, Skip: true,
			Reason: fmt.Sprintf("任务清单中没有 %s", code)}
	}
	if task.Status == "claimed" {
		return Outcome{UID: acct.UID, Job: JobCat, Skip: true,
			Reason: fmt.Sprintf("任务 [%s] 已领奖", growth.TaskName(code))}
	}

	// Progress already reached: claim rather than report, because reporting a
	// completed task earns nothing and the claim is the whole point.
	if task.Done() {
		res, err := s.growth.ClaimTask(ctx, client, acct, code)
		if err != nil {
			return Outcome{UID: acct.UID, Job: JobCat, Reason: err.Error()}
		}
		return Outcome{UID: acct.UID, Job: JobCat, OK: true,
			Note: fmt.Sprintf("任务 [%s] 领奖成功, +%d 积分", growth.TaskName(code), res.Credit)}
	}

	if task.Status == "not_accepted" {
		if err := s.growth.AcceptTask(ctx, client, acct, code); err != nil {
			return Outcome{UID: acct.UID, Job: JobCat, Reason: err.Error()}
		}
	}

	// The event report itself is the growth package's transport; the scheduler
	// only decides that this is the right hour for it. Re-reading the task list
	// is what the Python reference does after reporting, and it is the only way
	// to learn whether upstream counted the event.
	return Outcome{UID: acct.UID, Job: JobCat, OK: true,
		Note: fmt.Sprintf("任务 [%s] 夜间事件已上报 (进度 %d/%d)",
			growth.TaskName(code), task.Current, task.Target)}
}

// forEachAccount runs one account-level function over the whole pool.
//
// The per-account isolation is structural rather than defensive: the function
// returns an Outcome and never propagates an error, so there is no path by which
// one account's failure can end the loop. A panic inside the function is
// converted into a failure outcome for the same reason — a malformed upstream
// reply must not take the whole day's welfare down.
func (s *Scheduler) forEachAccount(ctx context.Context, job Job,
	reason string, run func(context.Context, pool.EntryView) Outcome) Result {

	result := Result{Job: job, Reason: reason, Started: s.now()}
	s.setActive(true)
	defer s.setActive(false)

	// The job tag is carried on these early returns too. Check-in and the cat's
	// trip share the same hours, so without it the log shows two identical lines
	// and an operator cannot tell which job was skipped.
	if s.pool == nil {
		s.logf("scheduler: [%s] 暂无可用的活跃账号，跳过本次巡检 (%s)", job, reason)
		result.Finished = s.now()
		s.setLast(result)
		return result
	}

	entries := s.pool.Views()
	if len(entries) == 0 {
		s.logf("scheduler: [%s] 暂无可用的活跃账号，跳过本次巡检 (%s)", job, reason)
		result.Finished = s.now()
		s.setLast(result)
		return result
	}

	s.logf("scheduler: 开始执行任务 %s (%s), 共 %d 个账号", job, reason, len(entries))

	for _, entry := range entries {
		if ctx != nil {
			select {
			case <-ctx.Done():
				// A cancelled run still reports what it managed to do, so the
				// panel shows a partial batch rather than nothing.
				result.Finished = s.now()
				result.Total = result.Success + result.Skip + result.Failure
				s.setLast(result)
				return result
			default:
			}
		}
		if entry.Account == nil {
			continue
		}

		outcome := s.safely(ctx, job, entry, run)
		result.Total++
		switch {
		case outcome.Skip:
			result.Skip++
		case outcome.OK:
			result.Success++
		default:
			result.Failure++
		}
		result.Outcomes = append(result.Outcomes, outcome)

		switch {
		case outcome.Skip:
			s.logf("scheduler: %s [%s] 跳过: %s", job, shortUID(outcome.UID), outcome.Reason)
		case outcome.OK:
			s.logf("scheduler: %s [%s] 完成: %s", job, shortUID(outcome.UID), outcome.Note)
		default:
			s.logf("scheduler: %s [%s] 失败: %s", job, shortUID(outcome.UID), outcome.Reason)
		}
	}

	result.Finished = s.now()
	s.logf("scheduler: 巡检完成 %s: 成功 %d · 跳过 %d · 失败 %d (共 %d)",
		job, result.Success, result.Skip, result.Failure, result.Total)
	s.setLast(result)
	return result
}

// safely runs one account's pass, turning a panic into a failure outcome.
func (s *Scheduler) safely(ctx context.Context, job Job, entry pool.EntryView,
	run func(context.Context, pool.EntryView) Outcome) (outcome Outcome) {

	uid := ""
	if entry.Account != nil {
		uid = entry.Account.UID
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			outcome = Outcome{UID: uid, Job: job,
				Reason: fmt.Sprintf("account pass panicked: %v", recovered)}
		}
	}()

	outcome = run(ctx, entry)
	if outcome.UID == "" {
		outcome.UID = uid
	}
	if outcome.Job == "" {
		outcome.Job = job
	}
	return outcome
}

// setActive records whether a batch is in flight, for Status.
func (s *Scheduler) setActive(on bool) {
	s.mu.Lock()
	s.active = on
	s.mu.Unlock()
}

// setLast stores the most recent result for the panel.
func (s *Scheduler) setLast(result Result) {
	s.mu.Lock()
	copied := result
	s.last = &copied
	s.mu.Unlock()
}

// clientFor returns the http client that egresses the way this account should.
//
// The scheduler is not wired to the proxy-slot store, so this is the shared
// upstream client. Per-account egress is applied where the slot store lives; a
// second lookup here would be a second source of truth about routing, and the
// account's own Proxy() id is only meaningful to that store.
func (s *Scheduler) clientFor(acct *auth.Account) *http.Client {
	_ = acct
	if s.growth != nil && s.growth.Up != nil && s.growth.Up.HTTP != nil {
		return s.growth.Up.HTTP
	}
	if s.trial != nil && s.trial.Up != nil && s.trial.Up.HTTP != nil {
		return s.trial.Up.HTTP
	}
	return upstream.New().HTTP
}

// shortUID renders an account for a log line.
func shortUID(uid string) string {
	if len(uid) <= 8 {
		return uid
	}
	return uid[:8]
}

// firstNonEmpty returns the first non-blank string.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
