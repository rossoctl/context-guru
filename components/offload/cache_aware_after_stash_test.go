package offload

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
)

// turnStash models the host's stash the way proxy's sentStash behaves: it holds the message count
// of the last body put for the thread, and AskCovered refuses a span longer than that, as
// snapshotCovering does. A turn's own body is put only when the host calls put, which is after its
// upstream answers 200 — after Offload has returned.
type turnStash struct {
	mu      sync.Mutex
	stashed int // messages in the stashed body; 0 = nothing stashed
	calls   int64
	out     string
}

func (a *turnStash) put(n int) { a.mu.Lock(); a.stashed = n; a.mu.Unlock() }

func (a *turnStash) Ask(context.Context, string, string) (string, components.PrefixUsage, error) {
	return "", components.PrefixUsage{}, components.ErrStalePrefix // AskCovered is the path used
}

func (a *turnStash) AskCovered(_ context.Context, _ string, count int, _ string, _ string) (string, components.PrefixUsage, error) {
	a.mu.Lock()
	n := a.stashed
	a.mu.Unlock()
	switch {
	case n == 0:
		return "", components.PrefixUsage{}, components.ErrNoPrefix
	case count > n:
		return "", components.PrefixUsage{}, components.ErrStalePrefix
	}
	atomic.AddInt64(&a.calls, 1)
	return a.out, components.PrefixUsage{CacheRead: 1000}, nil
}

// #722. A detached prefix-ask commission on a normal (warm) turn must commit. It used to start at
// Offload time, about 1 ms before the host stashed this turn's body, so the stash still held the
// previous turn's shorter body and the coverage check refused it: every detached summary ended
// stale_prefix, and only the keep-alive substitute could ever commit one.
func TestCacheAwareDetachedPrefixAskCommitsAfterTheTurnIsStashed(t *testing.T) {
	s := newCacheAware(t, caBaseCfg+"instruction_role: auto\n")
	asker := &turnStash{out: "<summary>explored the handler via prefix ask.</summary>"}
	ctx := caCtxNoModel("ca-after-stash", asker)
	in := caFixture()
	staleBefore := CacheAwareSummarizerStalePrefix()

	// The previous turn's body is what the host holds when this turn arrives. The span to summarize
	// always reaches into this turn's new messages, so that body is shorter than the span: one message
	// short of it. (Live, on a clean session, it was 7 required against 5 stashed; this fixture needs
	// end messages and stashes end-1.)
	_, start, end := summarizeSpan(in, 1, s.keepLastTurns, nil)
	if end <= start+1 {
		t.Fatalf("fixture span [%d,%d) is too short to test", start, end)
	}
	asker.put(end - 1)
	req := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), in...)}
	var rep components.Report
	if _, err := s.Offload(req, &rep, ctx); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	// Live, the upstream round trip takes seconds and a detached call starts in about 1 ms, so
	// anything started at Offload time has finished before this turn's body is stashed. Wait for it
	// here, then let the host stash this turn's body (its upstream answered 200) and run its hook as
	// the request ends.
	WaitForSummaryForTest(convKey(ctx), 5*time.Second)
	asker.put(len(in))
	ResolveDeferredCacheAwareSummary(ctx.Session, ctx.Thread)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := loadCheckpoint(ctx); ok {
			break
		}
		WaitForSummaryForTest(ctx.Session, 100*time.Millisecond)
		time.Sleep(5 * time.Millisecond)
	}
	if n := CacheAwareSummarizerStalePrefix() - staleBefore; n != 0 {
		t.Fatalf("the detached commission ended stale_prefix %d time(s): it read the previous turn's "+
			"stash instead of this turn's", n)
	}
	if _, ok := loadCheckpoint(ctx); !ok {
		t.Fatalf("no checkpoint was committed (asks: %d, gates %v, events %v)",
			atomic.LoadInt64(&asker.calls), rep.Gates, rep.Events)
	}
}

// #724 review, finding 1. A host that sets PrefixAsk but never calls the hook must still get its
// warm commissions dispatched. When a newer request on the thread commissions again, the older
// deferral is dispatched at once, since that older request has ended and its body is stashed;
// dropping it and restarting the fallback would starve a thread whose turns come less than the
// fallback apart.
func TestAReplacedWarmDeferralIsDispatchedWithoutTheHook(t *testing.T) {
	s := newCacheAware(t, caBaseCfg+"instruction_role: auto\n")
	asker := &turnStash{out: "<summary>explored the handler via prefix ask.</summary>"}
	ctx := caCtxNoModel("ca-replaced-warm", asker)
	in := caFixture()
	_, _, end := summarizeSpan(in, 1, s.keepLastTurns, nil)
	asker.put(end - 1)
	turn := func(msgs []bschemas.ChatMessage) {
		t.Helper()
		req := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), msgs...)}
		var rep components.Report
		if _, err := s.Offload(req, &rep, ctx); err != nil {
			t.Fatalf("Offload: %v", err)
		}
	}
	turn(in)           // commissions, deferred
	asker.put(len(in)) // its request ended: the body is stashed (no hook is ever called)
	turn(append(append([]bschemas.ChatMessage(nil), in...), caMsg(bschemas.ChatMessageRoleUser, "and then?")))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := loadCheckpoint(ctx); ok {
			break
		}
		WaitForSummaryForTest(ctx.Session, 100*time.Millisecond)
		time.Sleep(5 * time.Millisecond)
	}
	if _, ok := loadCheckpoint(ctx); !ok {
		t.Fatal("no checkpoint: the replaced warm deferral was dropped instead of dispatched")
	}
	takeWarmDeferral(convKey(ctx)) // the newer one; nothing else will resolve it in this test
}

// #724 review, finding 2. Warm deferrals have their own bound, so a full cold registry does not
// force a warm commission to dispatch early, against the previous turn's stash.
func TestAWarmCommissionDefersWhenTheColdRegistryIsFull(t *testing.T) {
	s := newCacheAware(t, caBaseCfg+"instruction_role: auto\n")
	coldDeferredMu.Lock()
	for i := 0; len(coldDeferred) < maxColdDeferred; i++ {
		coldDeferred["fill-"+strconv.Itoa(i)] = &coldDeferral{}
	}
	coldDeferredMu.Unlock()
	t.Cleanup(func() {
		coldDeferredMu.Lock()
		for k := range coldDeferred {
			if strings.HasPrefix(k, "fill-") {
				delete(coldDeferred, k)
			}
		}
		coldDeferredMu.Unlock()
	})
	asker := &turnStash{out: "<summary>x</summary>"}
	ctx := caCtxNoModel("ca-warm-own-bound", asker)
	in := caFixture()
	_, _, end := summarizeSpan(in, 1, s.keepLastTurns, nil)
	asker.put(end - 1)
	req := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), in...)}
	var rep components.Report
	if _, err := s.Offload(req, &rep, ctx); err != nil {
		t.Fatalf("Offload: %v", err)
	}
	d := takeWarmDeferral(convKey(ctx))
	if d == nil {
		t.Fatal("the warm commission was not deferred while the cold registry was full")
	}
	if atomic.LoadInt64(&asker.calls) != 0 {
		t.Fatal("the warm commission was dispatched early")
	}
}

// #724 review (reviewer 1). Between Offload and the hook the idle keep-alive can commit the same
// span; the deferred commission must then not pay for it again.
func TestADeferredCommissionSkipsASpanACheckpointAlreadyCovers(t *testing.T) {
	s := newCacheAware(t, caBaseCfg+"instruction_role: auto\n")
	asker := &turnStash{out: "<summary>x</summary>"}
	ctx := caCtxNoModel("ca-skip-dup", asker)
	in := caFixture()
	_, start, end := summarizeSpan(in, 1, s.keepLastTurns, nil)
	asker.put(len(in))
	req := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), in...)}
	var rep components.Report
	if _, err := s.Offload(req, &rep, ctx); err != nil {
		t.Fatalf("Offload: %v", err)
	}
	// The keep-alive committed this span meanwhile.
	saveCheckpoint(ctx, sumCheckpoint{SummaryMsg: "<summary>k</summary>", CoveredCount: end - start,
		CoveredHash: spanHash(in[start:end])})
	ResolveDeferredCacheAwareSummary(ctx.Session, ctx.Thread)
	WaitForSummaryForTest(ctx.Session, time.Second)
	if n := atomic.LoadInt64(&asker.calls); n != 0 {
		t.Fatalf("the deferred commission paid %d call(s) for a span a checkpoint already covers", n)
	}
}
