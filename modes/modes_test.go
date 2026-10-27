package modes

import (
	"context"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- Tracker ----------------------------------------------------------------

func TestTurnReturnsPreviousLength(t *testing.T) {
	tr := NewTracker(0)
	if pl := tr.Turn("s", 5); pl != 0 {
		t.Fatalf("first turn: got %d, want 0", pl)
	}
	if pl := tr.Turn("s", 9); pl != 5 {
		t.Fatalf("second turn: got %d, want 5", pl)
	}
	// A shorter turn is the agent's own compaction: the boundary restarts at 0 and the
	// NEW, shorter transcript becomes the prefix later turns are measured against.
	if pl := tr.Turn("s", 3); pl != 0 {
		t.Fatalf("compaction did not restart the boundary: got %d, want 0", pl)
	}
	if pl := tr.Turn("s", 12); pl != 3 {
		t.Fatalf("boundary not rebased on the post-compaction prefix: got %d, want 3", pl)
	}
}

// TestBoundaryRule is the whole detection rule in one table: it GROWS on an append-only
// stream (the original invariant, which is what a normal agent turn looks like) and RESETS
// on any shrink, counting the reset. The cases it deliberately does not distinguish —
// a rewind, a retry, a truncated resend, two conversations colliding on one id — are
// listed as resets on purpose: see Boundary for why "any shrink" beats a threshold.
func TestBoundaryRule(t *testing.T) {
	tests := []struct {
		name      string
		prev, n   int
		want      int
		wantReset bool
	}{
		{"first turn", 0, 5, 0, false},
		{"append-only growth", 5, 9, 5, false},
		{"same length (retry of the same turn)", 9, 9, 9, false},
		{"compaction 50 -> 5", 50, 5, 0, true},
		{"partial compaction 50 -> 40 (a fraction threshold would miss this)", 50, 40, 0, true},
		{"one-message shrink (rewind; reset on purpose, fail-open)", 9, 8, 0, true},
		{"shrink to nothing", 9, 0, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := CompactionResets()
			if got := Boundary(tc.prev, tc.n); got != tc.want {
				t.Errorf("Boundary(%d, %d) = %d, want %d", tc.prev, tc.n, got, tc.want)
			}
			if got := CompactionResets() - before; (got != 0) != tc.wantReset {
				t.Errorf("reset delta = %d, wantReset %v", got, tc.wantReset)
			}
		})
	}
}

// TestTurnCountsCompactionResets: the reset has to be COUNTABLE through the tracker too,
// because that is the path the proxy takes and /stats is the only place an operator can
// see "this session restarted its prefix".
func TestTurnCountsCompactionResets(t *testing.T) {
	tr := NewTracker(0)
	before := CompactionResets()
	tr.Turn("s", 50)
	tr.Turn("s", 60) // growth: no reset
	if got := CompactionResets() - before; got != 0 {
		t.Fatalf("append-only turns reported %d compaction resets, want 0", got)
	}
	tr.Turn("s", 5) // compaction
	tr.Turn("s", 7) // growth again
	tr.Turn("s", 2) // second compaction
	if got := CompactionResets() - before; got != 2 {
		t.Fatalf("compaction resets = %d, want 2", got)
	}
}

func TestSessionsAreIsolated(t *testing.T) {
	tr := NewTracker(0)
	tr.Turn("a", 7)
	if pl := tr.Turn("b", 2); pl != 0 {
		t.Fatalf("session b saw session a's length: %d", pl)
	}
}

// TestConcurrentTurnsDoNotCorruptState is the race this type exists to remove: the
// previous implementation read prevLen from the store and wrote it back in a `defer`, so
// two concurrent turns of one session could both read the same value and the second's
// write-back could land first, leaving a boundary describing neither turn. Every observed
// value must be a length some turn really carried (or 0, the compaction reset).
// Run under -race.
//
// It no longer asserts that the final boundary is the LARGEST length seen: with the
// compaction reset, a concurrent turn that lands out of order looks like a shrink and
// rebases the boundary on its own length. That is deliberate and fail-open (lost savings
// for a turn, never a wrong rewrite) — see Boundary. What must still hold is that the
// state is never torn: the boundary always equals some real turn length.
func TestConcurrentTurnsDoNotCorruptState(t *testing.T) {
	tr := NewTracker(0)
	const n = 64

	seen := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			seen[i] = tr.Turn("s", i+1)
		}(i)
	}
	wg.Wait()

	for i, pl := range seen {
		if pl < 0 || pl > n {
			t.Fatalf("turn %d observed an impossible prevLen %d", i, pl)
		}
	}
	// The recorded boundary must be a length some turn really carried, i.e. in [1, n].
	if pl := tr.Turn("s", n+1); pl < 1 || pl > n {
		t.Fatalf("final boundary is %d, want a real turn length in [1, %d]", pl, n)
	}
}

// The tracker's session cap is its eviction policy (there is no session-end signal on
// this wire — an agent simply stops sending), so the cap must hold under an unbounded
// stream of distinct sessions.
func TestTrackerStaysBounded(t *testing.T) {
	small := NewTracker(2)
	for i := 0; i < 20; i++ {
		small.Turn("s"+strconv.Itoa(i), 1)
	}
	if n := small.Sessions(); n > 2 {
		t.Fatalf("tracker exceeded its bound: %d sessions", n)
	}
}

// At its bound the tracker evicts the least recently used key, so the threads of a busy session
// keep their record while new subagents arrive (#484). Each round adds one new key and then
// touches the 10 hot ones; every hot key must still be there. The old random eviction picked a hot
// key with probability 10/11 per round, so over 100 rounds it fails every time.
func TestTrackerEvictsLeastRecentlyUsed(t *testing.T) {
	const hot = 10
	tr := NewTracker(hot + 1)
	now := int64(1_700_000_000_000)
	for i := 0; i < hot; i++ {
		tr.TurnAt("hot"+strconv.Itoa(i), 5, now)
	}
	for round := 0; round < 100; round++ {
		now++
		tr.TurnAt("new"+strconv.Itoa(round), 5, now)
		for i := 0; i < hot; i++ {
			if prevLen, prevAt := tr.TurnAt("hot"+strconv.Itoa(i), 5, now); prevAt == 0 || prevLen != 5 {
				t.Fatalf("round %d: hot key %d was evicted while still in use", round, i)
			}
		}
	}
	if n := tr.Sessions(); n != hot+1 {
		t.Fatalf("tracker holds %d keys, want its bound %d", n, hot+1)
	}
}

// A new key that continues a parent's prefix reads the parent's record: its boundary (capped at
// its own length) and its clock. The parent's record is not written.
func TestTurnFromInheritsTheParentRecordReadOnly(t *testing.T) {
	tr := NewTracker(0)
	t0 := int64(1_700_000_000_000)
	tr.TurnAt("main", 8, t0)
	before := CompactionResets()

	if n, at := tr.TurnFrom("side", "main", 9, t0+1000); n != 8 || at != t0 {
		t.Fatalf("side request: (%d, %d), want the parent's (8, %d)", n, at, t0)
	}
	// A fork shorter than its parent diverged inside the parent's prefix: freeze all of it.
	if n, _ := tr.TurnFrom("fork", "main", 5, t0+2000); n != 5 {
		t.Fatalf("short fork: boundary %d, want its own length 5", n)
	}
	if got := CompactionResets() - before; got != 0 {
		t.Fatalf("an inherited read counted %d compaction resets", got)
	}
	// Read only: main's next turn is measured against its own 8 and its own time.
	if n, at := tr.TurnAt("main", 10, t0+3000); n != 8 || at != t0 {
		t.Fatalf("main after its children: (%d, %d), want (8, %d)", n, at, t0)
	}
	// Once the key has its own record, the parent no longer counts.
	if n, _ := tr.TurnFrom("side", "main", 12, t0+4000); n != 9 {
		t.Fatalf("side's second request: boundary %d, want its own 9", n)
	}
	// An unknown parent, or none, is a plain new key.
	if n, at := tr.TurnFrom("orphan", "nobody", 4, t0); n != 0 || at != 0 {
		t.Fatalf("orphan: (%d, %d), want (0, 0)", n, at)
	}
}

// A key the tracker evicted may still be cached at the provider. Its next request must read as
// "all cached" (boundary = its length), not 0, which would let every component rewrite it.
func TestDroppedKeyReadsAsAllCached(t *testing.T) {
	tr := NewTracker(1)
	t0 := int64(1_700_000_000_000)
	tr.TurnAt("a", 5, t0)
	tr.TurnAt("b", 3, t0+1) // evicts a
	if n, at := tr.TurnAt("a", 7, t0+2); n != 7 || at != 0 {
		t.Fatalf("evicted key came back as (%d, %d), want (7, 0): all cached, idle unknown", n, at)
	}
	// It has a real record again, so the turn after is normal.
	tr.TurnAt("c", 1, t0+3) // evicts the LRU, b
	if n, _ := tr.TurnAt("c", 2, t0+4); n != 1 {
		t.Fatalf("c: boundary %d, want 1", n)
	}
}

// A new thread whose parent was evicted reads as all cached too: the provider may still hold the
// parent's prefix that this request reads. The peek does not forget the parent's drop, so the
// parent's own next request is still all cached.
func TestNewKeyOfADroppedParentReadsAsAllCached(t *testing.T) {
	tr := NewTracker(1)
	t0 := int64(1_700_000_000_000)
	tr.TurnAt("main", 8, t0)
	tr.TurnAt("other", 3, t0+1) // evicts main
	if n, at := tr.TurnFrom("side", "main", 9, t0+2); n != 9 || at != 0 {
		t.Fatalf("side of an evicted parent: (%d, %d), want (9, 0): all cached, idle unknown", n, at)
	}
	if n, _ := tr.TurnAt("main", 10, t0+3); n != 10 {
		t.Fatalf("evicted parent's own next request: boundary %d, want 10 (all cached)", n)
	}
}

// Issue #496. The tracker's records die with the process; the provider's cache entries do not.
// A tracker younger than restartGraceMs reads an unknown key as all cached, like a dropped key,
// so a proxy restart cannot let components rewrite a live thread's cached prefix. The idle
// clock stays unknown (prevAtMs 0), so nothing reads cold off a fabricated age.
func TestAYoungTrackerReadsAnUnknownKeyAsAllCached(t *testing.T) {
	t0 := int64(1_700_000_000_000)
	tr := NewTracker(0)
	tr.startedAtMs = t0 - time.Minute.Milliseconds() // started 1 minute ago
	if n, at := tr.TurnAt("unknown", 925, t0); n != 925 || at != 0 {
		t.Fatalf("young tracker, unknown key = (%d, %d), want (925, 0): all cached, idle unknown", n, at)
	}
	// The same answer with a parent that has no record of its own: no usable parent.
	if n, at := tr.TurnFrom("side", "nobody", 925, t0); n != 925 || at != 0 {
		t.Fatalf("young tracker, unknown key with a parent = (%d, %d), want (925, 0)", n, at)
	}
}

// Past the grace an unknown key is a new thread again, as before: no entry the restart forgot
// can still be alive after the longest cache lifetime.
func TestAnOldTrackerReadsAnUnknownKeyAsNew(t *testing.T) {
	t0 := int64(1_700_000_000_000)
	tr := NewTracker(0)
	tr.startedAtMs = t0 - 2*time.Hour.Milliseconds() // started 2 hours ago
	if n, at := tr.TurnAt("unknown", 925, t0); n != 0 || at != 0 {
		t.Fatalf("old tracker, unknown key = (%d, %d), want (0, 0): a new thread, as today", n, at)
	}
	if n, at := tr.TurnFrom("side", "nobody", 925, t0); n != 0 || at != 0 {
		t.Fatalf("old tracker, unknown key with a parent = (%d, %d), want (0, 0)", n, at)
	}
}

// At the bound, keys idle past StaleAfter expire first, all at once, before a live key is evicted.
// Here b was used AFTER a, so least-recently-used alone would evict a, the live one.
func TestStaleKeysExpireBeforeLiveOnes(t *testing.T) {
	tr := NewTracker(3)
	now := int64(1_700_000_000_000) + StaleAfter.Milliseconds()*2
	stale := now - StaleAfter.Milliseconds() - 1
	tr.TurnAt("a", 4, now)
	tr.TurnAt("b", 4, stale)
	tr.TurnAt("c", 4, stale)
	tr.TurnAt("d", 4, now+1) // the tracker is full: b and c expire
	if n := tr.Sessions(); n != 2 {
		t.Fatalf("tracker holds %d keys, want 2 (both stale keys expired)", n)
	}
	if n, at := tr.TurnAt("a", 5, now+2); n != 4 || at != now {
		t.Fatalf("live key a was dropped: (%d, %d)", n, at)
	}
	if n, _ := tr.TurnAt("b", 6, now+3); n != 6 {
		t.Fatalf("expired key b: boundary %d, want 6 (all cached), not 0", n)
	}
}

// --- Pool -------------------------------------------------------------------

func TestPoolRunsJobs(t *testing.T) {
	p := NewPool(0, 0)
	defer p.Stop()
	done := make(chan struct{})
	if !p.Enqueue("k", func(context.Context) { close(done) }) {
		t.Fatal("enqueue refused")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("job never ran")
	}
	waitFor(t, func() bool { return p.Stats().Processed == 1 })
}

// TestEnqueueDedupIsAtomic hammers one key from many goroutines while the worker is
// blocked. Exactly one may be accepted: the pending slot is claimed before the job is
// observable in the queue, so a concurrent enqueue cannot slip past the check.
func TestEnqueueDedupIsAtomic(t *testing.T) {
	p := NewPool(0, 1)
	defer p.Stop()

	release := make(chan struct{})
	var ran atomic.Int64
	block := func(context.Context) { ran.Add(1); <-release }

	var accepted atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p.Enqueue("same-key", block) {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := accepted.Load(); n != 1 {
		t.Fatalf("dedup admitted %d jobs for one key, want 1", n)
	}
	close(release)
	waitFor(t, func() bool { return p.Stats().Pending == 0 })
	if n := ran.Load(); n != 1 {
		t.Fatalf("job body ran %d times, want 1", n)
	}
}

// TestFullQueueDropsAndNeverBlocks: the request has already been forwarded, so a drop
// costs a measurement only — but it must be counted, and Enqueue must not block.
func TestFullQueueDropsAndNeverBlocks(t *testing.T) {
	p := NewPool(2, 1)
	defer p.Stop()

	release := make(chan struct{})
	defer close(release)
	p.Enqueue("busy", func(context.Context) { <-release }) // occupy the single worker
	waitFor(t, func() bool { return p.Stats().Pending == 1 })

	noop := func(context.Context) {}
	accepted, dropped := 0, 0
	deadline := time.After(5 * time.Second)
	for i := 0; i < 50; i++ {
		ok := make(chan bool, 1)
		go func(i int) { ok <- p.Enqueue(strconv.Itoa(i), noop) }(i)
		select {
		case v := <-ok:
			if v {
				accepted++
			} else {
				dropped++
			}
		case <-deadline:
			t.Fatal("Enqueue blocked on a full queue")
		}
	}
	if dropped == 0 {
		t.Fatal("a full queue accepted everything")
	}
	if got := p.Stats().Dropped; got != int64(dropped) {
		t.Fatalf("dropped counter is %d, want %d", got, dropped)
	}
	if accepted > 2 {
		t.Fatalf("queue of 2 accepted %d jobs", accepted)
	}
}

func TestStopLeaksNoGoroutines(t *testing.T) {
	settle()
	before := runtime.NumGoroutine()

	p := NewPool(16, 4)
	p.Enqueue("a", func(context.Context) {})
	waitFor(t, func() bool { return p.Stats().Processed >= 1 })
	if !p.Stop() {
		t.Fatal("Stop reported an unclean exit with no job running")
	}
	p.Stop() // idempotent

	settle()
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutine leak: %d before, %d after Stop", before, after)
	}
	if p.Enqueue("b", func(context.Context) {}) {
		t.Fatal("a stopped pool accepted a job")
	}
}

// TestStopDoesNotWaitForASlowJob: cancelling asks a job to stop, but one blocked in an
// HTTP call to the cheap model only notices when that call returns — and its client
// timeout is minutes. Shutdown must not inherit that: the result is a measurement nobody
// waits for, and main defers Close().
func TestStopDoesNotWaitForASlowJob(t *testing.T) {
	p := NewPool(0, 1)
	release := make(chan struct{})
	defer close(release)

	started := make(chan struct{})
	p.Enqueue("slow", func(context.Context) {
		close(started)
		<-release // ignores cancellation, like an in-flight HTTP call
	})
	<-started

	done := make(chan bool, 1)
	go func() { done <- p.Stop() }()
	select {
	case clean := <-done:
		if clean {
			t.Fatal("Stop reported a clean exit while a job was still running")
		}
	case <-time.After(stopGrace + 3*time.Second):
		t.Fatal("Stop blocked past its grace period on an uncancellable job")
	}
}

// TestPanickingJobIsContained: fail-open. Nothing was riding on the job.
func TestPanickingJobIsContained(t *testing.T) {
	p := NewPool(0, 1)
	defer p.Stop()
	p.Enqueue("boom", func(context.Context) { panic("nope") })
	waitFor(t, func() bool { return p.Stats().Errors == 1 })

	done := make(chan struct{})
	p.Enqueue("after", func(context.Context) { close(done) })
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the worker died with the panicking job")
	}
}

// --- helpers ----------------------------------------------------------------

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}

// settle gives already-finishing goroutines a chance to exit so a leak check compares
// like with like.
func settle() {
	for i := 0; i < 20; i++ {
		runtime.Gosched()
		time.Sleep(5 * time.Millisecond)
	}
}
