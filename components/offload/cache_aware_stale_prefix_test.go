package offload

import (
	"sync/atomic"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
)

// coverageAsker is fakePrefixAsker plus components.PrefixCoverage, so a test can drive the
// stale-prefix guard without needing a real proxy stash. covers is read fresh on every call,
// which is what lets TestCacheAwareDeclinesOnAStalePrefix flip it mid-test the way a real
// session's stash can go from not-yet-covering to covering between the turn that commissions
// and the detached call that actually runs.
type coverageAsker struct {
	fakePrefixAsker
	covers           bool
	gotCount         int
	gotHash          string
	coversSpanCalled int64
}

func (c *coverageAsker) CoversSpan(session string, count int, hash string) bool {
	atomic.AddInt64(&c.coversSpanCalled, 1)
	c.gotCount, c.gotHash = count, hash
	return c.covers
}

// ⭐ PR #402 review, finding 1 (High): a PrefixAsk commission used to answer from WHATEVER body
// the host's stash happened to hold, with nothing checking it was the right one. The guard lives
// in the CALL itself (the closure startAsyncSummary/commissionSync eventually invoke), not
// synchronously inside Offload — Offload only ever sees, at best, the PREVIOUS turn's stashed
// body (the host stashes THIS turn's own body only after Offload has already returned), so a
// check made there would decline every ordinary turn whose span simply grew past what that
// older stash held, not only a genuinely stale one. Checking immediately before the call
// actually pays for anything is both correct (by then the host has had the chance to stash at
// least this turn's own body) and cheap (no HTTP round trip for a stale read). This test drives
// that check directly, at the components.PrefixCoverage seam, rather than through a real
// oversized HTTP body — package proxy has the same failure reproduced through a real serve()
// call against the real sentStash.
func TestCacheAwareDeclinesOnAStalePrefix(t *testing.T) {
	s := newCacheAware(t, caBaseCfg+"instruction_role: auto\n")
	asker := &coverageAsker{fakePrefixAsker: fakePrefixAsker{
		out: "<summary>should never be committed.</summary>"}}
	asker.covers = false // the stash does not cover the span about to be summarized
	ctx := caCtxNoModel("ca-stale-prefix", asker)

	before := CacheAwareSummarizerStalePrefix()
	noPrefixBefore := CacheAwareSummarizerNoPrefix()
	in := caFixture()
	req := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), in...)}
	var rep components.Report
	if _, err := s.Offload(req, &rep, ctx); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	if !WaitForSummaryForTest(ctx.Session, 5*time.Second) {
		t.Fatal("the commissioned (and declined) summary never resolved")
	}
	if atomic.LoadInt64(&asker.coversSpanCalled) == 0 {
		t.Fatal("CoversSpan was never consulted — the guard did not run at all")
	}
	if atomic.LoadInt64(&asker.calls) != 0 {
		t.Fatalf("Ask was called %d times; a stale prefix must decline BEFORE paying for the call",
			asker.calls)
	}
	if CacheAwareSummarizerStalePrefix() != before+1 {
		t.Error("the stale-prefix decline was not counted")
	}
	if CacheAwareSummarizerNoPrefix() != noPrefixBefore {
		t.Error("a STALE prefix (something stashed, just not enough) was miscounted as NO prefix " +
			"(nothing stashed) — the two need different attention")
	}
	if _, ok := loadCheckpoint(ctx); ok {
		t.Fatal("a checkpoint was committed from a prefix the guard itself said was stale")
	}

	// Flip the stash to covering: the SAME span, asked again on a later turn, now proceeds —
	// proving the decline above was this guard specifically, not some other precondition.
	asker.covers = true
	req2 := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), in...)}
	var rep2 components.Report
	if _, err := s.Offload(req2, &rep2, ctx); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	if !WaitForSummaryForTest(ctx.Session, 5*time.Second) {
		t.Fatal("the commissioned summary never landed once the prefix covered the span")
	}
	if atomic.LoadInt64(&asker.calls) != 1 {
		t.Fatalf("Ask was called %d times once the prefix covered the span, want 1", asker.calls)
	}
}
