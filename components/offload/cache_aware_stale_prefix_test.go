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
	// The host's hook: this turn's body is stashed, the request ends (#722).
	ResolveDeferredCacheAwareSummary(ctx.Session, ctx.Thread)
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
	// The host's hook: this turn's body is stashed, the request ends (#722).
	ResolveDeferredCacheAwareSummary(ctx.Session, ctx.Thread)
	if !WaitForSummaryForTest(ctx.Session, 5*time.Second) {
		t.Fatal("the commissioned summary never landed once the prefix covered the span")
	}
	if atomic.LoadInt64(&asker.calls) != 1 {
		t.Fatalf("Ask was called %d times once the prefix covered the span, want 1", asker.calls)
	}
}

// A count-changing component BEFORE the summarizer declines through the same guard, but it is
// NOT a stale stash (#452): the raw and pipeline coordinates name different messages, so
// rawSpanForHash cannot locate the span at all and the call returns components.ErrCoordsShifted.
// The stash itself can be perfectly current — on a `high`/`xhigh` pipeline this is the expected
// decline of every compacted turn — so it is counted apart from the stale-prefix counter, which
// keeps meaning "the stashed body was the wrong one".
func TestCacheAwareCountsShiftedCoordinatesApartFromAStalePrefix(t *testing.T) {
	s := newCacheAware(t, caBaseCfg+"instruction_role: auto\n")
	asker := &coverageAsker{fakePrefixAsker: fakePrefixAsker{
		out: "<summary>should never be committed.</summary>"}}
	asker.covers = true // covering: only the coordinate shift can decline
	ctx := caCtxNoModel("ca-coords-shifted", asker)
	// The pre-pipeline body holds one message more than req.Input does at this component's own
	// position — what a count-changing summarize ahead of it leaves behind.
	in := caFixture()
	raw := make([]bschemas.ChatMessage, len(in), len(in)+1)
	copy(raw, in)
	ctx.RawInput = append(raw, in[0])

	before := CacheAwareSummarizerCoordsShifted()
	staleBefore := CacheAwareSummarizerStalePrefix()
	req := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), in...)}
	var rep components.Report
	if _, err := s.Offload(req, &rep, ctx); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	// The host's hook: this turn's body is stashed, the request ends (#722).
	ResolveDeferredCacheAwareSummary(ctx.Session, ctx.Thread)
	if !WaitForSummaryForTest(ctx.Session, 5*time.Second) {
		t.Fatal("the commissioned (and declined) summary never resolved")
	}
	if atomic.LoadInt64(&asker.coversSpanCalled) != 0 {
		t.Fatal("CoversSpan was consulted; with the coordinates shifted there is no span safe to " +
			"compare, so the stash must not be judged at all")
	}
	if atomic.LoadInt64(&asker.calls) != 0 {
		t.Fatalf("Ask was called %d times; a guard that cannot locate the span must decline before "+
			"paying for the call", asker.calls)
	}
	if CacheAwareSummarizerCoordsShifted() != before+1 {
		t.Error("the coords-shifted decline was not counted")
	}
	if CacheAwareSummarizerStalePrefix() != staleBefore {
		t.Error("a shifted coordinate system was miscounted as a stale stash — the stash was " +
			"never judged, so it cannot have been found stale")
	}
	if _, ok := loadCheckpoint(ctx); ok {
		t.Fatal("a checkpoint was committed from a span the guard could not locate")
	}
}

// coveredAsker also implements components.PrefixCoveredAsker. Its CoversSpan always says
// "covered", as a stash read a moment too early would; AskCovered reports what the single
// snapshot it asks on really holds. The guard must follow AskCovered.
type coveredAsker struct {
	fakePrefixAsker
	snapshotErr   error
	askCoveredHit int64
	coversSpanHit int64
}

func (c *coveredAsker) CoversSpan(string, int, string) bool {
	atomic.AddInt64(&c.coversSpanHit, 1)
	return true
}

func (c *coveredAsker) AskCovered(_ context.Context, _ string, _ int, _, _ string) (string, components.PrefixUsage, error) {
	atomic.AddInt64(&c.askCoveredHit, 1)
	if c.snapshotErr != nil {
		return "", components.PrefixUsage{}, c.snapshotErr
	}
	return c.out, c.usage, nil
}

func TestCacheAwareChecksAndAsksOnOneSnapshot(t *testing.T) {
	s := newCacheAware(t, caBaseCfg+"instruction_role: auto\n")
	asker := &coveredAsker{fakePrefixAsker: fakePrefixAsker{out: "<summary>never committed.</summary>"},
		snapshotErr: components.ErrStalePrefix}
	ctx := caCtxNoModel("ca-one-snapshot", asker)

	before := CacheAwareSummarizerStalePrefix()
	req := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), caFixture()...)}
	var rep components.Report
	if _, err := s.Offload(req, &rep, ctx); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	// The host's hook: this turn's body is stashed, the request ends (#722).
	ResolveDeferredCacheAwareSummary(ctx.Session, ctx.Thread)
	if !WaitForSummaryForTest(ctx.Session, 5*time.Second) {
		t.Fatal("the commissioned (and declined) summary never resolved")
	}
	if atomic.LoadInt64(&asker.askCoveredHit) == 0 {
		t.Fatal("AskCovered was never used although the asker implements it")
	}
	if atomic.LoadInt64(&asker.coversSpanHit) != 0 || atomic.LoadInt64(&asker.calls) != 0 {
		t.Fatalf("the separate CoversSpan (%d) / Ask (%d) path ran; check and ask must be one call",
			asker.coversSpanHit, asker.calls)
	}
	if CacheAwareSummarizerStalePrefix() != before+1 {
		t.Error("the stale snapshot was not counted as a stale-prefix decline")
	}
	if _, ok := loadCheckpoint(ctx); ok {
		t.Fatal("a checkpoint was committed from a snapshot AskCovered refused")
	}

	asker.snapshotErr = nil // the snapshot covers the span: the same commission now lands
	req2 := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), caFixture()...)}
	var rep2 components.Report
	if _, err := s.Offload(req2, &rep2, ctx); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	// The host's hook: this turn's body is stashed, the request ends (#722).
	ResolveDeferredCacheAwareSummary(ctx.Session, ctx.Thread)
	if !WaitForSummaryForTest(ctx.Session, 5*time.Second) {
		t.Fatal("the commissioned summary never landed")
	}
	if _, ok := loadCheckpoint(ctx); !ok {
		t.Fatal("no checkpoint after AskCovered succeeded")
	}
}

// stashAsker models the host's stash: the thread's next request replaces the body right after the
// coverage check approved it. Separate CoversSpan then Ask therefore asks over the replacement;
// AskCovered asks over the body it checked.
type stashAsker struct {
	mu          sync.Mutex
	body        string
	replacement string
	askedOver   string
	out         string
}

func (a *stashAsker) swap() {
	a.mu.Lock()
	a.body = a.replacement
	a.mu.Unlock()
}

func (a *stashAsker) CoversSpan(string, int, string) bool {
	a.mu.Lock()
	covered := a.body == "approved body"
	a.mu.Unlock()
	a.swap() // the replacement lands after the check, before the ask
	return covered
}

func (a *stashAsker) Ask(context.Context, string, string) (string, components.PrefixUsage, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.askedOver = a.body
	return a.out, components.PrefixUsage{}, nil
}

func (a *stashAsker) AskCovered(context.Context, string, int, string, string) (string, components.PrefixUsage, error) {
	a.mu.Lock()
	snapshot := a.body
	a.mu.Unlock()
	a.swap()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.askedOver = snapshot
	return a.out, components.PrefixUsage{}, nil
}

// The ask must carry the body the coverage check approved, even if the stash is replaced between
// the check and the ask. Before the single-snapshot ask this asked over the replacement.
func TestCacheAskCarriesTheApprovedBodyWhenTheStashIsReplaced(t *testing.T) {
	s := newCacheAware(t, caBaseCfg+"instruction_role: auto\n")
	asker := &stashAsker{body: "approved body", replacement: "replacement body from the next request",
		out: "<summary>covers the approved span.</summary>"}
	ctx := caCtxNoModel("ca-approved-body", asker)

	req := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), caFixture()...)}
	var rep components.Report
	if _, err := s.Offload(req, &rep, ctx); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	// The host's hook: this turn's body is stashed, the request ends (#722).
	ResolveDeferredCacheAwareSummary(ctx.Session, ctx.Thread)
	if !WaitForSummaryForTest(ctx.Session, 5*time.Second) {
		t.Fatal("the commissioned summary never resolved")
	}
	asker.mu.Lock()
	defer asker.mu.Unlock()
	if asker.askedOver != "approved body" {
		t.Fatalf("the ask went out over %q, want the body the coverage check approved", asker.askedOver)
	}
}
