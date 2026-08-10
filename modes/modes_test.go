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

func TestTurnReturnsPreviousLengthAndAdvances(t *testing.T) {
	// Generation VALUES are not per-session counters — they are drawn from a global
	// high-water mark so a session recreated after eviction cannot reuse one an in-flight
	// job still holds. Only the ordering is contractual: strictly increasing per turn.
	tr := NewTracker(0)
	pl0, g0 := tr.Turn("s", 5)
	if pl0 != 0 {
		t.Fatalf("first turn prevLen: got %d, want 0", pl0)
	}
	pl1, g1 := tr.Turn("s", 9)
	if pl1 != 5 {
		t.Fatalf("second turn prevLen: got %d, want 5", pl1)
	}
	if g1 <= g0 {
		t.Fatalf("generation did not advance on a turn: %d then %d", g0, g1)
	}
	// A shorter turn must not shrink the boundary: content the provider already
	// cached would otherwise fall back into the mutable tail.
	if pl, _ := tr.Turn("s", 3); pl != 9 {
		t.Fatalf("shorter turn moved the boundary: got %d, want 9", pl)
	}
	if pl, _ := tr.Turn("s", 12); pl != 9 {
		t.Fatalf("boundary not preserved: got %d, want 9", pl)
	}
}

func TestSessionsAreIsolated(t *testing.T) {
	tr := NewTracker(0)
	tr.Turn("a", 7)
	if pl, _ := tr.Turn("b", 2); pl != 0 {
		t.Fatalf("session b saw session a's length: %d", pl)
	}
	// A commit on one session must not move another's generation (which would make
	// b's in-flight jobs spuriously stale).
	_, gb := tr.Turn("b", 4)
	tr.CommitIfCurrent("a", 1, nil)
	if g := tr.Gen("b"); g != gb {
		t.Fatalf("session b's generation moved with a's: %d, want %d", g, gb)
	}
}

// TestStaleResultIsDiscarded is the issue's single most important invariant: a
// result computed from a superseded generation must never be applied.
func TestStaleResultIsDiscarded(t *testing.T) {
	tr := NewTracker(0)
	_, gen := tr.Turn("s", 4)

	applied := 0
	if !tr.CommitIfCurrent("s", gen, func() { applied++ }) {
		t.Fatal("current generation was rejected")
	}
	if applied != 1 {
		t.Fatalf("commit did not run: %d", applied)
	}
	// A LATER TURN ships. That, not a previous commit, is what makes a job stale in
	// practice — and it is exactly the case an earlier version got wrong: the
	// generation only moved on commit, so a job from turn 1 stayed "current" through
	// any number of later turns and committed against a transcript long since replaced.
	tr.Turn("s", 9)
	if tr.CommitIfCurrent("s", gen, func() { applied++ }) {
		t.Fatal("a job superseded by a later TURN was accepted")
	}
	if applied != 1 {
		t.Fatalf("stale commit ran anyway: applied=%d", applied)
	}
}

// TestGenerationAdvancesPerTurn pins the invariant directly: every turn supersedes the
// jobs of every earlier turn. Without this the stale guard can only ever catch a dedup
// collision, never actual staleness — the guard exists but never fires.
func TestGenerationAdvancesPerTurn(t *testing.T) {
	tr := NewTracker(0)
	_, first := tr.Turn("s", 4)
	for n := 5; n <= 12; n++ {
		tr.Turn("s", n)
	}
	if g := tr.Gen("s"); g == first {
		t.Fatal("the generation did not move across eight turns")
	}
	if tr.CommitIfCurrent("s", first, func() {}) {
		t.Fatal("a job from the first turn committed after eight later turns")
	}
}

// TestLandedGatesRealizedSavings: before any deferred compaction commits, a session has
// realized nothing from deferral — whatever the inline pass saved, it saved on the
// request path. Crediting that to the deferral would make the "realized" counter a
// tautology (it reported exactly the inline saving on turn 1 of a model-free pipeline).
func TestLandedGatesRealizedSavings(t *testing.T) {
	tr := NewTracker(0)
	_, gen := tr.Turn("s", 4)
	if tr.Landed("s") {
		t.Fatal("a session with no committed compaction reports one")
	}
	tr.CommitIfCurrent("s", gen, func() {})
	if !tr.Landed("s") {
		t.Fatal("a committed compaction was not recorded")
	}
	if tr.Landed("other") {
		t.Fatal("Landed leaked across sessions")
	}
}

// TestConcurrentCommitsOnlyOneWins proves two jobs racing on one session's
// generation cannot both apply. Run under -race.
func TestConcurrentCommitsOnlyOneWins(t *testing.T) {
	tr := NewTracker(0)
	_, gen := tr.Turn("s", 4)

	var applied atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr.CommitIfCurrent("s", gen, func() { applied.Add(1) })
		}()
	}
	wg.Wait()
	if n := applied.Load(); n != 1 {
		t.Fatalf("concurrent commits at one generation applied %d times, want 1", n)
	}
}

// TestConcurrentTurnsDoNotCorruptState is the hazard the issue names: the old
// prevLen was read then written back in a defer, so two turns of one session raced.
// Every observed prevLen must be a length some turn really carried (never a torn or
// lost value), and the final boundary must be the largest.
func TestConcurrentTurnsDoNotCorruptState(t *testing.T) {
	tr := NewTracker(0)
	const n = 64

	seen := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pl, _ := tr.Turn("s", i+1)
			seen[i] = pl
		}(i)
	}
	wg.Wait()

	for i, pl := range seen {
		if pl < 0 || pl > n {
			t.Fatalf("turn %d observed an impossible prevLen %d", i, pl)
		}
	}
	if pl, _ := tr.Turn("s", 0); pl != n {
		t.Fatalf("final boundary is %d, want %d (a concurrent write was lost)", pl, n)
	}
}

// The tracker's session cap IS its eviction policy (there is no session-end signal to
// hook), so the cap must actually hold under an unbounded stream of distinct sessions.
func TestTrackerStaysBounded(t *testing.T) {
	small := NewTracker(2)
	for i := 0; i < 20; i++ {
		small.Turn(string(rune('a'+i)), 1)
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
// blocked. Exactly one may be accepted: the pending slot is claimed before the job
// is observable in the queue, so a concurrent enqueue cannot slip past the check.
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

// TestFullQueueDropsAndNeverBlocks: the request has already been forwarded, so a
// drop costs savings only — but it must be counted, and Enqueue must not block.
func TestFullQueueDropsAndNeverBlocks(t *testing.T) {
	p := NewPool(2, 1)
	defer p.Stop()

	release := make(chan struct{})
	defer close(release)
	// Occupy the single worker so nothing drains.
	p.Enqueue("busy", func(context.Context) { <-release })
	waitFor(t, func() bool { return p.Stats().Pending == 1 })

	noop := func(context.Context) {}
	accepted, dropped := 0, 0
	deadline := time.After(5 * time.Second)
	for i := 0; i < 50; i++ {
		ok := make(chan bool, 1)
		go func(i int) { ok <- p.Enqueue(string(rune('A'+i)), noop) }(i)
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

// TestStopLeaksNoGoroutines: cancellation must return every worker.
func TestStopLeaksNoGoroutines(t *testing.T) {
	settle()
	before := runtime.NumGoroutine()

	p := NewPool(16, 4)
	p.Enqueue("a", func(context.Context) {})
	waitFor(t, func() bool { return p.Stats().Processed >= 1 })
	p.Stop()
	p.Stop() // idempotent

	settle()
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutine leak: %d before, %d after Stop", before, after)
	}
	if p.Enqueue("b", func(context.Context) {}) {
		t.Fatal("a stopped pool accepted a job")
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

func TestStatsExposesTheWholeTuple(t *testing.T) {
	p := NewPool(0, 1)
	defer p.Stop()
	p.RecordStale()
	p.Enqueue("boom", func(context.Context) { panic("x") })
	waitFor(t, func() bool { return p.Stats().Errors == 1 })
	s := p.Stats()
	if s.StaleDiscarded != 1 || s.Errors != 1 {
		t.Fatalf("counters not recorded: %+v", s)
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

// settle gives already-finishing goroutines a chance to exit so a leak check
// compares like with like.
func settle() {
	for i := 0; i < 20; i++ {
		runtime.Gosched()
		time.Sleep(5 * time.Millisecond)
	}
}

// TestEvictionDoesNotResurrectAnInFlightJob: a session recreated after eviction must not
// restart at a generation an in-flight job could still match, or that job commits over a
// session that has moved several turns on.
func TestEvictionDoesNotResurrectAnInFlightJob(t *testing.T) {
	tr := NewTracker(2)
	_, gen := tr.Turn("victim", 4) // a job is now in flight holding `gen`

	// Churn other sessions until "victim" is evicted.
	for i := 0; i < 10; i++ {
		tr.Turn("filler"+strconv.Itoa(i), 3)
	}
	// "victim" comes back as a fresh session.
	tr.Turn("victim", 5)

	if tr.CommitIfCurrent("victim", gen, func() {}) {
		t.Fatalf("a pre-eviction job (gen %d) committed over the recreated session", gen)
	}
}

// TestStopDoesNotWaitForASlowJob: cancelling asks a job to stop, but one blocked in an
// HTTP call to the cheap model only notices when that call returns — and its client
// timeout is minutes. Shutdown must not inherit that: the job's result is pure savings
// nobody is waiting for. main.go defers Close(), so a blocking Stop would hang exit.
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

// TestBarrenSessionStopsPayingForCompaction: each deferred job is a real cheap-model
// call, so traffic that simply does not compact must stop buying attempts.
func TestBarrenSessionStopsPayingForCompaction(t *testing.T) {
	tr := NewTracker(0)
	for i := 0; i < barrenLimit; i++ {
		if tr.Barren("s") {
			t.Fatalf("gave up after only %d unproductive jobs", i)
		}
		tr.RecordJobOutcome("s", false)
	}
	if !tr.Barren("s") {
		t.Fatalf("still deferring after %d unproductive jobs", barrenLimit)
	}
	// One productive job proves the traffic does compact after all; resume.
	tr.RecordJobOutcome("s", true)
	if tr.Barren("s") {
		t.Fatal("a productive job did not reset the budget")
	}
	if tr.Barren("untouched") {
		t.Fatal("the barren budget leaked across sessions")
	}
}
