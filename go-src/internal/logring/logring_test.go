package logring

import (
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

// at builds a fixed local wall clock for rendering assertions.
func at(hour, min, sec int) time.Time {
	return time.Date(2026, time.March, 10, hour, min, sec, 0, time.Local)
}

// newRing builds a ring whose clock is pinned, so At is a value the test chose.
func newRing(capacity int) *Ring {
	return New(capacity).WithClock(func() time.Time { return at(9, 30, 15) })
}

func TestRingKeepsTheMostRecentLines(t *testing.T) {
	r := newRing(3)
	for _, line := range []string{"one", "two", "three", "four", "five"} {
		if _, err := io.WriteString(r, line); err != nil {
			t.Fatalf("write %q: %v", line, err)
		}
	}

	if got := r.Len(); got != 3 {
		t.Fatalf("Len = %d, want 3 (ring is full at capacity)", got)
	}
	got := r.Snapshot()
	want := []string{"three", "four", "five"}
	if len(got) != len(want) {
		t.Fatalf("Snapshot returned %d lines, want %d", len(got), len(want))
	}
	for i, line := range got {
		if line.Text != want[i] {
			t.Fatalf("line %d = %q, want %q", i, line.Text, want[i])
		}
	}
}

func TestRingEvictionKeepsSeqDenseAndAscending(t *testing.T) {
	r := newRing(2)
	io.WriteString(r, "a")
	io.WriteString(r, "b")
	io.WriteString(r, "c")

	lines := r.Snapshot()
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	if lines[0].Seq >= lines[1].Seq {
		t.Fatalf("seqs are not ascending: %d then %d", lines[0].Seq, lines[1].Seq)
	}
	if lines[1].Seq != r.CurrentSeq()-1 {
		t.Fatalf("newest seq = %d, but CurrentSeq-1 = %d",
			lines[1].Seq, r.CurrentSeq()-1)
	}
	if lines[0].Text != "b" || lines[1].Text != "c" {
		t.Fatalf("eviction kept the wrong lines: %q, %q", lines[0].Text, lines[1].Text)
	}
}

func TestRingNeverGrowsPastCapacity(t *testing.T) {
	r := newRing(4)
	for i := 0; i < 1000; i++ {
		fmt.Fprintf(r, "line %d", i)
	}
	if r.Len() != 4 {
		t.Fatalf("Len = %d, want 4", r.Len())
	}
	if r.Cap() != 4 {
		t.Fatalf("Cap = %d, want 4", r.Cap())
	}
}

func TestSnapshotSinceReturnsOnlyNewerLines(t *testing.T) {
	r := newRing(10)
	io.WriteString(r, "first")
	io.WriteString(r, "second")

	seen := r.CurrentSeq()
	if len(r.SnapshotSince(seen)) != 0 {
		t.Fatal("SnapshotSince(CurrentSeq) returned lines it should not have")
	}

	io.WriteString(r, "third")
	fresh := r.SnapshotSince(seen)
	if len(fresh) != 1 || fresh[0].Text != "third" {
		t.Fatalf("SnapshotSince returned %v, want exactly [third]", fresh)
	}

	// Everything ever written is returned from zero, not just the first line.
	all := r.SnapshotSince(0)
	if len(all) != 3 {
		t.Fatalf("SnapshotSince(0) returned %d lines, want 3", len(all))
	}
	if all[0].Text != "first" || all[2].Text != "third" {
		t.Fatalf("SnapshotSince(0) ordering is wrong: %q .. %q", all[0].Text, all[2].Text)
	}
}

func TestSnapshotSinceAfterEvictionReturnsWhatIsHeld(t *testing.T) {
	r := newRing(2)
	io.WriteString(r, "old")
	evicted := r.CurrentSeq()
	io.WriteString(r, "newer")
	io.WriteString(r, "newest")

	got := r.SnapshotSince(evicted)
	if len(got) != 2 {
		t.Fatalf("got %d lines, want the 2 still held", len(got))
	}
	for _, line := range got {
		if line.Seq < evicted {
			t.Fatalf("line %q predates the cursor %d", line.Text, evicted)
		}
	}
}

func TestRingStripsATimestampAndRendersItsOwn(t *testing.T) {
	r := newRing(5)
	// Go's default log prefix, and the bare clock time the old in-memory ring
	// used, must both collapse to a single rendered timestamp.
	io.WriteString(r, "2009/01/23 01:23:23 hello world")
	io.WriteString(r, "01:23:23 hello again")

	lines := r.Snapshot()
	if lines[0].Text != "hello world" {
		t.Fatalf("Go log prefix not stripped: %q", lines[0].Text)
	}
	if lines[1].Text != "hello again" {
		t.Fatalf("bare clock prefix not stripped: %q", lines[1].Text)
	}
	for _, line := range lines {
		if line.At != "09:30:15" {
			t.Fatalf("At = %q, want the ring's own clock", line.At)
		}
		if strings.Count(line.Text, ":") != 0 {
			t.Fatalf("line still carries a clock time: %q", line.Text)
		}
	}
}

func TestRingSplitsEmbeddedNewlines(t *testing.T) {
	r := newRing(5)
	io.WriteString(r, "first\nsecond\n\nthird\n")
	if got := r.Len(); got != 3 {
		t.Fatalf("Len = %d, want 3 (blank lines dropped)", got)
	}
	lines := r.Snapshot()
	for i, want := range []string{"first", "second", "third"} {
		if lines[i].Text != want {
			t.Fatalf("line %d = %q, want %q", i, lines[i].Text, want)
		}
	}
}

func TestRingImplementsIOWriterForLogSetOutput(t *testing.T) {
	r := newRing(10)

	// Wire it the way the gateway does, then log through the standard logger.
	previous := log.Writer()
	previousFlags := log.Flags()
	log.SetOutput(io.MultiWriter(io.Discard, r))
	log.SetFlags(log.LstdFlags)
	defer func() {
		log.SetOutput(previous)
		log.SetFlags(previousFlags)
	}()

	log.Printf("scheduler: fired 09:00 job")

	lines := r.Snapshot()
	if len(lines) != 1 {
		t.Fatalf("log.Printf produced %d lines, want 1", len(lines))
	}
	if !strings.Contains(lines[0].Text, "fired 09:00 job") {
		t.Fatalf("unexpected line: %q", lines[0].Text)
	}
	if lines[0].Kind != KindScheduler {
		t.Fatalf("kind = %q, want %q", lines[0].Kind, KindScheduler)
	}
	// The log package's own date/time prefix must not survive into the text.
	if strings.Contains(lines[0].Text, "2009/") || strings.Contains(lines[0].Text, "01:23") {
		t.Fatalf("log package timestamp leaked into text: %q", lines[0].Text)
	}
}

func TestWriteReportsTheFullLengthAndNoError(t *testing.T) {
	r := newRing(2)
	n, err := r.Write([]byte("a line long enough to matter"))
	if err != nil {
		t.Fatalf("Write returned an error: %v", err)
	}
	if want := len("a line long enough to matter"); n != want {
		t.Fatalf("Write returned n = %d, want %d", n, want)
	}
	if n, err := r.Write(nil); n != 0 || err != nil {
		t.Fatalf("Write(nil) = (%d, %v), want (0, nil)", n, err)
	}
}

func TestWriteIsSafeFromManyGoroutines(t *testing.T) {
	r := newRing(50)

	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				fmt.Fprintf(r, "goroutine %d line %d", id, i)
			}
		}(g)
	}
	// Snapshotting while writes are in flight is the panel's whole access
	// pattern; the race detector is what proves it is safe.
	wgDone := make(chan struct{})
	go func() {
		defer close(wgDone)
		for i := 0; i < 50; i++ {
			_ = r.Snapshot()
			_ = r.SnapshotSince(0)
			_ = r.Len()
		}
	}()
	wg.Wait()
	<-wgDone

	if r.Len() != 50 {
		t.Fatalf("Len = %d, want the ring full at 50", r.Len())
	}
	// Sequences must stay unique and ordered even under contention.
	lines := r.Snapshot()
	for i := 1; i < len(lines); i++ {
		if lines[i].Seq != lines[i-1].Seq+1 {
			t.Fatalf("seq gap at %d: %d then %d", i, lines[i-1].Seq, lines[i].Seq)
		}
	}
}

func TestSnapshotIsACopyTheCallerOwns(t *testing.T) {
	r := newRing(4)
	io.WriteString(r, "original")

	lines := r.Snapshot()
	lines[0].Text = "mutated by the caller"

	if got := r.Snapshot()[0].Text; got != "original" {
		t.Fatalf("the ring was mutated through a snapshot: %q", got)
	}
}

func TestKindOfMapsRealisticGatewayLines(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		// The lines internal/server actually emits today.
		{"chat uid=abc model=auto stream=true", KindChat},
		{"chat uid=abc err=upstream unreachable", KindChat},
		{"account 42 checkin -> HTTP 200", KindCheckin},
		{"proxy slot home created (http://127.0.0.1:1080)", KindProxy},
		{"proxy slot home removed", KindProxy},
		{"proxy slot home probe: ok=true exit=1.2.3.4 美国 住宅", KindProxy},
		{"proxy slot home bound to 2 account(s)", KindProxy},
		{"api key k1 created (name=\"phone\" realm=\"cn\")", KindAuth},
		{"account 42 imported (realm=cn)", KindAuth},
		{"account 42 enabled=false", KindAuth},
		{"account 42 cooling cleared", KindAuth},
		{"limits updated: {DailyCreditLimit:100}", KindPanel},
		{"restart requested", KindPanel},
		{"local web tools enabled=true", KindPanel},
		// The start-up banner from cmd/server/main.go.
		{"[start] wb2hub 1.0.0 listening on 127.0.0.1:7863", KindPanel},
		{"[start] accounts=2 auth_dir=/data/auths", KindPanel},
		{"[auth] loaded 2 account(s) from /data/auths", KindAuth},
		{"[stop] wb2hub exited", KindPanel},
		{"[warn] skipped credential: bad json", KindAuth},
		// The scheduler's own lines.
		{"scheduler: fired 09:00 job", KindScheduler},
		{"scheduler: 整点排程命中 (9:00)", KindScheduler},
		{"调度器: 巡检完成：Token保活 1 个", KindScheduler},
		{"keepalive: token refreshed for account 42", KindScheduler},
		// Growth / travel / check-in.
		{"checkin: account 42 already checked in today", KindCheckin},
		{"travel: buddy departed for 上海", KindTravel},
		{"black_cat progress 2/3", KindTravel},
		{"lottery: 1 draw earned 50 credits", KindCheckin},
		{"growth: redeem tier 1 -> +100", KindCheckin},
		{"makeup-cards: used 1 for 2026-03-09", KindCheckin},
		// Upstream transport failures.
		{"upstream HTTP 502: bad gateway", KindUpstream},
		{"TLS handshake timeout for copilot.tencent.com", KindUpstream},
		// Nothing recognisable.
		{"", KindOther},
		{"well this is a line about nothing in particular", KindOther},
	}

	for _, tc := range cases {
		if got := KindOf(tc.line); got != tc.want {
			t.Errorf("KindOf(%q) = %q, want %q", tc.line, got, tc.want)
		}
	}
}

func TestKindOfPrefersTheSpecificSubsystemOverTheMentionedOne(t *testing.T) {
	// An account's check-in is a check-in event: an operator filtering for
	// "checkin" wants it, and filtering for "auth" does not.
	if got := KindOf("account 42 checkin -> HTTP 200"); got != KindCheckin {
		t.Fatalf("kind = %q, want %q", got, KindCheckin)
	}
	// A task label at the head of a line wins over a keyword later in it.
	if got := KindOf("travel: buddy departed, upstream HTTP 200"); got != KindTravel {
		t.Fatalf("kind = %q, want %q", got, KindTravel)
	}
}

func TestLevelOfInfersSeverity(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		{"chat uid=abc model=auto stream=true", LevelInfo},
		{"[fatal] load config: bad json", LevelError},
		{"chat uid=abc err=connection refused", LevelError},
		{"proxy slot home probe failed: timeout", LevelError},
		{"account 42 签到失败: token expired", LevelError},
		{"[warn] skipped credential: bad json", LevelWarn},
		{"account 42 retrying in 30s", LevelWarn},
		{"scheduler: skipped 01:00 job, not in night window", LevelWarn},
		{"旅行任务跳过 (需桌面端)", LevelWarn},
		{"scheduler: fired 09:00 job", LevelInfo},
		{"", LevelInfo},
	}

	for _, tc := range cases {
		if got := LevelOf(tc.line); got != tc.want {
			t.Errorf("LevelOf(%q) = %q, want %q", tc.line, got, tc.want)
		}
	}
}

func TestErrorOutranksWarningOnTheSameLine(t *testing.T) {
	// "failed (retrying)" is a failure the operator should see as one.
	if got := LevelOf("upstream failed, retrying in 5s"); got != LevelError {
		t.Fatalf("level = %q, want %q", got, LevelError)
	}
}

func TestClassifiedLinesCarryTheStrippedText(t *testing.T) {
	r := newRing(4)
	io.WriteString(r, "2009/01/23 01:23:23 [warn] skipped credential: bad json")

	line := r.Snapshot()[0]
	if line.Level != LevelWarn {
		t.Fatalf("level = %q, want %q", line.Level, LevelWarn)
	}
	if line.Kind != KindAuth {
		t.Fatalf("kind = %q, want %q", line.Kind, KindAuth)
	}
	if line.Text != "skipped credential: bad json" {
		t.Fatalf("text = %q, want the message without its decorations", line.Text)
	}
}

func TestNewClampsACapacityThatWouldDisableTheRing(t *testing.T) {
	if got := New(0).Cap(); got != DefaultCapacity {
		t.Fatalf("New(0).Cap() = %d, want %d", got, DefaultCapacity)
	}
	if got := New(-5).Cap(); got != DefaultCapacity {
		t.Fatalf("New(-5).Cap() = %d, want %d", got, DefaultCapacity)
	}
}

func TestIsKnownKindAndKindsAgree(t *testing.T) {
	listed := Kinds()
	if len(listed) != 9 {
		t.Fatalf("Kinds() has %d entries, want 9", len(listed))
	}
	for _, kind := range listed {
		if !IsKnownKind(kind) {
			t.Fatalf("IsKnownKind(%q) = false, but it is listed", kind)
		}
	}
	if IsKnownKind("nonsense") {
		t.Fatal("IsKnownKind accepted an invented kind")
	}
	// Every kind the classifier can return must be filterable by the panel.
	for _, line := range []string{"", "chat", "checkin", "travel", "scheduler", "proxy", "auth", "upstream"} {
		if got := KindOf(line); !IsKnownKind(got) {
			t.Fatalf("KindOf(%q) = %q, which the panel cannot filter on", line, got)
		}
	}
}

func TestStringRendersATranscript(t *testing.T) {
	r := newRing(4)
	io.WriteString(r, "one")
	io.WriteString(r, "two")

	want := "09:30:15 one\n09:30:15 two\n"
	if got := r.String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
