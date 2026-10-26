package offload

import (
	"sync/atomic"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/thread"
	"github.com/rossoctl/context-guru/store"
)

type threadCoverageAsker struct {
	fakePrefixAsker
	coverSession atomic.Value
}

func (c *threadCoverageAsker) CoversSpan(session string, _ int, _ string) bool {
	c.coverSession.Store(session)
	return true
}

// #423: the host stashes each THREAD's last body apart, so cache_aware_summarizer must ask, and
// check coverage, under thread.Key(session, thread). Under the bare session a subagent's
// commission read the main thread's body: ErrStalePrefix at best, the wrong conversation at worst.
func TestCacheAwareAsksUnderTheThreadKey(t *testing.T) {
	s := newCacheAware(t, caBaseCfg+"instruction_role: auto\n")
	asker := &threadCoverageAsker{fakePrefixAsker: fakePrefixAsker{out: "<summary>subagent work.</summary>"}}
	ctx := caCtxNoModel("ca-thread-key", asker)
	ctx.Thread = "a:S"
	req := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), caFixture()...)}
	var rep components.Report
	if _, err := s.Offload(req, &rep, ctx); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	// The host's hook: this turn's body is stashed, the request ends (#722).
	ResolveDeferredCacheAwareSummary(ctx.Session, ctx.Thread)
	if !WaitForSummaryForTest(convKey(ctx), 5*time.Second) {
		t.Fatal("the commissioned summary never resolved")
	}
	if atomic.LoadInt64(&asker.calls) == 0 {
		t.Fatalf("precondition: no prefix ask was made (gates %v), so nothing is under test", rep.Gates)
	}
	want := thread.Key("ca-thread-key", "a:S")
	if asker.gotSession != want {
		t.Errorf("Ask used key %q, want the thread key %q", asker.gotSession, want)
	}
	if got, _ := asker.coverSession.Load().(string); got != want {
		t.Errorf("CoversSpan used key %q, want the thread key %q", got, want)
	}
}

// The same for extract_llm_sweep's one prefix ask.
func TestSweepAsksUnderTheThreadKey(t *testing.T) {
	asker := &labelAsker{verdict: "drop", needed: "none"}
	asker.cacheRead = 19595
	e := newSweepSmall(t, "")
	req := sweepReqStocked()
	ctx := preExpiryCtx("s", asker, store.NewMemory(store.Options{}))
	ctx.Thread = "a:S"
	var rep components.Report
	if _, err := askOffload(e, req, &rep, ctx); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	if n := atomic.LoadInt64(&asker.calls); n != 1 {
		t.Fatalf("precondition: expected one prefix ask, got %d (gates %v)", n, rep.Gates)
	}
	if want := thread.Key("s", "a:S"); asker.sess() != want {
		t.Errorf("the ask used key %q, want the thread key %q", asker.sess(), want)
	}
}
