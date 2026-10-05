// Package pool picks which account serves a request.
//
// The gateway is a load balancer in front of several upstream identities. The
// selection is not round-robin: an account that just hit a rate limit should
// stay out of rotation for a while, one that was disabled by an operator must
// never be picked, and a request that names a model must land on an account
// that can actually serve it.
//
// Ported from the AccountPool half of the Python reference (wb_proxy.py).
package pool

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"wb2hub/internal/auth"
)

// Cooldown reasons. They are kept apart because they expire on different
// clocks: a soft rate limit clears in seconds, an exhausted credit plan only
// clears at the next billing day.
type CoolKind string

const (
	CoolNone        CoolKind = ""
	CoolRateLimit   CoolKind = "rate_limit"
	CoolCredits     CoolKind = "credits"
	CoolError       CoolKind = "error"
	CoolSessionDead CoolKind = "session_dead"
	CoolManual      CoolKind = "manual"
)

// String renders a cooldown reason for the panel.
func (k CoolKind) String() string { return string(k) }

// Entry is one account plus its runtime state.
type Entry struct {
	Account *auth.Account

	// CoolUntil is when the account may serve again (unix nanos, 0 = ready).
	CoolUntil int64
	CoolKind  CoolKind

	// ModelCoolUntil blocks one (account, model) pair only. This is what makes
	// a per-model quota usable: the model that exhausted its budget drops out
	// while the account's other models keep working.
	ModelCoolUntil map[string]int64

	InFlight  int
	LastUsed  int64
	LastError string
	Successes int64
	Failures  int64
	Weight    int
	Disabled  bool
}

// Ready reports whether the entry may serve now.
func (e *Entry) Ready(now time.Time, model string) bool {
	if e.Disabled || e.Account == nil || !e.Account.IsEnabled() {
		return false
	}
	n := now.UnixNano()
	if e.CoolUntil > n {
		return false
	}
	if model != "" {
		if until, ok := e.ModelCoolUntil[model]; ok && until > n {
			return false
		}
	}
	return true
}

// CooledReason explains why an entry is unavailable, or CoolNone.
func (e *Entry) CooledReason(now time.Time, model string) CoolKind {
	n := now.UnixNano()
	if e.CoolUntil > n {
		return e.CoolKind
	}
	if model != "" {
		if until, ok := e.ModelCoolUntil[model]; ok && until > n {
			return CoolRateLimit
		}
	}
	return CoolNone
}

// Pool owns the entries and serialises access to them.
type Pool struct {
	mu      sync.RWMutex
	entries map[string]*Entry
	order   []string

	// statePath persists cooldowns across restarts so a rate-limited account is
	// not immediately retried just because the process bounced.
	statePath string
	dirty     bool
}

// New creates an empty pool that persists state to statePath.
func New(statePath string) *Pool {
	return &Pool{
		entries:   map[string]*Entry{},
		statePath: statePath,
	}
}

// Restore reads persisted cooldowns and re-attaches them to loaded accounts.
func (p *Pool) Restore(accounts []*auth.Account) []error {
	p.mu.Lock()
	defer p.mu.Unlock()

	saved := map[string]persistedEntry{}
	if p.statePath != "" {
		if raw, err := os.ReadFile(p.statePath); err == nil {
			var doc persistedDoc
			if err := json.Unmarshal(raw, &doc); err == nil {
				saved = doc.Entries
			}
		}
	}

	p.entries = make(map[string]*Entry, len(accounts))
	p.order = p.order[:0]
	for _, acct := range accounts {
		entry := &Entry{Account: acct, ModelCoolUntil: map[string]int64{}}
		if prev, ok := saved[acct.UID]; ok {
			entry.CoolUntil = prev.CoolUntil
			entry.CoolKind = CoolKind(prev.CoolKind)
			entry.Weight = prev.Weight
			entry.ModelCoolUntil = prev.ModelCoolUntil
			if entry.ModelCoolUntil == nil {
				entry.ModelCoolUntil = map[string]int64{}
			}
		}
		if entry.Weight == 0 {
			entry.Weight = 1
		}
		p.entries[acct.UID] = entry
		p.order = append(p.order, acct.UID)
	}
	sort.Strings(p.order)
	return nil
}

// List returns a snapshot of every entry, for the panel and the JSON API.
func (p *Pool) List() []Entry {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]Entry, 0, len(p.order))
	for _, uid := range p.order {
		if e, ok := p.entries[uid]; ok {
			out = append(out, *e)
		}
	}
	return out
}

// EntryView is a shallow copy of an Entry that callers outside this package can
// hold without racing the pool. The Account pointer is shared on purpose — it
// carries its own lock, and handing out a copy would silently detach a caller's
// mutations from the live account.
type EntryView struct {
	Account   *auth.Account
	CoolUntil int64
	CoolKind  CoolKind
	InFlight  int
	LastUsed  int64
	LastError string
	Successes int64
	Failures  int64
	Weight    int
	Disabled  bool
}

// viewOf copies an entry into the form callers outside this package may hold.
func viewOf(e *Entry) EntryView {
	return EntryView{
		Account:   e.Account,
		CoolUntil: e.CoolUntil,
		CoolKind:  e.CoolKind,
		InFlight:  e.InFlight,
		LastUsed:  e.LastUsed,
		LastError: e.LastError,
		Successes: e.Successes,
		Failures:  e.Failures,
		Weight:    e.Weight,
		Disabled:  e.Disabled,
	}
}

// View returns a snapshot of one entry by uid.
func (p *Pool) View(uid string) (EntryView, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.entries[uid]
	if !ok {
		return EntryView{}, false
	}
	return viewOf(e), true
}

// Views returns every entry as a copyable view.
func (p *Pool) Views() []EntryView {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]EntryView, 0, len(p.order))
	for _, uid := range p.order {
		e, ok := p.entries[uid]
		if !ok {
			continue
		}
		out = append(out, viewOf(e))
	}
	return out
}

// Counts reports (total, enabled, ready).
func (p *Pool) Counts(now time.Time) (total, enabled, ready int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, e := range p.entries {
		total++
		if e.Account != nil && e.Account.IsEnabled() && !e.Disabled {
			enabled++
			if e.Ready(now, "") {
				ready++
			}
		}
	}
	return
}

// Add registers a new account.
func (p *Pool) Add(acct *auth.Account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.entries[acct.UID]; exists {
		return
	}
	p.entries[acct.UID] = &Entry{Account: acct, Weight: 1, ModelCoolUntil: map[string]int64{}}
	p.order = append(p.order, acct.UID)
	sort.Strings(p.order)
	p.dirty = true
}

// Remove drops an account.
func (p *Pool) Remove(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.entries, uid)
	out := p.order[:0]
	for _, id := range p.order {
		if id != uid {
			out = append(out, id)
		}
	}
	p.order = out
	p.dirty = true
}

// Pick chooses an account for a model, skipping cooled and disabled ones.
//
// Among the usable entries the one idle longest wins, which spreads load
// without needing a counter that has to be kept in sync with reality.
func (p *Pool) Pick(now time.Time, model string, accountOf func(string) string, minGap time.Duration) (EntryView, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var best *Entry
	var bestKey int64
	for _, uid := range p.order {
		entry := p.entries[uid]
		if entry == nil || !entry.Ready(now, model) {
			continue
		}
		// Respect a per-key pin: when a key is bound to an account, only that
		// account may serve it.
		if accountOf != nil {
			if want := accountOf(uid); want != "" && want != uid {
				continue
			}
		}
		key := entry.LastUsed
		if entry.LastUsed == 0 {
			key = -1 // never used sorts first
		}
		if best == nil || key < bestKey {
			best, bestKey = entry, key
		}
	}
	if best == nil {
		return EntryView{}, false
	}

	// A minimum gap stops one healthy account from being hammered when the rest
	// are cooling: the caller is told to wait instead of silently overloading.
	if minGap > 0 && best.LastUsed != 0 &&
		now.Sub(time.Unix(0, best.LastUsed)) < minGap {
		return EntryView{}, false
	}
	return viewOf(best), true
}

// PickWeighted chooses among ready entries in proportion to weight.
func (p *Pool) PickWeighted(now time.Time, model string) (EntryView, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	total := 0
	for _, uid := range p.order {
		if e := p.entries[uid]; e != nil && e.Ready(now, model) {
			total += e.Weight
		}
	}
	if total <= 0 {
		return EntryView{}, false
	}

	// Deterministic sweep over the weighted ranges; a random draw would need a
	// source of randomness seeded per call and buys nothing here.
	target := int(now.UnixNano()%int64(total)) + 1
	acc := 0
	for _, uid := range p.order {
		e := p.entries[uid]
		if e == nil || !e.Ready(now, model) {
			continue
		}
		acc += e.Weight
		if acc >= target {
			return viewOf(e), true
		}
	}
	return EntryView{}, false
}

// Acquire marks an entry as in-flight and returns it.
func (p *Pool) Acquire(uid string, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[uid]; ok {
		e.InFlight++
		e.LastUsed = now.UnixNano()
	}
}

// Release clears the in-flight flag and records the outcome.
func (p *Pool) Release(uid string, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[uid]; ok {
		if e.InFlight > 0 {
			e.InFlight--
		}
		if ok {
			e.Successes++
		} else {
			e.Failures++
		}
	}
}

// Cool puts an account (or one of its models) out of rotation.
func (p *Pool) Cool(uid string, kind CoolKind, until time.Time, model string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[uid]
	if !ok {
		return
	}
	if model != "" {
		if e.ModelCoolUntil == nil {
			e.ModelCoolUntil = map[string]int64{}
		}
		e.ModelCoolUntil[model] = until.UnixNano()
	} else {
		e.CoolUntil = until.UnixNano()
		e.CoolKind = kind
	}
	p.dirty = true
}

// ClearCooling lifts every cooldown, for the panel's "clear cooling" action.
func (p *Pool) ClearCooling() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, e := range p.entries {
		if e.CoolUntil > 0 || len(e.ModelCoolUntil) > 0 {
			n++
		}
		e.CoolUntil = 0
		e.CoolKind = CoolNone
		e.ModelCoolUntil = map[string]int64{}
	}
	p.dirty = true
	return n
}

// SetDisabled toggles an account's manual disable flag.
func (p *Pool) SetDisabled(uid string, off bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[uid]
	if !ok {
		return false
	}
	e.Disabled = off
	p.dirty = true
	return true
}

// NoteError records a failure and cools the account once the threshold is hit.
func (p *Pool) NoteError(uid string, errText string, threshold int, cool time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[uid]
	if !ok {
		return
	}
	e.LastError = errText
	e.Failures++
	if threshold > 0 && e.Failures >= int64(threshold) {
		e.CoolUntil = time.Now().Add(cool).UnixNano()
		e.CoolKind = CoolError
		e.Failures = 0
		p.dirty = true
	}
}

// Flush persists cooldown state.
func (p *Pool) Flush() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.dirty || p.statePath == "" {
		return nil
	}
	doc := persistedDoc{Entries: map[string]persistedEntry{}}
	for uid, e := range p.entries {
		doc.Entries[uid] = persistedEntry{
			CoolUntil:      e.CoolUntil,
			CoolKind:       string(e.CoolKind),
			Weight:         e.Weight,
			ModelCoolUntil: e.ModelCoolUntil,
		}
	}
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := p.statePath + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, p.statePath); err != nil {
		return err
	}
	p.dirty = false
	return nil
}

// StartFlusher persists state on an interval until ctx is done.
func (p *Pool) StartFlusher(stop <-chan struct{}, every time.Duration) {
	if every <= 0 {
		every = 30 * time.Second
	}
	go func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				_ = p.Flush()
				return
			case <-ticker.C:
				_ = p.Flush()
			}
		}
	}()
}

type persistedEntry struct {
	CoolUntil      int64            `json:"cool_until"`
	CoolKind       string           `json:"cool_kind"`
	Weight         int              `json:"weight"`
	ModelCoolUntil map[string]int64 `json:"model_cool_until,omitempty"`
}

type persistedDoc struct {
	Entries map[string]persistedEntry `json:"entries"`
}

// ModelOf strips any vendor prefix from a model name so cooldowns apply to the
// same model regardless of how the client spelled it.
func ModelOf(model string) string {
	name := strings.TrimSpace(model)
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return strings.ToLower(name)
}
