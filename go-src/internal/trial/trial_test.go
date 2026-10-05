package trial

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"wb2hub/internal/auth"
	"wb2hub/internal/config"
	"wb2hub/internal/upstream"
)

// gateway is a stand-in for the whole upstream: one handler that records what
// was asked for and answers from a script.
type gateway struct {
	mu       sync.Mutex
	requests []string
	// reply maps "METHOD path" to the body to answer with.
	reply map[string]string
	// status maps "METHOD path" to a status code; missing means 200.
	status map[string]int
	server *httptest.Server
}

func newGateway(t *testing.T) *gateway {
	t.Helper()
	g := &gateway{
		reply:  map[string]string{},
		status: map[string]int{},
	}
	g.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		g.mu.Lock()
		g.requests = append(g.requests, key)
		body, ok := g.reply[key]
		code := g.status[key]
		g.mu.Unlock()

		if code == 0 {
			code = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		if ok {
			_, _ = w.Write([]byte(body))
			return
		}
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	t.Cleanup(g.server.Close)
	return g
}

func (g *gateway) saw(t *testing.T, key string) {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, got := range g.requests {
		if got == key {
			return
		}
	}
	t.Errorf("upstream never saw %q; saw %v", key, g.requests)
}

func (g *gateway) count(key string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, got := range g.requests {
		if got == key {
			n++
		}
	}
	return n
}

// testAccount builds an in-memory account bound to the test server's realm.
//
// The realm registry is global, so the test swaps the two upstream hosts and
// restores them when the test ends; nothing else in the package reads the
// realm's other fields.
func testAccount(t *testing.T, g *gateway, realm string) *auth.Account {
	t.Helper()
	original, ok := config.Realms[realm]
	if !ok {
		t.Fatalf("unknown realm %q", realm)
	}
	patched := original
	patched.ChatUpstream = g.server.URL
	patched.BillingUpstream = g.server.URL
	config.Realms[realm] = patched
	t.Cleanup(func() { config.Realms[realm] = original })

	return &auth.Account{UID: "uid-" + realm, Realm: realm}
}

// intlAccount is an account on the international realm, which has no check-in
// and therefore no trial.
func intlAccount() *auth.Account { return &auth.Account{UID: "uid-intl", Realm: config.RealmIntl} }

func TestRealmGateRefusesInternationalAccounts(t *testing.T) {
	acct := intlAccount()
	client := New(upstream.New())
	ctx := context.Background()
	httpClient := &http.Client{Timeout: time.Second}

	calls := map[string]func() error{
		"Claim": func() error {
			_, err := client.Claim(ctx, httpClient, acct)
			return err
		},
		"Checkin": func() error {
			_, err := client.Checkin(ctx, httpClient, acct)
			return err
		},
		"UserResource": func() error {
			_, err := client.UserResource(ctx, httpClient, acct)
			return err
		},
		"ClaimCompensation": func() error {
			_, err := client.ClaimCompensation(ctx, httpClient, acct)
			return err
		},
		"ClaimGift": func() error {
			_, err := client.ClaimGift(ctx, httpClient, acct)
			return err
		},
	}
	for name, call := range calls {
		err := call()
		if err == nil {
			t.Errorf("%s on the intl realm returned no error", name)
			continue
		}
		if !errors.Is(err, ErrNotSupported) {
			t.Errorf("%s on the intl realm returned %v, want ErrNotSupported", name, err)
		}
	}
}

func TestNilAccountIsRefused(t *testing.T) {
	client := New(nil)
	if _, err := client.Checkin(context.Background(), &http.Client{}, nil); !errors.Is(err, ErrNotSupported) {
		t.Errorf("Checkin(nil) = %v, want ErrNotSupported", err)
	}
}

func TestEndpointsAndMethods(t *testing.T) {
	g := newGateway(t)
	acct := testAccount(t, g, config.RealmCN)
	client := New(upstream.New())
	ctx := context.Background()
	httpClient := &http.Client{Timeout: 5 * time.Second}

	if _, err := client.Claim(ctx, httpClient, acct); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	g.saw(t, "POST /billing/ide/trial")

	if _, err := client.Checkin(ctx, httpClient, acct); err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	g.saw(t, "POST /v2/billing/meter/daily-checkin")

	if _, err := client.UserResource(ctx, httpClient, acct); err != nil {
		t.Fatalf("UserResource: %v", err)
	}
	g.saw(t, "POST /v2/billing/meter/get-user-resource")

	if _, err := client.ClaimCompensation(ctx, httpClient, acct); err != nil {
		t.Fatalf("ClaimCompensation: %v", err)
	}
	g.saw(t, "POST /v2/billing/meter/claim-compensation")

	if _, err := client.ClaimGift(ctx, httpClient, acct); err != nil {
		t.Fatalf("ClaimGift: %v", err)
	}
	g.saw(t, "POST /v2/billing/meter/claim-gift")
}

func TestCheckinTreatsDuplicateCodeAsAlreadyClaimed(t *testing.T) {
	g := newGateway(t)
	g.reply["POST /v2/billing/meter/daily-checkin"] = `{"code":10001,"msg":"今日已签到"}`
	acct := testAccount(t, g, config.RealmCN)
	client := New(upstream.New())

	// The Python reference treats 10001 as success; the gateway must too, and
	// must additionally be able to tell the caller it was a duplicate.
	res, err := client.Checkin(context.Background(), &http.Client{Timeout: 5 * time.Second}, acct)
	if err != nil {
		t.Fatalf("Checkin on code 10001 = %v, want success", err)
	}
	if !res.OK || !res.AlreadyClaimed {
		t.Errorf("Checkin on code 10001 = %+v, want ok and already-claimed", res)
	}
	if res.Code != 10001 {
		t.Errorf("Checkin code = %d, want 10001", res.Code)
	}
}

func TestCheckinTreatsBusinessFailureAsError(t *testing.T) {
	g := newGateway(t)
	g.reply["POST /v2/billing/meter/daily-checkin"] = `{"code":500,"msg":"service busy"}`
	acct := testAccount(t, g, config.RealmCN)
	client := New(upstream.New())

	res, err := client.Checkin(context.Background(), &http.Client{Timeout: 5 * time.Second}, acct)
	if err == nil {
		t.Fatal("Checkin on a non-zero business code = nil, want an error")
	}
	if res.OK || res.AlreadyClaimed {
		t.Errorf("failed Checkin reported as %+v", res)
	}
	if !contains(err.Error(), "service busy") {
		t.Errorf("error %q loses the upstream wording", err)
	}
}

func TestNonJSONBodyYieldsAnErrorNotAPanic(t *testing.T) {
	g := newGateway(t)
	g.reply["POST /billing/ide/trial"] = `<html><body>502 Bad Gateway</body></html>`
	g.status["POST /billing/ide/trial"] = http.StatusBadGateway
	acct := testAccount(t, g, config.RealmCN)
	client := New(upstream.New())

	_, err := client.Claim(context.Background(), &http.Client{Timeout: 5 * time.Second}, acct)
	if err == nil {
		t.Fatal("Claim on an HTML error page = nil, want an error")
	}
	if !contains(err.Error(), "502") {
		t.Errorf("error %q does not mention the status", err)
	}
	// The body preview is what the operator has to diagnose with, so it must
	// survive into the message rather than being dropped.
	if !contains(err.Error(), "Bad Gateway") {
		t.Errorf("error %q loses the body preview", err)
	}
}

func TestParseResource(t *testing.T) {
	// A cycle package whose reported used exceeds size-remain: the larger
	// figure is the conservative one and must win.
	body := `{
		"code": 0,
		"data": {"Response": {"Data": {"Accounts": [
			{"PackageName": "Cycle Pack", "CycleCapacitySize": 1000,
			 "CycleCapacityRemain": 400, "CycleCapacityUsed": 700},
			{"PackageName": "Flat Pack", "CapacitySize": 500,
			 "CapacityRemain": 300, "CapacityUsed": 200}
		]}}}
	}`
	decoded := decode(t, body)
	got := ParseResource(decoded)

	if got.Size != 1500 {
		t.Errorf("Size = %d, want 1500", got.Size)
	}
	if got.Used != 900 {
		t.Errorf("Used = %d, want 900 (700 from the cycle pack, 200 flat)", got.Used)
	}
	if got.Remain != 600 {
		t.Errorf("Remain = %d, want 600 (300 cycle + 300 flat)", got.Remain)
	}
	if len(got.Packages) != 2 {
		t.Fatalf("Packages = %+v, want two entries", got.Packages)
	}
	if got.Packages[0].Name != "Cycle Pack" || got.Packages[0].Remain != 300 {
		t.Errorf("cycle package = %+v, want remain 300 after the used correction", got.Packages[0])
	}
}

func TestParseResourceToleratesMissingFields(t *testing.T) {
	cases := map[string]string{
		"no data key":    `{"code":0}`,
		"data is a list": `{"code":0,"data":[]}`,
		"no accounts":    `{"code":0,"data":{"Response":{"Data":{}}}}`,
		"accounts empty": `{"code":0,"data":{"Response":{"Data":{"Accounts":[]}}}}`,
		"truncated nest": `{"code":0,"data":{"Response":{}}}`,
		"junk entries":   `{"code":0,"data":{"Response":{"Data":{"Accounts":["x",null]}}}}`,
		"null data":      `{"code":0,"data":null}`,
	}
	for name, body := range cases {
		got := ParseResource(decode(t, body))
		if got.Remain != 0 || got.Used != 0 || got.Size != 0 || len(got.Packages) != 0 {
			t.Errorf("%s: ParseResource = %+v, want the zero value", name, got)
		}
	}
	if got := ParseResource(nil); got.Remain != 0 || len(got.Packages) != 0 {
		t.Errorf("ParseResource(nil) = %+v, want the zero value", got)
	}
}

func TestParseResourceUsesStringNumbers(t *testing.T) {
	body := `{"code":0,"data":{"Response":{"Data":{"Accounts":[
		{"PackageName":"P","CapacitySize":"100","CapacityRemain":"60","CapacityUsed":"40"}
	]}}}}`
	got := ParseResource(decode(t, body))
	if got.Size != 100 || got.Remain != 60 || got.Used != 40 {
		t.Errorf("ParseResource with string numbers = %+v, want 100/60/40", got)
	}
}

func TestUserResourceReadsTheEnvelopeThroughHTTP(t *testing.T) {
	g := newGateway(t)
	g.reply["POST /v2/billing/meter/get-user-resource"] = `{"code":0,"data":{"Response":{"Data":{"Accounts":[
		{"PackageName":"P","CapacitySize":200,"CapacityRemain":150,"CapacityUsed":50}
	]}}}}`
	acct := testAccount(t, g, config.RealmCN)
	client := New(upstream.New())

	got, err := client.UserResource(context.Background(), &http.Client{Timeout: 5 * time.Second}, acct)
	if err != nil {
		t.Fatalf("UserResource: %v", err)
	}
	if got.Size != 200 || got.Remain != 150 || got.Used != 50 {
		t.Errorf("UserResource = %+v, want 200/150/50", got)
	}
}

func TestIsAlreadyClaimedPositive(t *testing.T) {
	bodies := []string{
		`{"code":10001,"msg":"今日已签到"}`,
		`{"code":0,"msg":"Already checked in"}`,
		`{"code":1,"msg":"you have already claimed this reward"}`,
		`{"msg":"已领取过"}`,
		`{"msg":"请勿重复领取"}`,
		`{"msg":"不能重复"}`,
		`{"msg":"已领过"}`,
		`plain text: Already Claimed`,
		`{"msg":"今日已领"}`,
		`{"msg":"Already signed up today"}`,
		`{"msg":"you already obtained this"}`,
		// A duplicate report that also carries refusal wording. The negation
		// guard must not read the refusal half as "you still have a claim
		// coming", which is what a body-wide scan would do.
		`{"msg":"已签到，请勿重复领取"}`,
	}
	for _, body := range bodies {
		if !IsAlreadyClaimed([]byte(body)) {
			t.Errorf("IsAlreadyClaimed(%q) = false, want true", body)
		}
	}
}

// TestIsAlreadyClaimedIgnoresNegatedClaim is the reason the marker list is
// split in two: 已领取 is a substring of 可领取 ("claimable") and 未领取 ("not
// claimed"), and reading either of those as "already taken" would make the
// healer mark an account settled and stop topping up a grant it never got.
func TestIsAlreadyClaimedIgnoresNegatedClaim(t *testing.T) {
	bodies := []string{
		`{"msg":"可领取"}`,
		`{"msg":"未领取"}`,
		`{"msg":"红包未领取"}`,
		`{"msg":"尚未领取,请前往领取"}`,
	}
	for _, body := range bodies {
		if IsAlreadyClaimed([]byte(body)) {
			t.Errorf("IsAlreadyClaimed(%q) = true, want false", body)
		}
	}
}

func TestIsAlreadyClaimedNegative(t *testing.T) {
	bodies := []string{
		``,
		`{"code":0,"msg":"ok"}`,
		`{"code":400,"msg":"task not completed"}`,
		`{"code":500,"msg":"service busy"}`,
		`{"code":0,"msg":"领取成功"}`,
		`<html>502 Bad Gateway</html>`,
		`{"msg":"网络异常,请稍后重试"}`,
	}
	for _, body := range bodies {
		if IsAlreadyClaimed([]byte(body)) {
			t.Errorf("IsAlreadyClaimed(%q) = true, want false", body)
		}
	}
	if IsAlreadyClaimed(nil) {
		t.Error("IsAlreadyClaimed(nil) = true, want false")
	}
}

func TestIsAlreadyClaimedIsCaseInsensitive(t *testing.T) {
	for _, body := range []string{"ALREADY CLAIMED", "already claimed", "AlReAdY cLaImEd"} {
		if !IsAlreadyClaimed([]byte(body)) {
			t.Errorf("IsAlreadyClaimed(%q) = false, want true", body)
		}
	}
}

// fixedClock is a hand-driven clock: the interesting behaviour here is what
// happens hours later, and a suite that waits for it is a suite nobody runs.
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

func (c *fixedClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// Reset moves the clock back to a known instant so a sub-interval assertion can
// be made against the same origin the interval was measured from.
func (c *fixedClock) Reset(t time.Time) {
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
}

func baseTime() time.Time { return time.Date(2026, time.September, 15, 9, 0, 0, 0, time.UTC) }

func TestHealerAttemptsAFreshAccount(t *testing.T) {
	h := NewHealer(newClock(baseTime()).Now)
	if !h.ShouldAttempt("uid-a") {
		t.Error("ShouldAttempt on an untried account = false, want true")
	}
	if h.Settled("uid-a") {
		t.Error("Settled on an untried account = true, want false")
	}
	if _, ok := h.Last("uid-a"); ok {
		t.Error("Last on an untried account returned a value")
	}
}

func TestHealerRateLimitsAfterAFailure(t *testing.T) {
	clock := newClock(baseTime())
	h := NewHealer(clock.Now)

	h.NoteResult("uid-a", Outcome{Reason: "network down"})
	if h.ShouldAttempt("uid-a") {
		t.Fatal("ShouldAttempt right after a failure = true, want false")
	}
	clock.Advance(HealCooldown - time.Minute)
	if h.ShouldAttempt("uid-a") {
		t.Error("ShouldAttempt one minute before the cooldown = true, want false")
	}
	// The wait is bounded by the back-off, and the first failure is already
	// counted, so the account opens at 2x the base interval, not at the base.
	clock.Advance(time.Minute)
	if h.ShouldAttempt("uid-a") {
		t.Error("ShouldAttempt at the base cooldown after one failure = true, want false")
	}
	clock.Advance(HealCooldown)
	if !h.ShouldAttempt("uid-a") {
		t.Error("ShouldAttempt at the accounted cooldown = false, want true")
	}
}

func TestHealerBacksOffExponentiallyOnRepeatedFailures(t *testing.T) {
	// The measured ladder: the wait doubles per consecutive failure, so one
	// failure waits 12h, two wait 24h (the cap) and three hold at the cap. Each
	// case asserts the boundary from both sides.
	cases := []struct {
		failures int
		wait     time.Duration
	}{
		{1, 2 * HealCooldown},
		{2, 4 * HealCooldown},
		{3, 24 * time.Hour},
	}
	for _, c := range cases {
		clock := newClock(baseTime())
		h := NewHealer(clock.Now)
		for i := 0; i < c.failures; i++ {
			h.NoteResult("uid-a", Outcome{Reason: "nope"})
		}
		if got := h.cooldownLocked("uid-a"); got != c.wait {
			t.Fatalf("cooldownLocked after %d failures = %v, want %v", c.failures, got, c.wait)
		}
		clock.Advance(c.wait)
		if !h.ShouldAttempt("uid-a") {
			t.Errorf("ShouldAttempt at the %d-failure cooldown (%v) = false, want true", c.failures, c.wait)
		}
		clock.Reset(baseTime())
		clock.Advance(c.wait - time.Nanosecond)
		if h.ShouldAttempt("uid-a") {
			t.Errorf("ShouldAttempt a nanosecond before the %d-failure cooldown = true, want false", c.failures)
		}
	}

	// The back-off is capped at a day, not unbounded.
	clock := newClock(baseTime())
	h := NewHealer(clock.Now)
	for i := 0; i < 20; i++ {
		h.NoteResult("uid-b", Outcome{Reason: "nope"})
	}
	if got := h.cooldownLocked("uid-b"); got != 24*time.Hour {
		t.Errorf("cooldownLocked after 20 failures = %v, want the 24h cap", got)
	}
	clock.Advance(24 * time.Hour)
	if !h.ShouldAttempt("uid-b") {
		t.Error("ShouldAttempt after a day with many failures = false, want true")
	}
}

func TestHealerStopsAttemptingASettledAccount(t *testing.T) {
	clock := newClock(baseTime())
	h := NewHealer(clock.Now)

	h.NoteResult("uid-a", Outcome{Healed: true, Reason: "claimed"})
	if h.ShouldAttempt("uid-a") {
		t.Error("ShouldAttempt on a healed account = true, want false")
	}
	if !h.Settled("uid-a") {
		t.Error("Settled on a healed account = false, want true")
	}

	h.NoteResult("uid-b", Outcome{Healed: true, AlreadyClaimed: true, Reason: "already claimed"})
	if !h.Settled("uid-b") {
		t.Error("Settled on an already-claimed account = false, want true")
	}
	// "Already claimed" is not a failure, so it must never expire into a retry.
	clock.Advance(365 * 24 * time.Hour)
	if h.ShouldAttempt("uid-b") {
		t.Error("an already-claimed account became retryable")
	}
}

func TestHealerForgetsOnDemand(t *testing.T) {
	clock := newClock(baseTime())
	h := NewHealer(clock.Now)

	h.NoteResult("uid-a", Outcome{Healed: true, Reason: "claimed"})
	h.Forget("uid-a")
	if !h.ShouldAttempt("uid-a") {
		t.Error("ShouldAttempt after Forget = false, want true")
	}
	if h.Settled("uid-a") {
		t.Error("Settled after Forget = true, want false")
	}
	if _, ok := h.Last("uid-a"); ok {
		t.Error("Last after Forget returned a value")
	}
}

func TestHealerResetsTheBackOffAfterASuccess(t *testing.T) {
	clock := newClock(baseTime())
	h := NewHealer(clock.Now)

	// Two failures put the account on a 12h cooldown.
	h.NoteResult("uid-a", Outcome{Reason: "failed once"})
	h.NoteResult("uid-a", Outcome{Reason: "failed twice"})
	clock.Advance(HealCooldown)
	if h.ShouldAttempt("uid-a") {
		t.Fatal("the account was released before its doubled cooldown")
	}

	// A heal clears the failure history, so the *next* failure starts from the
	// base interval again rather than inheriting the stretched one.
	h.NoteResult("uid-a", Outcome{Healed: true})
	h.Forget("uid-a")

	h.NoteResult("uid-a", Outcome{Reason: "failed again"})
	clock.Advance(2 * HealCooldown)
	if !h.ShouldAttempt("uid-a") {
		t.Error("a fresh failure after a heal inherited the old back-off")
	}
}

func TestHealerFillsInTheTimestamp(t *testing.T) {
	clock := newClock(baseTime())
	h := NewHealer(clock.Now)
	h.NoteResult("uid-a", Outcome{Reason: "no time supplied"})

	got, ok := h.Last("uid-a")
	if !ok {
		t.Fatal("Last returned no outcome")
	}
	if !got.At.Equal(baseTime()) {
		t.Errorf("recorded At = %v, want the injected clock's %v", got.At, baseTime())
	}
}

func TestHealerHandlesBlankUIDs(t *testing.T) {
	h := NewHealer(nil)
	if !h.ShouldAttempt("") {
		t.Error("ShouldAttempt(\"\") = false; an unidentified account must not be dropped")
	}
	// Recording for a blank uid is a no-op rather than a bucket no real
	// account can ever clear.
	h.NoteResult("", Outcome{Reason: "x"})
	if h.Settled("") {
		t.Error("a blank uid became settled")
	}
	if !h.ShouldAttempt("   ") {
		t.Error("ShouldAttempt(whitespace) = false, want true")
	}
}

func TestHealerNilClockUsesTheWallClock(t *testing.T) {
	h := NewHealer(nil)
	h.NoteResult("uid-a", Outcome{Reason: "x"})
	if h.ShouldAttempt("uid-a") {
		t.Error("ShouldAttempt immediately after a failure = true, want false")
	}
}

func TestHealOnceRecordsTheOutcome(t *testing.T) {
	g := newGateway(t)
	g.reply["POST /billing/ide/trial"] = `{"code":0,"msg":"ok"}`
	acct := testAccount(t, g, config.RealmCN)
	h := NewHealer(newClock(baseTime()).Now)
	client := New(upstream.New())

	outcome := h.HealOnce(context.Background(), client, &http.Client{Timeout: 5 * time.Second}, acct)
	if !outcome.Healed {
		t.Errorf("HealOnce on a clean claim = %+v, want Healed", outcome)
	}
	if !h.Settled(acct.UID) {
		t.Error("the healer did not record the successful heal")
	}
	if h.ShouldAttempt(acct.UID) {
		t.Error("ShouldAttempt after a successful heal = true, want false")
	}
}

func TestHealOnceRecordsAnAlreadyClaimedGrant(t *testing.T) {
	g := newGateway(t)
	g.reply["POST /billing/ide/trial"] = `{"code":10001,"msg":"Already claimed"}`
	acct := testAccount(t, g, config.RealmCN)
	h := NewHealer(newClock(baseTime()).Now)
	client := New(upstream.New())

	outcome := h.HealOnce(context.Background(), client, &http.Client{Timeout: 5 * time.Second}, acct)
	if !outcome.Healed || !outcome.AlreadyClaimed {
		t.Errorf("HealOnce on an already-claimed grant = %+v, want healed and already-claimed", outcome)
	}
	if !h.Settled(acct.UID) {
		t.Error("an already-claimed grant was not recorded as settled")
	}
	if g.count("POST /billing/ide/trial") != 1 {
		t.Errorf("HealOnce sent %d requests, want exactly 1", g.count("POST /billing/ide/trial"))
	}
}

func TestHealOnceRecordsATransportFailure(t *testing.T) {
	g := newGateway(t)
	g.status["POST /billing/ide/trial"] = http.StatusInternalServerError
	g.reply["POST /billing/ide/trial"] = `{"code":500,"msg":"busy"}`
	acct := testAccount(t, g, config.RealmCN)
	h := NewHealer(newClock(baseTime()).Now)
	client := New(upstream.New())

	outcome := h.HealOnce(context.Background(), client, &http.Client{Timeout: 5 * time.Second}, acct)
	if outcome.Healed {
		t.Errorf("HealOnce on a 500 = %+v, want not healed", outcome)
	}
	if outcome.Reason == "" {
		t.Error("a failed heal recorded no reason")
	}
	if h.Settled(acct.UID) {
		t.Error("a failed heal was recorded as settled")
	}
	if h.ShouldAttempt(acct.UID) {
		t.Error("ShouldAttempt right after a failed heal = true, want false")
	}
}

func TestHealOnceWithoutAnAccount(t *testing.T) {
	h := NewHealer(newClock(baseTime()).Now)
	outcome := h.HealOnce(context.Background(), New(nil), &http.Client{}, nil)
	if outcome.Healed {
		t.Errorf("HealOnce(nil account) = %+v, want not healed", outcome)
	}
	if outcome.Reason == "" {
		t.Error("HealOnce(nil account) gave no reason")
	}
}

func TestTrimFloatAndHelpers(t *testing.T) {
	if got := trimFloat(7); got != "7" {
		t.Errorf("trimFloat(7) = %q, want \"7\"", got)
	}
	if got := trimFloat(7.5); got != "7.5" {
		t.Errorf("trimFloat(7.5) = %q, want \"7.5\"", got)
	}
	if got := preview([]byte("  \n ")); got != "(empty body)" {
		t.Errorf("preview(blank) = %q, want \"(empty body)\"", got)
	}
	if got := preview([]byte("a\nb")); got != "a b" {
		t.Errorf("preview collapsed = %q, want \"a b\"", got)
	}
	long := strings.Repeat("x", 500)
	if got := preview([]byte(long)); len(got) != 200 {
		t.Errorf("preview(long) length = %d, want 200", len(got))
	}
	if got := firstNonEmpty("", "  ", "v"); got != "v" {
		t.Errorf("firstNonEmpty = %q, want \"v\"", got)
	}
	if got := dataObject(map[string]any{"data": "nope"}); len(got) != 0 {
		t.Errorf("dataObject(non-object) = %v, want empty", got)
	}
	if got := envelopeError(map[string]any{"code": float64(0)}); got != nil {
		t.Errorf("envelopeError(code=0) = %v, want nil", got)
	}
}

func TestOutcomeSettled(t *testing.T) {
	if (Outcome{}).settled() {
		t.Error("an empty outcome is settled")
	}
	if !(Outcome{Healed: true}).settled() {
		t.Error("a healed outcome is not settled")
	}
	if !(Outcome{AlreadyClaimed: true}).settled() {
		t.Error("an already-claimed outcome is not settled")
	}
}

// decode parses a JSON object written inline in a test.
func decode(t *testing.T, body string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("test fixture is not valid JSON: %v", err)
	}
	return out
}

// contains is strings.Contains, spelled out so the assertion lines read the
// same way as the growth suite's.
func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
