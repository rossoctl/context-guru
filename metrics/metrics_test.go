package metrics

import (
	"testing"

	"github.com/rossoctl/context-guru/components"
)

func TestAggregatorHonestMetrics(t *testing.T) {
	a := NewAggregator()
	a.Component(components.Report{Component: "dedup", TokensBefore: 100, TokensAfter: 60}) // saved 40, acted
	a.Component(components.Report{Component: "format", TokensBefore: 50, TokensAfter: 50, Skipped: true})
	a.Run(components.RunReport{TokensBefore: 100, TokensAfter: 60})
	a.RecordExpand(25) // 25 tokens had to be re-served

	s := a.Snapshot()
	if s.SavedTokens != 40 {
		t.Fatalf("saved=%d want 40", s.SavedTokens)
	}
	if s.WastedTokens != 25 || s.Bounces != 1 {
		t.Fatalf("wasted=%d bounces=%d want 25/1", s.WastedTokens, s.Bounces)
	}
	if s.AdjustedSaved != 15 {
		t.Fatalf("adjusted=%d want 15 (40-25)", s.AdjustedSaved)
	}
	// format never saved a token -> flagged; dedup did -> not flagged.
	if len(s.TopPassthrough) != 1 || s.TopPassthrough[0] != "format" {
		t.Fatalf("top_passthrough=%v want [format]", s.TopPassthrough)
	}
	if s.Components["dedup"].Acted != 1 {
		t.Fatalf("dedup should have acted once, got %+v", s.Components["dedup"])
	}
}

// TestUniqueSavingsDedupsByKey: the same compaction (same CacheKey) re-sent every
// turn inflates cumulative saved_tokens, but saved_tokens_unique counts it once, and
// overcount_ratio surfaces the inflation.
func TestUniqueSavingsDedupsByKey(t *testing.T) {
	a := NewAggregator()
	// Turn 1: failed_run collapses one run (saved 300), stashed under key "k1".
	// Turns 2-4: the agent re-sends it verbatim, the proxy re-collapses to the SAME
	// bytes/key each turn — cumulative grows, unique does not.
	for turn := 0; turn < 4; turn++ {
		a.Component(components.Report{Component: "failed_run", TokensBefore: 400, TokensAfter: 100, CacheKeys: []string{"k1"}})
	}
	s := a.Snapshot()
	cs := s.Components["failed_run"]
	if cs.Saved != 1200 {
		t.Fatalf("cumulative saved=%d want 1200 (300*4)", cs.Saved)
	}
	if cs.SavedUnique != 300 {
		t.Fatalf("unique saved=%d want 300 (counted once)", cs.SavedUnique)
	}
	if cs.OvercountRatio != 4 {
		t.Fatalf("overcount_ratio=%v want 4", cs.OvercountRatio)
	}
}

// TestLatencyAverages: added + upstream latencies average correctly, split by bypass.
func TestLatencyAverages(t *testing.T) {
	a := NewAggregator()
	a.RecordAddedLatency(10)
	a.RecordAddedLatency(20)
	a.RecordUpstreamLatency(100, false)
	a.RecordUpstreamLatency(300, false)
	a.RecordUpstreamLatency(80, true)
	s := a.Snapshot()
	if s.AddedLatencyMsAvg != 15 {
		t.Fatalf("added avg=%v want 15", s.AddedLatencyMsAvg)
	}
	if s.UpstreamMsAvg != 200 || s.UpstreamMsAvgBypassed != 80 {
		t.Fatalf("upstream avg=%v byp=%v want 200/80", s.UpstreamMsAvg, s.UpstreamMsAvgBypassed)
	}
}

// TestMutatedZeroSavingsNotPassthrough locks the fix for cacheinject-style
// components: they change the request (add cache_control) but save no content
// tokens, so they must NOT be flagged as dead weight in top_passthrough.
func TestMutatedZeroSavingsNotPassthrough(t *testing.T) {
	a := NewAggregator()
	// cacheinject: ran, not skipped, not reverted, but 0 content tokens saved.
	a.Component(components.Report{Component: "cacheinject", Kind: "reformat", TokensBefore: 100, TokensAfter: 100})
	// skeleton: always skipped this workload -> genuine dead weight.
	a.Component(components.Report{Component: "skeleton", Kind: "offload", TokensBefore: 100, TokensAfter: 100, Skipped: true})

	s := a.Snapshot()
	if len(s.TopPassthrough) != 1 || s.TopPassthrough[0] != "skeleton" {
		t.Fatalf("top_passthrough=%v want [skeleton] (cacheinject mutated, not dead weight)", s.TopPassthrough)
	}
	if s.Components["cacheinject"].Mutated != 1 {
		t.Fatalf("cacheinject should record a mutation, got %+v", s.Components["cacheinject"])
	}
}

// An off-path (deferred) async run forwarded nothing, so its savings must not enter
// the enforced rollups. They are counted when a later turn REPLAYS the frozen
// decision on the request path; counting both would double-count every deferred
// compaction and credit savings to a request that never carried them.
func TestDeferredRunsAreNotCountedAsEnforced(t *testing.T) {
	a := NewAggregator()
	a.Component(components.Report{
		Component: "extract_llm", Kind: "offload", Mode: components.ModeAsync,
		Deferred: true, TokensBefore: 1000, TokensAfter: 400, CacheKeys: []string{"k"},
	})
	a.Run(components.RunReport{
		Session: "s", Mode: components.ModeAsync, Deferred: true,
		TokensBefore: 1000, TokensAfter: 400,
	})

	s := a.Snapshot()
	if s.Requests != 0 || s.SavedTokens != 0 || s.TokensBefore != 0 {
		t.Fatalf("a deferred run was counted as enforced: %+v", s)
	}
	if s.AsyncEnforced != 0 || s.SyncEnforced != 0 {
		t.Fatalf("deferred run counted under an enforced mode: %+v", s)
	}
	if len(s.Components) != 0 {
		t.Fatalf("deferred run reached the enforced per-component map: %v", s.Components)
	}
	// Nor may it be mistaken for an observe hypothetical.
	if s.ObserveRequests != 0 || s.PotentialSavedTokens != 0 {
		t.Fatalf("deferred run leaked into the hypothetical namespace: %+v", s)
	}

	// The on-path replay IS what gets counted.
	a.Run(components.RunReport{Session: "s", Mode: components.ModeAsync, TokensBefore: 1000, TokensAfter: 400})
	a.RecordRealized(600)
	s = a.Snapshot()
	if s.AsyncEnforced != 1 || s.SavedTokens != 600 {
		t.Fatalf("the on-path replay was not counted: %+v", s)
	}
	if s.RealizedSavedTokens != 600 {
		t.Fatalf("realized savings not attributed: %d", s.RealizedSavedTokens)
	}
}
