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
