package offload

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/thread"
	"github.com/rossoctl/context-guru/store"
)

// The summary checkpoint, the single-flight registry and the cold-deferral registry are keyed by
// THREAD, not session (#430). Each thread of a Claude Code session is its own message list with its
// own provider cache entry, so one thread's summary must never be offered to another, and one
// thread's commission in flight must never block another.

// threadModel counts its calls atomically: two threads' detached commissions run concurrently.
type threadModel struct {
	out   string
	calls atomic.Int64
}

func (m *threadModel) Complete(context.Context, string) (string, error) { return m.out, nil }

func (m *threadModel) CompleteMessages(context.Context, string, []bschemas.ChatMessage) (string, error) {
	m.calls.Add(1)
	return m.out, nil
}

func threadCtx(session, threadID string, st store.Store, phase string) *components.Ctx {
	c := caGatedCtx(session, st, phase, 0, false, 0)
	c.Thread = threadID
	return c
}

// The primary thread keeps the bare session key, so a single-thread session reads the checkpoint it
// wrote before threads existed. A subagent's checkpoint lives under its own key.
func TestCheckpointKeyIsTheThreadAndThePrimaryKeepsTheSession(t *testing.T) {
	st := store.NewMemory(store.Options{MaxEntries: 100})
	main := threadCtx("thr-key", thread.Primary, st, "unknown")
	sub := threadCtx("thr-key", "agent-1", st, "unknown")

	saveCheckpoint(main, sumCheckpoint{SummaryMsg: "main", CoveredCount: 3})
	if _, ok := st.Get(store.SumPrefix + "thr-key"); !ok {
		t.Fatal("the primary thread's checkpoint is not under the bare session key")
	}
	if _, ok := loadCheckpoint(sub); ok {
		t.Fatal("a subagent read the main thread's checkpoint")
	}
	saveCheckpoint(sub, sumCheckpoint{SummaryMsg: "sub", CoveredCount: 5})
	if cp, _ := loadCheckpoint(main); cp.SummaryMsg != "main" {
		t.Fatalf("the subagent's save overwrote the main thread's checkpoint: %q", cp.SummaryMsg)
	}
	if cp, _ := loadCheckpoint(sub); cp.SummaryMsg != "sub" {
		t.Fatalf("subagent checkpoint = %q, want its own", cp.SummaryMsg)
	}
}

// A subagent's commission writes the subagent's checkpoint only. Before #430 it wrote the session's,
// and the main thread's next turn was offered the subagent's summary.
func TestCacheAwareCommissionWritesOnlyItsOwnThread(t *testing.T) {
	s := newCacheAware(t, "keep_last_turns: 1\nmin_tokens: 10\ninstruction_role: user\n"+
		"trigger:\n  min_request_frac: 0\n  cache_state: any\n")
	s.modelClient = &threadModel{out: "<summary>subagent work.</summary>"}
	st := store.NewMemory(store.Options{MaxEntries: 400})
	main := threadCtx("thr-commit", thread.Primary, st, "warm")
	sub := threadCtx("thr-commit", "agent-1", st, "warm")

	req := &bschemas.BifrostChatRequest{Input: caTranscript(3)}
	var rep components.Report
	if _, err := s.Offload(req, &rep, sub); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	if !WaitForSummaryForTest(convKey(sub), 5*time.Second) {
		t.Fatal("the subagent's summary never resolved")
	}
	if _, ok := loadCheckpoint(sub); !ok {
		t.Fatalf("the subagent's commission wrote no checkpoint (gates: %v, events: %v)", rep.Gates, rep.Events)
	}
	if _, ok := loadCheckpoint(main); ok {
		t.Fatal("the subagent's commission wrote the main thread's checkpoint")
	}
}

// One thread's commission in flight does not block another thread's. Both callers of the registry
// are checked: the detached commission and the keep-alive substitute.
func TestInFlightGuardIsPerThread(t *testing.T) {
	s := newCacheAware(t, "keep_last_turns: 1\nmin_tokens: 10\ninstruction_role: user\n"+
		"trigger:\n  min_request_frac: 0\n  cache_state: any\n")
	st := store.NewMemory(store.Options{MaxEntries: 400})
	main := threadCtx("thr-flight", thread.Primary, st, "warm")
	sub := threadCtx("thr-flight", "agent-1", st, "warm")
	span := caTranscript(2)
	call := func(context.Context) (string, error) { return "<summary>main work.</summary>", nil }

	j, ok := inFlight.begin(convKey(sub))
	if !ok {
		t.Fatal("could not claim the subagent's flight")
	}
	defer inFlight.finish(convKey(sub), j)

	if got := s.commissionSync(sub, call, "test", false, span, len(span), time.Second); got.Reason != KeepAliveReasonInFlight {
		t.Fatalf("the subagent's own flight did not block it: reason %q", got.Reason)
	}
	if got := s.commissionSync(main, call, "test", false, span, len(span), time.Second); !got.Committed {
		t.Fatalf("the subagent's flight blocked the main thread: reason %q", got.Reason)
	}
	if gate := s.startAsyncSummary(main, call, "test", "test", false, span, len(span)); gate != "" {
		t.Fatalf("the subagent's flight blocked the main thread's detached commission: gate %q", gate)
	}
	WaitForSummaryForTest(convKey(main), 5*time.Second)
}

// A deferred cold commission resolves only on its own thread's forwarded request. A subagent's
// request did not rewrite the main thread's cache, so it must not release the main thread's
// deferral.
func TestColdDeferralResolvesOnlyItsOwnThread(t *testing.T) {
	s := newCacheAware(t, "keep_last_turns: 1\nmin_tokens: 10\ninstruction_role: user\n"+
		"trigger:\n  min_request_frac: 0\n  cache_state: any\n")
	model := &threadModel{out: "<summary>explored.</summary>"}
	s.modelClient = model
	st := store.NewMemory(store.Options{MaxEntries: 400})
	main := threadCtx("thr-cold", thread.Primary, st, "cold")
	sub := threadCtx("thr-cold", "agent-1", st, "cold")

	for _, c := range []*components.Ctx{main, sub} {
		req := &bschemas.BifrostChatRequest{Input: caTranscript(3)}
		var rep components.Report
		if _, err := s.Offload(req, &rep, c); err != nil {
			t.Fatalf("Offload must fail open: %v", err)
		}
		if rep.Events["cold_commission_deferred"] == 0 {
			t.Fatalf("thread %q did not defer on a cold cache (gates: %v)", c.Thread, rep.Gates)
		}
	}
	// Two deferrals, one per thread: before #430 the second replaced the first.
	ResolveDeferredCacheAwareSummary(sub.Session, sub.Thread)
	if !WaitForSummaryForTest(convKey(sub), 5*time.Second) {
		t.Fatal("the subagent's deferred summary never resolved")
	}
	if n := model.calls.Load(); n != 1 {
		t.Fatalf("model calls after resolving the subagent = %d, want 1", n)
	}
	if _, ok := loadCheckpoint(main); ok {
		t.Fatal("resolving the subagent's deferral wrote the main thread's checkpoint")
	}
	ResolveDeferredCacheAwareSummary(main.Session, main.Thread)
	if !WaitForSummaryForTest(convKey(main), 5*time.Second) {
		t.Fatal("the main thread's deferred summary never resolved")
	}
	if n := model.calls.Load(); n != 2 {
		t.Fatalf("model calls after resolving both = %d, want 2", n)
	}
	if _, ok := loadCheckpoint(main); !ok {
		t.Fatal("the main thread's deferral, still pending after the subagent's resolved, wrote no checkpoint")
	}
}

// A new thread that continues another thread's prefix (Ctx.ThreadInherits, set on its first request
// only) reads the parent's checkpoint, and never writes it. A tool-less side request reads it
// read-only and stores nothing. A fork that declares tools keeps a copy under its own key, so its
// later requests, which no longer carry ThreadInherits, still find it.
func TestInheritingThreadReadsTheParentCheckpointAndNeverWritesIt(t *testing.T) {
	st := store.NewMemory(store.Options{MaxEntries: 100})
	const sess = "thr-inherit"
	main := threadCtx(sess, thread.Primary, st, "unknown")
	saveCheckpoint(main, sumCheckpoint{SummaryMsg: "main", CoveredCount: 3})

	side := threadCtx(sess, "p:side", st, "unknown")
	side.ThreadInherits = true // ThreadParent "" is the main thread
	adoptParentCheckpoint(side)
	if cp, ok := loadCheckpoint(side); !ok || cp.SummaryMsg != "main" {
		t.Fatalf("a side request did not read its parent's checkpoint: %q %v", cp.SummaryMsg, ok)
	}
	if store.Peek(st, sumKey(convKey(side))) {
		t.Fatal("a tool-less side request stored a copy of its parent's checkpoint")
	}

	fork := threadCtx(sess, "p:fork", st, "unknown")
	fork.ThreadInherits, fork.DeclaresTools = true, true
	adoptParentCheckpoint(fork)
	saveCheckpoint(fork, sumCheckpoint{SummaryMsg: "fork", CoveredCount: 7})
	if cp, _ := loadCheckpoint(main); cp.SummaryMsg != "main" {
		t.Fatalf("a fork's save changed its parent's checkpoint: %q", cp.SummaryMsg)
	}
	next := threadCtx(sess, "p:fork", st, "unknown") // the fork's second request: no ThreadInherits
	if cp, ok := loadCheckpoint(next); !ok || cp.SummaryMsg != "fork" {
		t.Fatalf("the fork's next request lost its checkpoint: %q %v", cp.SummaryMsg, ok)
	}

	fresh := threadCtx(sess, "p:fresh-fork", st, "unknown")
	fresh.ThreadInherits, fresh.DeclaresTools = true, true
	adoptParentCheckpoint(fresh)
	if cp, ok := loadCheckpoint(threadCtx(sess, "p:fresh-fork", st, "unknown")); !ok || cp.SummaryMsg != "main" {
		t.Fatalf("a fork's copy of its parent's checkpoint is missing on its next request: %q %v", cp.SummaryMsg, ok)
	}

	stranger := threadCtx(sess, "a:other", st, "unknown") // a new thread that inherits nothing
	if _, ok := loadCheckpoint(stranger); ok {
		t.Fatal("a thread that inherits nothing read another thread's checkpoint")
	}
}

// A tool-less side request (an end-of-turn note carrying the whole conversation) on a turn where the
// trigger fires and the parent's checkpoint is stale must not pay for a summary of its own: its
// thread id is new every time, so that summary would never be reused, and the main thread commissions
// its own anyway. It splices the parent's checkpoint and stops: no commission, no cold deferral, no
// keep-alive candidate. The main thread's next request still commissions exactly one summary.
func TestCacheAwareSideRequestIsReplayOnly(t *testing.T) {
	s := newCacheAware(t, "keep_last_turns: 1\nmin_tokens: 10\nresummarize_tokens: 50\ninstruction_role: user\n"+
		"trigger:\n  min_request_frac: 0\n  cache_state: any\n")
	model := &threadModel{out: "<summary>main work.</summary>"}
	s.modelClient = model
	st := store.NewMemory(store.Options{MaxEntries: 400})
	const sess = "thr-side"
	t.Cleanup(func() { ClearKeepAliveCandidate(thread.Key(sess, thread.Primary)) })

	main := threadCtx(sess, thread.Primary, st, "warm")
	main.DeclaresTools = true
	var rep components.Report
	if _, err := s.Offload(&bschemas.BifrostChatRequest{Input: caTranscript(3)}, &rep, main); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	WaitForSummaryForTest(convKey(main), 5*time.Second)
	if _, ok := loadCheckpoint(main); !ok || model.calls.Load() != 1 {
		t.Fatalf("precondition: main thread has no checkpoint (calls %d, gates %v)", model.calls.Load(), rep.Gates)
	}

	long := caTranscript(12) // the tail past main's checkpoint is well over resummarize_tokens: stale
	for _, phase := range []string{"warm", "cold"} {
		side := threadCtx(sess, "p:side-"+phase, st, phase)
		side.ThreadInherits = true // parent "" is the main thread; DeclaresTools false
		in := append(append([]bschemas.ChatMessage(nil), long...),
			caMsg(bschemas.ChatMessageRoleUser, "write a one-line title for this conversation"))
		req := &bschemas.BifrostChatRequest{Input: in}
		var srep components.Report
		if _, err := s.Offload(req, &srep, side); err != nil {
			t.Fatalf("Offload must fail open: %v", err)
		}
		if srep.Gates["side_request_read_only"] == 0 {
			t.Fatalf("%s side request: no side_request_read_only gate (gates %v)", phase, srep.Gates)
		}
		if len(req.Input) >= len(in) {
			t.Errorf("%s side request did not splice its parent's checkpoint (%d -> %d messages)", phase, len(in), len(req.Input))
		}
		key := convKey(side)
		if _, running := inFlight.peek(key); running {
			t.Errorf("%s side request started a commission", phase)
		}
		coldDeferredMu.Lock()
		_, deferred := coldDeferred[key]
		coldDeferredMu.Unlock()
		if deferred {
			t.Errorf("%s side request deferred a cold commission", phase)
		}
		if _, _, _, ok := KeepAliveSubstitute(key); ok {
			ClearKeepAliveCandidate(key)
			t.Errorf("%s side request registered a keep-alive candidate", phase)
		}
		if store.Peek(st, sumKey(key)) {
			t.Errorf("%s side request stored a checkpoint", phase)
		}
	}
	time.Sleep(50 * time.Millisecond) // a commission that slipped through would have called the model by now
	if n := model.calls.Load(); n != 1 {
		t.Fatalf("model calls after the side requests = %d, want 1", n)
	}

	next := threadCtx(sess, thread.Primary, st, "warm")
	next.DeclaresTools = true
	var nrep components.Report
	if _, err := s.Offload(&bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), long...)}, &nrep, next); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	WaitForSummaryForTest(convKey(next), 5*time.Second)
	if n := model.calls.Load(); n != 2 {
		t.Fatalf("the main thread's next request made %d model calls in total, want 2 (gates %v)", n, nrep.Gates)
	}
}

// summarize's async path is reachable by a side request too. It must replay, never commission.
func TestSummarizeSideRequestIsReplayOnly(t *testing.T) {
	s := newSummarizeReusing(t, 1_000_000)
	st := store.NewMemory(store.Options{})
	side := &components.Ctx{Ctx: context.Background(), Session: "sum-side", Thread: "p:side",
		ThreadInherits: true, Store: st, CtxWindow: 1_000_000}
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		sysMsg("you are a coding agent"),
		userMsg("Fix the failing handler in src/mod/file.go and run the tests."),
		callMsg("t_a"),
		bulkResult("t_a"),
		assistantMsg("Next I will run the tests."),
		userMsg("write a one-line title for this conversation"),
	}}
	var rep components.Report
	if _, err := s.Offload(req, &rep, side); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	if rep.Gates["side_request_read_only"] == 0 {
		t.Fatalf("no side_request_read_only gate (gates %v)", rep.Gates)
	}
	if _, running := inFlight.peek(convKey(side)); running {
		t.Fatal("a side request started a summarize commission")
	}
	if store.Peek(st, sumKey(convKey(side))) {
		t.Fatal("a side request stored a checkpoint")
	}
}
