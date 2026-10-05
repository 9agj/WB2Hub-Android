// Package trial claims the bonus credits a China-realm account is entitled to —
// the IDE trial grant, the daily check-in, the compensation payout and the
// gift — and decides when it is worth retrying a claim that came back
// "already claimed".
//
// The retry half is the reason this is a package and not four one-line request
// builders. The upstream is inconsistent about the trial grant: the same call
// that fails today succeeds tomorrow, and a claim that genuinely already
// happened is reported with the *same* envelope code as a transient failure in
// some builds. So the caller's loop is: try, read whether upstream said
// "already taken" or "not now", and back off per account instead of hammering.
//
// What the retry must not do is treat "already claimed" as a failure. That is
// the success-with-a-different-shape case, and the Healer records it as such so
// the scheduler stops retrying an account whose trial is genuinely banked.
//
// Everything here is gated on the realm: the international build has no
// check-in subsystem, so a call there is refused locally with ErrNotSupported
// rather than fired off to earn a guaranteed 404.
//
// Ported from the billing/check-in half of wb_accounts.py (Account.checkin,
// Account.fetch_credits) plus the trial-heal state machine named in the
// reverse-engineering notes (trialHealOutcome, tryHealTrialAccount,
// healTrialOnce, trialHealLast, trialAlreadyMarkers). Standard library only.
package trial

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"wb2hub/internal/auth"
	"wb2hub/internal/upstream"
)

// HealCooldown is how long one account is left alone after a heal attempt that
// did not succeed.
//
// It is measured in hours rather than minutes because the thing being retried
// is a *grant*, and the upstream's own grants are daily. Retrying sooner cannot
// find a state the upstream has not reached yet, and every attempt is visible to
// the upstream as another call.
const HealCooldown = 6 * time.Hour

// duplicateCode is the check-in envelope code the Python reference treats as
// success: 10001 means "already done today".
const duplicateCode = 10001

// ErrNotSupported reports that the account's realm has no check-in / trial
// subsystem at all.
//
// The trial grant is a China-build feature: the international build answers the
// same path with a 404 that reads like a deployment mistake. Returning a
// sentinel lets the scheduler say "国际版无此功能" rather than reporting an
// upstream fault the gateway caused itself.
var ErrNotSupported = errors.New("trial: not available on this realm")

// alreadyMarkers are the substrings that are candidates for "this grant has
// already been taken" rather than "this call failed".
//
// Two wordings are listed because the account's Accept-Language decides which
// one comes back: the English the billing host returns when it is asked for it,
// and the Chinese it returns otherwise. Testing only one would make the
// healer's behaviour depend on the account's locale.
//
// The list holds candidates only. Which of them actually mean "taken" is
// decided by alreadyConfirmed plus the positional negation check in
// IsAlreadyClaimed, not by the word alone.
var alreadyMarkers = []string{
	"already claimed",
	"already checked in",
	"already checked-in",
	"already checkin",
	"already signed",
	"already received",
	"already obtained",
	"already got",
	"已签到",
	"今日已领",
	"已领取",
	"已领过",
	"重复领取",
	"不能重复",
}

// alreadyConfirmed lists the markers that are conclusive on their own: each of
// them can only appear in a sentence about a grant that was already taken.
//
// "已领取" is deliberately not among them. The plain reading is "already
// claimed" — the panel even uses that wording as a claimed-state label — but
// the phrase is not a complete claim about the past: Chinese leaves the past
// marker optional, so "可领取" ("claimable") and "未领取" ("not claimed") both
// contain it while saying the opposite. Treating those as already-claimed would
// make the healer mark an account settled and stop topping up a grant that is
// still available, which is the one outcome that loses money rather than a
// retry. "已领取过" carries the explicit past marker 过 and is conclusive.
var alreadyConfirmed = []string{
	"already claimed",
	"已领取过",
	"已领过",
	"重复领取",
	"不能重复",
}

// alreadyNegated are the modifiers that flip a candidate marker into its
// opposite: "未领取" is "not yet claimed", not "claimed".
var alreadyNegated = []string{
	"未", "没", "尚", "待", "可", "not ", "no ", "请勿",
}

// Client issues the billing/trial calls for one gateway.
//
// The http.Client is supplied per call, not stored: the egress route belongs to
// the account's proxy slot.
type Client struct {
	Up *upstream.Client
}

// New builds a trial client over an existing upstream transport.
func New(up *upstream.Client) *Client {
	if up == nil {
		up = upstream.New()
	}
	return &Client{Up: up}
}

// Claim redeems the IDE trial grant.
//
// "already claimed" is reported through Result.AlreadyClaimed rather than as an
// error: for the caller the two outcomes are the same money, and only the
// healer's retry policy needs to tell them apart.
func (c *Client) Claim(ctx context.Context, client *http.Client, acct *auth.Account) (Result, error) {
	return c.post(ctx, client, acct, "/billing/ide/trial")
}

// Checkin performs the daily check-in.
//
// The upstream answers a duplicate check-in with envelope code 10001 rather
// than 0, which the Python reference treats as success — the day is banked
// either way. That code is surfaced as Result.AlreadyClaimed so a caller that
// wants to distinguish "checked in just now" from "was already checked in" can,
// without treating the second one as a failure.
func (c *Client) Checkin(ctx context.Context, client *http.Client, acct *auth.Account) (Result, error) {
	return c.post(ctx, client, acct, "/v2/billing/meter/daily-checkin")
}

// ClaimCompensation redeems the outage/compensation payout.
func (c *Client) ClaimCompensation(ctx context.Context, client *http.Client, acct *auth.Account) (Result, error) {
	return c.post(ctx, client, acct, "/v2/billing/meter/claim-compensation")
}

// ClaimGift redeems the standing gift.
func (c *Client) ClaimGift(ctx context.Context, client *http.Client, acct *auth.Account) (Result, error) {
	return c.post(ctx, client, acct, "/v2/billing/meter/claim-gift")
}

// UserResource fetches the account's package balances.
//
// The request is a POST with a paging body even though the endpoint reads like
// a GET; upstream accepts the GET but answers it with an empty package list,
// which silently reads as "no credits left". The body is kept identical to the
// desktop client's for that reason.
func (c *Client) UserResource(ctx context.Context, client *http.Client, acct *auth.Account) (Resource, error) {
	if err := c.gate(acct); err != nil {
		return Resource{}, err
	}
	now := time.Now()
	payload := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   "2036-01-01 00:00:00",
	}

	decoded, raw, status, err := c.Up.DoJSON(ctx, client, http.MethodPost,
		acct.RealmConfig().BillingUpstream+"/v2/billing/meter/get-user-resource",
		upstream.Headers(acct, "billing"), payload)
	if err != nil {
		return Resource{}, fmt.Errorf("trial: user resource: %w", err)
	}
	if decoded == nil || len(decoded) == 0 {
		return Resource{}, fmt.Errorf("trial: user resource: HTTP %d: %s", status, preview(raw))
	}
	if err := envelopeError(decoded); err != nil {
		return Resource{}, fmt.Errorf("trial: user resource: %w", err)
	}
	if status != http.StatusOK {
		return Resource{}, fmt.Errorf("trial: user resource: HTTP %d: %s", status, preview(raw))
	}
	return ParseResource(decoded), nil
}

// Result is the outcome of one claim call.
//
// OK and AlreadyClaimed are separate so a caller rendering the panel can say
// "just now" or "was already done" without parsing text, and so the healer's
// bookkeeping does not have to re-derive it from a message.
type Result struct {
	OK             bool           `json:"ok"`
	AlreadyClaimed bool           `json:"already_claimed"`
	Code           int            `json:"code"`
	Message        string         `json:"message"`
	Data           map[string]any `json:"data,omitempty"`
}

// Resource is the account's balance summary.
//
// The totals are summed here rather than left to the caller because the
// upstream reports one entry per purchased package and the panel wants the
// account-wide number; per-package detail is kept alongside it so the operator
// can still see which package is running out.
type Resource struct {
	Remain   int64            `json:"remain"`
	Used     int64            `json:"used"`
	Size     int64            `json:"size"`
	Packages []PackageBalance `json:"packages"`
	Fetched  time.Time        `json:"fetched_at"`
}

// PackageBalance is one purchased package's balance.
type PackageBalance struct {
	Name   string `json:"name"`
	Remain int64  `json:"remain"`
	Used   int64  `json:"used"`
	Size   int64  `json:"size"`
}

// ParseResource reads the balance block out of a get-user-resource reply.
//
// The response nests three levels deep (data.Response.Data) and reports two
// different capacity schemes: packages with a resetting cycle report
// CycleCapacity*, everything else reports Capacity*. A cycle package reports
// its own CycleCapacityUsed inconsistently — sometimes larger than the
// arithmetic remainder implies — so the larger of the two is taken. That is the
// Python reference's behaviour and it is the conservative reading, since it
// never claims more remaining credit than upstream's own numbers support.
//
// A reply with no packages returns a zero Resource and no error: "no packages"
// is a real answer for a fresh account, not a parse failure.
func ParseResource(decoded map[string]any) Resource {
	out := Resource{Fetched: time.Now()}
	accounts := nestedList(decoded, "data", "Response", "Data", "Accounts")
	for _, acct := range accounts {
		pkg := PackageBalance{Name: firstNonEmpty(stringField(acct, "PackageName"), "Package")}
		if size := int64Field(acct, "CycleCapacitySize"); size > 0 {
			pkg.Size = size
			pkg.Remain = int64Field(acct, "CycleCapacityRemain")
			pkg.Used = size - pkg.Remain
			if pkg.Used < 0 {
				pkg.Used = 0
			}
			if used := int64Field(acct, "CycleCapacityUsed"); used > pkg.Used {
				pkg.Used = used
				pkg.Remain = size - used
				if pkg.Remain < 0 {
					pkg.Remain = 0
				}
			}
		} else {
			pkg.Remain = int64Field(acct, "CapacityRemain")
			pkg.Used = int64Field(acct, "CapacityUsed")
			pkg.Size = int64Field(acct, "CapacitySize")
		}
		out.Remain += pkg.Remain
		out.Used += pkg.Used
		out.Size += pkg.Size
		out.Packages = append(out.Packages, pkg)
	}
	return out
}

// post issues one gated billing POST with an empty body.
func (c *Client) post(ctx context.Context, client *http.Client, acct *auth.Account, path string) (Result, error) {
	if err := c.gate(acct); err != nil {
		return Result{}, err
	}

	decoded, raw, status, err := c.Up.DoJSON(ctx, client, http.MethodPost,
		acct.RealmConfig().BillingUpstream+path, upstream.Headers(acct, "billing"), map[string]any{})
	if err != nil {
		return Result{}, fmt.Errorf("trial: %s: %w", path, err)
	}

	result := Result{Data: dataObject(decoded)}
	result.Code = intField(decoded, "code")
	result.Message = firstNonEmpty(stringField(decoded, "msg"), stringField(decoded, "message"))

	// A non-200 still carries a usable verdict when the body is a JSON
	// envelope: the billing host answers 400 with {"code":10001,...} for a
	// duplicate check-in, which is a success the transport layer calls a
	// failure.
	if len(result.Data) > 0 || result.Code != 0 || strings.TrimSpace(result.Message) != "" {
		result.AlreadyClaimed = result.Code == duplicateCode || IsAlreadyClaimed(raw)
		result.OK = status == http.StatusOK && (result.Code == 0 || result.AlreadyClaimed)
		if result.OK {
			if result.Message == "" {
				result.Message = "ok"
			}
			return result, nil
		}
	}

	if status != http.StatusOK {
		return result, fmt.Errorf("trial: %s: HTTP %d: %s", path, status, preview(raw))
	}
	if err := envelopeError(decoded); err != nil {
		return result, fmt.Errorf("trial: %s: %w", path, err)
	}
	return result, nil
}

// gate refuses a realm with no check-in subsystem.
func (c *Client) gate(acct *auth.Account) error {
	if acct == nil {
		return fmt.Errorf("%w: no account", ErrNotSupported)
	}
	if !acct.RealmConfig().HasCheckin {
		return fmt.Errorf("%w: realm %s", ErrNotSupported, acct.RealmID())
	}
	return nil
}

// Outcome is what one heal attempt produced.
//
// It is recorded, not just returned, so the healer can rate-limit per account
// from the same value the panel displays.
type Outcome struct {
	Healed         bool      `json:"healed"`
	AlreadyClaimed bool      `json:"already_claimed"`
	Reason         string    `json:"reason"`
	At             time.Time `json:"at"`
}

// settled reports whether this outcome ends the retry loop.
//
// Both a successful heal and an "already claimed" answer end it: in the second
// case there is nothing left to heal, and continuing to retry is what the
// upstream eventually answers with a rate limit.
func (o Outcome) settled() bool { return o.Healed || o.AlreadyClaimed }

// Healer rate-limits trial-heal attempts per account.
//
// It is deliberately process-local and in-memory: the state is a retry back-off,
// and a restart losing it costs one extra attempt, whereas persisting it would
// add a file format to migrate for no benefit.
//
// The zero value is not usable; NewHealer builds one. now may be nil, in which
// case the wall clock is used.
type Healer struct {
	now func() time.Time

	mu     sync.Mutex
	last   map[string]Outcome
	failed map[string]int
}

// NewHealer builds a healer over the supplied clock.
func NewHealer(now func() time.Time) *Healer {
	if now == nil {
		now = time.Now
	}
	return &Healer{
		now:    now,
		last:   make(map[string]Outcome),
		failed: make(map[string]int),
	}
}

// ShouldAttempt reports whether uid should be tried now.
//
// An account that has never been tried is always a candidate. After an attempt
// that settled, it is done forever as far as this process is concerned — the
// grant is not going to appear again today. After an attempt that did not
// settle, the account is left alone for cooldownLocked, which doubles per
// consecutive failure so a permanently broken account backs off to a daily poke
// instead of a six-hourly one.
//
// A nil healer is treated as "no policy": every account is a candidate. That
// keeps a caller that has not wired the healer up yet from silently skipping
// every account, which would look exactly like a working healer on a healthy
// pool.
func (h *Healer) ShouldAttempt(uid string) bool {
	if h == nil {
		return true
	}
	id := strings.TrimSpace(uid)
	if id == "" {
		// No identity means no per-account policy: attempting is the only
		// answer that does not silently drop the account.
		return true
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	last, ok := h.last[id]
	if !ok {
		return true
	}
	if last.settled() {
		return false
	}
	return !h.now().Before(last.At.Add(h.cooldownLocked(id)))
}

// cooldownLocked is the current back-off for an account: HealCooldown doubled
// once per consecutive failure, capped at a day.
//
// The doubling is counted from the failure count as recorded, so one failure
// waits 12h rather than the base 6h — the increment in NoteResult has already
// happened by the time this is read.
func (h *Healer) cooldownLocked(uid string) time.Duration {
	wait := HealCooldown
	for i := 0; i < h.failed[uid]; i++ {
		wait *= 2
		if wait >= 24*time.Hour {
			return 24 * time.Hour
		}
	}
	if wait > 24*time.Hour {
		wait = 24 * time.Hour
	}
	return wait
}

// NoteResult records the outcome of one heal attempt for uid.
//
// The timestamp is filled in when the caller left it zero, so a caller that
// only knows the verdict cannot accidentally record an attempt at the zero time
// and make the account look like it is due immediately.
func (h *Healer) NoteResult(uid string, outcome Outcome) {
	id := strings.TrimSpace(uid)
	if id == "" {
		return
	}
	if outcome.At.IsZero() {
		outcome.At = h.now()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.last[id] = outcome
	if outcome.settled() {
		// A settled account has no failure history worth remembering: if the
		// grant is revoked upstream later, the retry should start from the
		// base cooldown rather than a day.
		delete(h.failed, id)
		return
	}
	h.failed[id]++
}

// Last returns the recorded outcome for uid, if any.
func (h *Healer) Last(uid string) (Outcome, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	outcome, ok := h.last[strings.TrimSpace(uid)]
	return outcome, ok
}

// Forget drops all recorded state for uid, so the next ShouldAttempt is true.
//
// It exists for the panel's "retry now" button: an operator who has just
// re-authenticated an account should not have to wait out a back-off earned
// while the credentials were stale.
func (h *Healer) Forget(uid string) {
	id := strings.TrimSpace(uid)
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.last, id)
	delete(h.failed, id)
}

// Settled reports whether uid has been healed or found already claimed.
//
// The scheduler uses it to keep a settled account out of the daily cycle
// entirely, rather than calling ShouldAttempt and discarding the answer.
func (h *Healer) Settled(uid string) bool {
	outcome, ok := h.Last(uid)
	return ok && outcome.settled()
}

// HealOnce runs one heal attempt for an account and records it.
//
// It is the healTrialOnce equivalent: one claim, one verdict, one note.
// Splitting the verdict out of the caller's loop is what makes the policy
// testable — the claim is transport, the decision here is state.
func (h *Healer) HealOnce(ctx context.Context, c *Client, client *http.Client, acct *auth.Account) Outcome {
	if acct == nil {
		return Outcome{Reason: "no account", At: h.now()}
	}
	result, err := c.Claim(ctx, client, acct)
	outcome := Outcome{At: h.now()}
	switch {
	case err != nil:
		outcome.Reason = err.Error()
	case result.AlreadyClaimed:
		outcome.Healed = true
		outcome.AlreadyClaimed = true
		outcome.Reason = firstNonEmpty(result.Message, "already claimed")
	default:
		outcome.Healed = true
		outcome.Reason = firstNonEmpty(result.Message, "claimed")
	}
	h.NoteResult(acct.UID, outcome)
	return outcome
}

// IsAlreadyClaimed reports whether an upstream body says the grant was already
// taken.
//
// It is exported because the same marker appears on the chat-side endpoints
// this package does not own, and a second copy of the list would drift from
// this one. A body that is not JSON is scanned too: several of these endpoints
// answer a refusal as plain text, and the status line is not where the
// distinction lives.
func IsAlreadyClaimed(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	lowered := strings.ToLower(string(body))

	// A conclusive marker settles it outright, and is checked first on purpose:
	// a conclusive phrase can sit next to a negated one — "已签到，请勿重复"
	// says both "already checked in" and "do not repeat" — and the duplicate
	// reading is the correct one there.
	for _, marker := range alreadyConfirmed {
		if strings.Contains(lowered, strings.ToLower(marker)) {
			return true
		}
	}

	for _, marker := range alreadyMarkers {
		idx := strings.Index(lowered, strings.ToLower(marker))
		if idx < 0 {
			continue
		}
		if negatedAt(lowered, idx) {
			continue
		}
		return true
	}
	return false
}

// negatedAt reports whether a negation governs the marker that starts at idx.
//
// The check is positional on purpose. A body-wide search for a word such as
// "请勿" reads "已签到，请勿重复" — a genuine duplicate — as a refusal, because
// the sentence contains both readings. What matters is whether the text
// immediately before the marker negates it: "未领取" and "可领取" both end with
// the marker 领取 but mean the opposite of a claim.
func negatedAt(text string, idx int) bool {
	if idx <= 0 {
		return false
	}
	before := text[:idx]
	for _, neg := range alreadyNegated {
		if strings.HasSuffix(before, strings.ToLower(neg)) {
			return true
		}
	}
	return false
}

// envelopeError turns an upstream business code into an error.
func envelopeError(decoded map[string]any) error {
	if decoded == nil {
		return fmt.Errorf("empty upstream response")
	}
	code := intField(decoded, "code")
	if code == 0 {
		return nil
	}
	msg := firstNonEmpty(stringField(decoded, "msg"), stringField(decoded, "message"))
	if msg == "" {
		msg = fmt.Sprintf("code=%d", code)
	}
	return fmt.Errorf("%s (code=%d)", msg, code)
}

// preview renders the first part of a raw body for an error message.
//
// It exists so a non-JSON refusal is not reported as a bare status code: the
// billing host answers some faults with an HTML error page, and the first line
// of it is the only clue the operator gets.
func preview(raw []byte) string {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return "(empty body)"
	}
	if len(text) > 200 {
		text = text[:200]
	}
	return strings.Join(strings.Fields(text), " ")
}

// dataObject returns the envelope's "data" object, or an empty map.
func dataObject(decoded map[string]any) map[string]any {
	if decoded == nil {
		return map[string]any{}
	}
	if data, ok := decoded["data"].(map[string]any); ok {
		return data
	}
	return map[string]any{}
}

// nestedList walks a chain of object fields and returns the array it finds.
//
// get-user-resource buries its package list under data.Response.Data.Accounts,
// which is a shape no single lookup reads cleanly. Any missing or mistyped link
// returns nil rather than an error, so a truncated reply degrades to "no
// packages" instead of failing the balance refresh.
func nestedList(decoded map[string]any, path ...string) []map[string]any {
	var node any = decoded
	for _, key := range path {
		object, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		node, ok = object[key]
		if !ok {
			return nil
		}
	}
	items, ok := node.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if row, ok := item.(map[string]any); ok {
			out = append(out, row)
		}
	}
	return out
}

// stringField reads a string, tolerating a numeric value encoded as a number.
func stringField(node map[string]any, field string) string {
	if node == nil {
		return ""
	}
	switch v := node[field].(type) {
	case string:
		return v
	case float64:
		return trimFloat(v)
	case fmt.Stringer:
		return v.String()
	default:
		return ""
	}
}

// intField reads an integer, tolerating a string or float encoding.
func intField(node map[string]any, field string) int {
	if node == nil {
		return 0
	}
	switch v := node[field].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case string:
		var parsed float64
		if err := json.Unmarshal([]byte(strings.TrimSpace(v)), &parsed); err == nil {
			return int(parsed)
		}
		return 0
	default:
		return 0
	}
}

// int64Field is intField for balances that can exceed an int32.
func int64Field(node map[string]any, field string) int64 {
	if node == nil {
		return 0
	}
	switch v := node[field].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case string:
		var parsed float64
		if err := json.Unmarshal([]byte(strings.TrimSpace(v)), &parsed); err == nil {
			return int64(parsed)
		}
		return 0
	default:
		return 0
	}
}

// trimFloat renders a float without a spurious fractional part.
func trimFloat(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%g", v)
}

// firstNonEmpty returns the first non-blank string.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
