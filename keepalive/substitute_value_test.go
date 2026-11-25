package keepalive

import (
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/store"
)

// A GPT-5.5 summary at 600K tokens costs more than the old flat $0.50 and protects a miss of about
// $5.40. The value rule lets it through (#1439).
func TestGPT55SummaryAt600KPassesTheValueRule(t *testing.T) {
	k, fd, _ := testKeeper(t)
	fd.setPrices(proxyPricer{fixedPrice{gpt55()}})
	cost := k.projectedSummaryUSD("gpt-5.5", "", 600_000)
	miss := k.projectedMissUSD("gpt-5.5", 600_000, "", false)
	if cost <= DefaultMaxUSDPerPing {
		t.Fatalf("the fixture must exceed the old flat cap: summary $%.3f", cost)
	}
	if v := (Policy{}).SubstituteVerdict(cost, miss); v != VerdictOK {
		t.Errorf("verdict %v, want OK (summary $%.3f, miss $%.3f)", v, cost, miss)
	}
}

// A call that costs as much as the miss, or more, is refused. An explicit operator cap still
// refuses. An unpriced call and an unknown miss are allowed.
func TestSubstituteVerdictRule(t *testing.T) {
	p := Policy{}
	if v := p.SubstituteVerdict(1.2, 1.0); v != VerdictNotWorth || v.Reason() != ReasonNotWorth {
		t.Errorf("cost over miss: %v", v)
	}
	if v := p.SubstituteVerdict(1.0, 1.0); v != VerdictNotWorth {
		t.Errorf("cost equal to the miss: %v, want refused", v)
	}
	if v := p.SubstituteVerdict(0.9, 1.0); v != VerdictOK {
		t.Errorf("cost under miss: %v", v)
	}
	if v := (Policy{MaxUSDPerPing: 0.5}).SubstituteVerdict(0.6, 5.4); v != VerdictOverCap || v.Reason() != ReasonOverBudget {
		t.Errorf("explicit cap: %v, want OverCap", v)
	}
	if v := (Policy{MaxUSDPerPing: -1}).SubstituteVerdict(0.6, 5.4); v != VerdictOK {
		t.Errorf("negative cap: %v, want OK", v)
	}
	if v := p.SubstituteVerdict(0, 0); v != VerdictOK {
		t.Errorf("unpriced: %v, want OK", v)
	}
	if v := p.SubstituteVerdict(0.7, 0); v != VerdictOK {
		t.Errorf("unknown miss: %v, want OK", v)
	}
	k, fd, _ := testKeeper(t)
	fd.setPrices(proxyPricer{fixedPrice{modelinfo.Price{}}})
	if got := k.projectedSummaryUSD("m", "", 600_000); got != 0 {
		t.Errorf("unpriced summary projects %v, want 0", got)
	}
}

// The summary of a long session costs more than $0.50 and less than the miss: it is sent, with no
// explicit cap set. The flat cap refused it before.
func TestSummaryAboveTheOldFlatCapIsSentWhenItIsWorthIt(t *testing.T) {
	k, _, clock := testKeeper(t)
	// 20k prefix: ping $0.60, summary about $0.60 + output, miss $5.40.
	fakeOf(k).setPrices(proxyPricer{fixedPrice{modelinfo.Price{Input: 3e-4, CacheRead: 3e-5, CacheWrite: 3e-4, Output: 1e-7}}})
	st := store.NewMemory(store.Options{MaxEntries: 400})
	model := &fakeSummaryModel{out: "<summary>ok</summary>"}
	kaCandidate(t, "sess-1", st, model)

	pol := kaPolicy()
	pol.MaxUSDPerPing = 0 // unset: no flat cap
	at := clock.now()
	rec(k, pol, sess1, at.Add(-time.Second), kaToolBody, http.StatusOK, Usage{CacheRead: 20000}, true)
	rec(k, pol, sess1, at, kaToolBody, http.StatusOK, Usage{CacheRead: 20000}, true)
	if k.Stats().Live != 1 {
		t.Fatalf("the session was not tracked (live=%d)", k.Stats().Live)
	}
	if c := k.projectedSummaryUSD("claude-sonnet-5", "", 20000); c <= DefaultMaxUSDPerPing {
		t.Fatalf("fixture: summary $%.3f must exceed the old flat cap", c)
	}
	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
	if model.calls.Load() != 1 || k.summarySubstituted.Load() != 1 || k.summarySubstituteOverBudget.Load() != 0 {
		t.Errorf("model calls %d, substituted %d, over-budget %d, want 1, 1, 0",
			model.calls.Load(), k.summarySubstituted.Load(), k.summarySubstituteOverBudget.Load())
	}
}

// A summary that costs more than the miss is refused and the plain ping goes out in its place. The
// chain does not end.
func TestSummaryCostingMoreThanTheMissIsRefusedAndThePingStillGoes(t *testing.T) {
	k, fs, clock := testKeeper(t)
	// ping about $0.0076; chain of 2 about $0.0152 < miss $0.02 < summary about $0.0456.
	fakeOf(k).setPrices(proxyPricer{fixedPrice{modelinfo.Price{Input: 1.38e-6, CacheRead: 3.8e-7, CacheWrite: 1.38e-6, Output: 19e-6}}})
	st := store.NewMemory(store.Options{MaxEntries: 400})
	model := &fakeSummaryModel{out: "<summary>never</summary>"}
	kaCandidate(t, "sess-1", st, model)

	pol := kaPolicy()
	pol.MaxUSDPerPing = 0
	at := clock.now()
	rec(k, pol, sess1, at.Add(-time.Second), kaToolBody, http.StatusOK, Usage{CacheRead: 20000}, true)
	rec(k, pol, sess1, at, kaToolBody, http.StatusOK, Usage{CacheRead: 20000}, true)
	if k.Stats().Live != 1 {
		t.Fatalf("the session was not tracked, so the ping rule refused it (live=%d)", k.Stats().Live)
	}
	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
	if model.calls.Load() != 0 || k.summarySubstituteOverBudget.Load() != 1 {
		t.Errorf("model calls %d, over-budget %d, want 0 and 1", model.calls.Load(), k.summarySubstituteOverBudget.Load())
	}
	if fs.n() != 1 {
		t.Fatalf("sent %d plain pings, want 1: a refused substitute must fall back to the ping", fs.n())
	}
}

// The questions of one slot protect ONE miss: n of them are weighed together against it.
func TestSweepBatchesAreWeighedAgainstOneMiss(t *testing.T) {
	k, _, _ := testKeeper(t)
	fakeOf(k).setPrices(proxyPricer{fixedPrice{modelinfo.Price{CacheRead: 3.8e-7, Output: 19e-6}}})
	one := k.projectedSweepBatchesUSD("claude-sonnet-5", "", 20000, 1)
	inf := math.Inf(1)
	for _, tc := range []struct {
		miss float64
		want int
	}{{0, 4}, {0.5 * one, 0}, {one, 0}, {1.5 * one, 1}, {2.5 * one, 2}, {100, 4}} {
		if got := k.affordableSweepBatches("claude-sonnet-5", "", 20000, 4, inf, tc.miss); got != tc.want {
			t.Errorf("miss %.4f: %d batches, want %d", tc.miss, got, tc.want)
		}
	}
	// A huge FINITE cap is unlimited and must not overflow the int conversion on any platform.
	if got := k.affordableSweepBatches("claude-sonnet-5", "", 20000, 4, 1e300, 0); got != 4 {
		t.Errorf("a finite cap of 1e300: %d batches, want 4", got)
	}
	// The explicit cap still applies on top of the value.
	if got := k.affordableSweepBatches("claude-sonnet-5", "", 20000, 4, 1.5*one, 100); got != 1 {
		t.Errorf("cap 1.5 questions: %d batches, want 1", got)
	}
	// Unpriced: nothing to compare.
	fakeOf(k).setPrices(proxyPricer{noPrice{}})
	if got := k.affordableSweepBatches("unpriced-model", "", 20000, 4, 1e300, 0.0001); got != 4 {
		t.Errorf("unpriced: %d batches, want 4", got)
	}
}

// A sweep question that costs more than the miss is not sent, and the plain ping goes.
func TestSweepQuestionCostingMoreThanTheMissIsRefusedAndThePingStillGoes(t *testing.T) {
	k, fs, clock := testKeeper(t)
	fakeOf(k).setPrices(proxyPricer{fixedPrice{modelinfo.Price{Input: 1.38e-6, CacheRead: 3.8e-7, CacheWrite: 1.38e-6, Output: 19e-6}}})
	st := store.NewMemory(store.Options{MaxEntries: 400})
	asker := &sweepAsker{cacheRead: 20000}
	registerSweep(t, st, asker, "claude-sonnet-5", sweepConversation(3))

	pol := kaPolicy()
	pol.MaxUSDPerPing = 0
	at := clock.now()
	for _, d := range []time.Duration{-time.Second, 0} {
		rec(k, pol, sess1, at.Add(d), kaToolBody, http.StatusOK, Usage{CacheRead: 20000}, true)
	}
	if k.Stats().Live != 1 {
		t.Fatalf("the session was not tracked (live=%d)", k.Stats().Live)
	}
	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
	if asker.calls.Load() != 0 || k.sweepOverBudget.Load() != 1 || fs.n() != 1 {
		t.Errorf("asks %d, over-budget %d, plain pings %d, want 0, 1, 1", asker.calls.Load(), k.sweepOverBudget.Load(), fs.n())
	}
}
