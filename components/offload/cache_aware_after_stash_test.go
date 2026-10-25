package offload

import (
	"context"
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
	// short of it, the closest the live case comes (7 required against 5 stashed, on a clean session).
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
