// Package logring keeps the gateway's most recent log lines for the panel.
//
// The gateway logs to stderr, which on Android goes to logcat and is therefore
// invisible to the operator looking at the app's own screen. The panel needs to
// show the same lines, but it must not be the reason the process grows without
// bound: a phone runs for weeks, and an unbounded log buffer is a leak with a
// nice name.
//
// So this is a fixed-capacity ring. It implements io.Writer and is installed
// alongside stderr via log.SetOutput, which means every line the process already
// logs lands here without a single call site changing. The alternative —
// threading a logger through every package — would have meant touching code
// that has no other reason to know the panel exists.
//
// Two details make the ring usable rather than merely present:
//
//  1. Each line carries a monotonic Seq. The panel polls and asks only for what
//     it has not seen; comparing Seq is exact, while comparing text is not,
//     because the same line can legitimately repeat.
//  2. Each line is classified into a level and a kind. The panel filters by
//     kind ("show me only the scheduler") and colours by level, and doing that
//     at write time means the panel refresh does no parsing at all.
//
// Ported from workbuddy2api/internal/logring. Standard library only.
package logring

import (
	"bytes"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Line kinds. They mirror the subsystems that actually log, which is why the
// set is closed: a kind the panel has no filter for is a category of one.
const (
	KindChat      = "chat"
	KindCheckin   = "checkin"
	KindTravel    = "travel"
	KindPanel     = "panel"
	KindProxy     = "proxy"
	KindAuth      = "auth"
	KindScheduler = "scheduler"
	KindUpstream  = "upstream"
	KindOther     = "other"
)

// Levels, simplest first. The strings are what the panel renders, so they are
// stable values rather than free text.
const (
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// DefaultCapacity is the ring size used when a caller passes no capacity: about
// a day of normal operation, small enough to copy on every panel refresh.
const DefaultCapacity = 500

// maxLineBytes caps one stored line. A pathological upstream body can reach the
// log through an error string, and one such line must not push everything else
// out of the ring.
const maxLineBytes = 4096

// tsPrefixRe matches the timestamp Go's own log package writes in its default
// format ("2009/01/23 01:23:23 "), plus the bare time that the historic
// in-memory ring in internal/server prefixed its lines with ("15:04:05 ").
//
// Both are stripped before re-rendering: a line that already carries a
// timestamp and is then given another one renders as
// "01:23:23 2009/01/23 01:23:23 hello" in the panel.
var tsPrefixRe = regexp.MustCompile(
	`^\[?\d{4}[/-]\d{2}[/-]\d{2}[ T]\d{2}:\d{2}:\d{2}(?:\.\d+)?\]?[ \t]+|^\[?\d{2}:\d{2}:\d{2}(?:\.\d+)?\]?[ \t]+`)

// logDecorateRe strips the decorations Go's log package and this codebase's own
// conventions put in front of a message: a level keyword in brackets, an
// all-lowercase bracket tag, and a leading file:line pair.
//
// It exists so classification sees the message rather than the ornament; the
// text itself is still stored verbatim.
//
// The bracket alternative matches any lowercase word rather than a fixed list
// of level names, because this codebase tags lines with subsystem names too
// ("[start]", "[auth]", "[stop]") and a fixed list would leave those tags glued
// to the message — which is what makes "level" and "kind" readable off the same
// stripped text.
var logDecorateRe = regexp.MustCompile(
	`^\[[a-z][a-z0-9_-]*\][ \t]*|^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}[ \t]*|^[^\s:]+\.go:\d+:[ \t]*`)

// taskPrefixes are the single-word task labels the scheduler and growth
// subsystem put at the head of a line.
//
// Only genuine subsystem labels belong here. Words like "account" and "token"
// are the *subject* of a line, not its subsystem — "account 42 checkin -> HTTP
// 200" is a check-in event that happens to name an account — so they are left to
// kindKeywords, which the more specific entry wins.
var taskPrefixes = []struct {
	prefix string
	kind   string
}{
	{"scheduler", KindScheduler},
	{"调度器", KindScheduler},
	{"keepalive", KindScheduler},
	{"travel", KindTravel},
	{"旅行", KindTravel},
	{"buddy", KindTravel},
	{"cat-", KindTravel},
	{"cat:", KindTravel},
	{"checkin", KindCheckin},
	{"check-in", KindCheckin},
	{"签到", KindCheckin},
	{"lottery", KindCheckin},
	{"redeem", KindCheckin},
	{"makeup", KindCheckin},
	{"growth", KindCheckin},
	{"panel", KindPanel},
	{"admin", KindPanel},
	{"proxy", KindProxy},
	{"slot", KindProxy},
	{"auth", KindAuth},
	{"login", KindAuth},
	{"chat", KindChat},
	{"completion", KindChat},
	{"stream", KindChat},
	{"upstream", KindUpstream},
	{"transport", KindUpstream},
	{"tls", KindUpstream},
}

// kindKeywords match anywhere in the line, for the cases where the subsystem is
// named in the middle rather than as a prefix — "account 42 checkin -> HTTP 200"
// names both the account plane and the check-in subsystem.
//
// The order of this table decides ties, so the more specific subsystem wins over
// the one that merely happens to be mentioned: an account's check-in is a
// check-in, not an account event, because that is what the operator filtering
// for "checkin" is looking for.
var kindKeywords = []struct {
	needle string
	kind   string
}{
	{"travel", KindTravel},
	{"depart", KindTravel},
	{"buddy", KindTravel},
	{"black_cat", KindTravel},
	{"夜猫", KindTravel},
	{"checkin", KindCheckin},
	{"check-in", KindCheckin},
	{"签到", KindCheckin},
	{"lottery", KindCheckin},
	{"redeem", KindCheckin},
	{"makeup", KindCheckin},
	{"补签", KindCheckin},
	{"growth", KindCheckin},
	{"energy", KindCheckin},
	{"streak", KindCheckin},
	{"heatmap", KindCheckin},
	{"scheduler", KindScheduler},
	{"调度", KindScheduler},
	{"巡检", KindScheduler},
	// Chat is checked before the transport keywords because "upstream HTTP 502"
	// on a chat call is about the chat request, and that is the line the
	// operator came to the log for.
	{"chat", KindChat},
	{"completion", KindChat},
	{"tokens=", KindChat},
	{"proxy", KindProxy},
	{"slot", KindProxy},
	{"egress", KindProxy},
	{"discover", KindProxy},
	{"upstream", KindUpstream},
	{"api key", KindAuth},
	{"token", KindAuth},
	{"credential", KindAuth},
	{"refresh", KindAuth},
	{"realm", KindAuth},
	{"account", KindAuth},
	{"import", KindAuth},
	{"limits", KindPanel},
	{"restart", KindPanel},
	{"web tools", KindPanel},
	{"accounts=", KindPanel},
	{"auth_dir", KindPanel},
	{"listening on", KindPanel},
	{"wb2hub", KindPanel},
}

// levelKeywords map a substring to a level. Errors are checked before warnings
// so a line that reads "failed (retrying)" is an error, not a warning — the
// operator's eye is drawn to the worse of the two, which is the safe direction
// to be wrong in.
var levelKeywords = []struct {
	needle string
	level  string
}{
	{"fatal", LevelError},
	{"panic", LevelError},
	{"failed", LevelError},
	{"failure", LevelError},
	{"error", LevelError},
	{"err=", LevelError},
	{"✗", LevelError},
	{"拒绝", LevelError},
	{"失败", LevelError},
	{"异常", LevelError},
	{"warn", LevelWarn},
	{"retry", LevelWarn},
	{"retrying", LevelWarn},
	{"timeout", LevelWarn},
	{"cooling", LevelWarn},
	{"cooldown", LevelWarn},
	{"skipped", LevelWarn},
	{"skip ", LevelWarn},
	{"跳过", LevelWarn},
	{"警告", LevelWarn},
}

// knownTags are the tag-like brackets this codebase puts in front of a line
// that do NOT name a subsystem: the level tags and the lifecycle markers.
//
// They are stripped before the prefix table runs, because "start" is not a
// subsystem and leaving it attached would misfile the start-up banner. Every
// other tag is left alone so the prefix table can map it.
var knownTags = map[string]struct{}{
	"fatal": {}, "error": {}, "err": {}, "warn": {}, "warning": {},
	"info": {}, "debug": {}, "trace": {},
	"start": {}, "stop": {}, "boot": {},
}

// tagLevels reads severity off a bracket tag, before the tag is stripped.
//
// This is the only structured severity signal in the codebase: cmd/server uses
// "[fatal]" and "[warn]" prefixes, and Go's log package prints "fatal" for a
// Fatalf. Matching on the tag is exact where matching on the message is a
// guess.
var tagLevels = map[string]string{
	"fatal":   LevelError,
	"error":   LevelError,
	"err":     LevelError,
	"panic":   LevelError,
	"warn":    LevelWarn,
	"warning": LevelWarn,
	"info":    LevelInfo,
	"debug":   LevelInfo,
	"trace":   LevelInfo,
}

// tagKinds maps a subsystem tag to its kind, used when a line carries nothing
// but its tag.
var tagKinds = map[string]string{
	"auth":      KindAuth,
	"chat":      KindChat,
	"checkin":   KindCheckin,
	"travel":    KindTravel,
	"panel":     KindPanel,
	"proxy":     KindProxy,
	"scheduler": KindScheduler,
	"upstream":  KindUpstream,
	// Lifecycle markers are the gateway talking about itself, which is what the
	// panel filter is for.
	"start": KindPanel,
	"stop":  KindPanel,
	"boot":  KindPanel,
}

// Line is one stored log line.
//
// Seq is assigned under the same lock that appends, so it is dense: a consumer
// holding Seq N knows that everything up to N has been offered to it, and can
// ask for "strictly greater than N" without risking a gap.
type Line struct {
	Seq   uint64 `json:"seq"`
	At    string `json:"at"`    // local wall clock, HH:MM:SS
	Level string `json:"level"` // info / warn / error
	Kind  string `json:"kind"`  // chat / checkin / ... / other
	Text  string `json:"text"`
}

// clean is the line as the panel should render it, without the decorations the
// ring already accounts for.
func (l Line) clean() string {
	return strings.TrimSpace(stripDecorations(l.Text))
}

// Ring is a fixed-capacity, concurrency-safe store of the most recent lines.
//
// The zero value is not usable; call New. Every method is safe to call from
// several goroutines, including Write, which is called by the log package
// itself from whichever goroutine produced the message.
type Ring struct {
	mu   sync.RWMutex
	buf  []Line
	next int    // index the next line is written to
	size int    // how many slots are filled
	seq  uint64 // Seq of the next line to be written
	cap  int
	now  func() time.Time
}

// New creates a ring holding the most recent capacity lines.
//
// A capacity of zero or less means DefaultCapacity rather than an empty ring:
// a misconfigured buffer should degrade to the default, not silently discard
// everything the panel is meant to show.
func New(capacity int) *Ring {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	return &Ring{
		buf: make([]Line, capacity),
		cap: capacity,
		now: time.Now,
	}
}

// WithClock replaces the ring's clock. It exists for tests, where a rendered
// timestamp must be a fixed value rather than whatever the wall clock said.
func (r *Ring) WithClock(now func() time.Time) *Ring {
	if now != nil {
		r.mu.Lock()
		r.now = now
		r.mu.Unlock()
	}
	return r
}

// Write stores one log line and reports it as fully consumed.
//
// The signature is io.Writer's so the ring can be installed with
// log.SetOutput(io.MultiWriter(os.Stderr, ring)). It never returns an error:
// the log package treats a write error as a reason to print to stderr, and a
// buffer that is full by design has no failure to report.
//
// Embedded newlines are split, because log.Printf can emit several lines in one
// call and each one deserves its own Seq for the panel to filter separately.
func (r *Ring) Write(p []byte) (int, error) {
	n := len(p)
	if n == 0 {
		return 0, nil
	}
	body := p
	if len(body) > maxLineBytes {
		body = body[:maxLineBytes]
	}
	for _, raw := range bytes.Split(body, []byte{'\n'}) {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		r.push(string(raw))
	}
	return n, nil
}

// push classifies and stores one already-split line.
func (r *Ring) push(text string) {
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "\uFFFD")
	}
	if len(text) > maxLineBytes {
		text = text[:maxLineBytes]
	}

	// Classification is pure text work and is done outside the lock so a slow
	// line never blocks another writer; only the Seq stamp and the insertion
	// need to be atomic with each other.
	level := LevelOf(text)
	kind := KindOf(text)

	r.mu.Lock()
	clock := r.now
	if clock == nil {
		clock = time.Now
	}
	seq := r.seq
	r.seq++
	line := Line{
		Seq:   seq,
		At:    clock().Format("15:04:05"),
		Level: level,
		Kind:  kind,
		Text:  text,
	}
	r.buf[r.next] = line
	r.next = (r.next + 1) % r.cap
	if r.size < r.cap {
		r.size++
	}
	r.mu.Unlock()
}

// Len reports how many lines are currently stored, which is at most the
// capacity.
func (r *Ring) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.size
}

// Cap reports the ring's fixed capacity.
func (r *Ring) Cap() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cap
}

// Snapshot returns every stored line, oldest first.
//
// The ring is copied under the lock and rendered outside it, so a panel refresh
// never blocks a goroutine trying to log. The returned slice is a fresh copy:
// the panel may hold it while the ring keeps filling.
func (r *Ring) Snapshot() []Line {
	r.mu.RLock()
	out := make([]Line, 0, r.size)
	start := r.next - r.size
	if start < 0 {
		start += r.cap
	}
	for i := 0; i < r.size; i++ {
		line := r.buf[(start+i)%r.cap]
		line.Text = line.clean()
		out = append(out, line)
	}
	r.mu.RUnlock()
	return out
}

// SnapshotSince returns the lines written after seq, oldest first.
//
// A poller passes the highest Seq it has already rendered. A seq the ring has
// evicted simply yields whatever is still held: the panel shows the tail rather
// than a gap it can never fill.
func (r *Ring) SnapshotSince(seq uint64) []Line {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Line, 0, r.size)
	start := r.next - r.size
	if start < 0 {
		start += r.cap
	}
	for i := 0; i < r.size; i++ {
		line := r.buf[(start+i)%r.cap]
		if line.Seq < seq {
			continue
		}
		line.Text = line.clean()
		out = append(out, line)
	}
	return out
}

// String renders the ring as a newline-joined transcript, oldest first. It is
// what an operator sees after tapping "export log".
func (r *Ring) String() string {
	lines := r.Snapshot()
	var b strings.Builder
	for _, line := range lines {
		b.WriteString(line.At)
		b.WriteByte(' ')
		b.WriteString(line.Text)
		b.WriteByte('\n')
	}
	return b.String()
}

// CurrentSeq reports the Seq the next line will receive. A poller that has seen
// nothing yet passes it straight to SnapshotSince.
func (r *Ring) CurrentSeq() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.seq
}

// ------------------------------------------------------------- classification

// KindOf infers the subsystem a line came from.
//
// The prefix table runs first because a task label at the head of a line is a
// deliberate statement of what the line is about; keyword matching is the
// fallback for lines that name their subsystem in the middle. A line that
// matches nothing is "other" — an honest answer, since inventing a category for
// it would make the filter useless.
func KindOf(line string) string {
	tag := bracketTag(line)
	clean := stripDecorations(line)

	// A lifecycle marker is the gateway talking about itself, and its message
	// routinely names other subsystems ("[start] accounts=2" mentions
	// accounts, "[stop] wb2hub exited" mentions nothing). Letting the message
	// decide would file the start-up banner under auth, so the marker is
	// decisive here.
	if _, isMarker := knownTags[tag]; isMarker && tag != "" {
		if kind, ok := tagKinds[tag]; ok {
			return kind
		}
	}

	// A subsystem tag is left in front of the message so the prefix table
	// claims it.
	head := clean
	if tag != "" {
		head = tag + " " + clean
	}
	if head == "" {
		return KindOther
	}

	lower := strings.ToLower(head)
	for _, entry := range taskPrefixes {
		// A prefix only wins on a word boundary: "chatty" must not match "chat".
		if !strings.HasPrefix(lower, entry.prefix) {
			continue
		}
		if rest := lower[len(entry.prefix):]; rest == "" || rest[0] == ' ' ||
			rest[0] == ':' || rest[0] == '.' || rest[0] == '-' {
			return entry.kind
		}
	}

	// "[fatal] ..." and friends carry no subsystem, so they are classified by
	// what they are about below rather than by their tag.
	body := strings.ToLower(clean)
	for _, entry := range kindKeywords {
		if strings.Contains(body, strings.ToLower(entry.needle)) {
			return entry.kind
		}
	}
	return KindOther
}

// LevelOf infers a line's severity from its wording.
//
// This is text matching, so it is deliberately conservative: a line is only an
// error when it says so. The one structured signal available — a bracket tag
// such as "[fatal]" or "[warn]", which Go's log package and this codebase both
// use — is read off the tag itself rather than off the stripped message,
// because stripping is exactly what removes it.
func LevelOf(line string) string {
	tag := bracketTag(line)
	if level, ok := tagLevels[tag]; ok {
		return level
	}

	clean := strings.ToLower(stripDecorations(line))
	if clean == "" {
		return LevelInfo
	}
	for _, entry := range levelKeywords {
		if strings.Contains(clean, strings.ToLower(entry.needle)) {
			return entry.level
		}
	}
	return LevelInfo
}

// bracketTag returns the lowercase word inside a leading "[...]" tag, or "".
//
// The tag is read before any stripping, because stripping is what removes it and
// the tag is the only structured severity signal a log line carries.
func bracketTag(line string) string {
	trimmed := strings.TrimSpace(line)
	// A timestamp may precede the tag ("2009/01/23 01:23:23 [fatal] ..."), so
	// the tag is located rather than assumed to be first.
	if start := strings.IndexByte(trimmed, '['); start >= 0 {
		trimmed = trimmed[start:]
	} else {
		return ""
	}
	if !strings.HasPrefix(trimmed, "[") {
		return ""
	}
	end := strings.IndexByte(trimmed, ']')
	if end <= 1 {
		return ""
	}
	tag := strings.ToLower(trimmed[1:end])
	for _, r := range tag {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return ""
		}
	}
	return tag
}

// stripDecorations removes a leading timestamp and the level tag in front of a
// message, leaving the message itself.
func stripDecorations(line string) string {
	clean := strings.TrimSpace(line)
	clean = tsPrefixRe.ReplaceAllString(clean, "")
	clean = logDecorateRe.ReplaceAllString(clean, "")
	return strings.TrimSpace(clean)
}

// IsKnownKind reports whether kind is one this package emits, so a panel
// filtering by an arbitrary query string can reject nonsense before it is used
// as a filter.
func IsKnownKind(kind string) bool {
	switch kind {
	case KindChat, KindCheckin, KindTravel, KindPanel, KindProxy,
		KindAuth, KindScheduler, KindUpstream, KindOther:
		return true
	}
	return false
}

// Kinds lists every kind, in the order the panel's filter chips should show
// them: the subsystems an operator checks most, then the catch-all.
func Kinds() []string {
	return []string{
		KindChat, KindCheckin, KindTravel, KindScheduler,
		KindPanel, KindProxy, KindAuth, KindUpstream, KindOther,
	}
}
