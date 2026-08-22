package metrics

import (
	"encoding/json"
	"strconv"
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

// TestSSEBufferingStats: streamed vs buffered SSE responses average separately and
// the buffered share is reported, so a buffering regression is visible in /stats.
func TestSSEBufferingStats(t *testing.T) {
	a := NewAggregator()
	if s := a.Snapshot(); s.SSEBufferedPct != 0 || s.SSETTFBMsAvg != 0 {
		t.Fatalf("no SSE traffic should report zeros: %+v", s)
	}
	a.RecordSSE(20, false)
	a.RecordSSE(40, false)
	a.RecordSSE(900, true)
	s := a.Snapshot()
	if s.SSEStreamed != 2 || s.SSEBuffered != 1 {
		t.Fatalf("counts=%d/%d want 2 streamed / 1 buffered", s.SSEStreamed, s.SSEBuffered)
	}
	if s.SSETTFBMsAvg != 30 || s.SSETTFBMsAvgBuf != 900 {
		t.Fatalf("ttfb=%v buffered=%v want 30/900", s.SSETTFBMsAvg, s.SSETTFBMsAvgBuf)
	}
	if got := s.SSEBufferedPct; got < 33.3 || got > 33.4 {
		t.Fatalf("buffered pct=%v want ~33.33", got)
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

// The cmdfilter ledger: per-family and per-filter attribution, unique-vs-cumulative
// dedup, and the bounded selector-miss ledger.
func TestFilterStatsLedger(t *testing.T) {
	a := NewAggregator()
	a.FilterAct("iac", "terraform-plan", "k1", 300)
	a.FilterAct("iac", "terraform-plan", "k1", 300) // same compaction re-sent next turn
	a.FilterAct("iac", "terraform-init", "k2", 40)
	a.FilterAct("builds", "make", "k3", 100)
	a.FilterMiss("Totally unknown shape")
	a.FilterMiss("Totally unknown shape")
	a.FilterMiss("Another shape")

	s := a.Snapshot()
	if got := s.CmdfilterFamilies["iac"]; got.Acts != 3 || got.Saved != 640 || got.SavedUnique != 340 {
		t.Fatalf("iac family ledger wrong: %+v", got)
	}
	if got := s.CmdfilterFilters["make"]; got.Acts != 1 || got.SavedUnique != 100 {
		t.Fatalf("per-filter ledger wrong: %+v", got)
	}
	if len(s.CmdfilterMisses) != 2 || s.CmdfilterMisses[0].Selector != "Totally unknown shape" || s.CmdfilterMisses[0].Count != 2 {
		t.Fatalf("misses should be ranked by frequency: %+v", s.CmdfilterMisses)
	}
	// the ledger is bounded: unknown selectors past the cap are dropped, not appended
	for i := 0; i < maxMissKeys*2; i++ {
		a.FilterMiss("shape-" + strconv.Itoa(i))
	}
	if n := len(a.filterMiss); n > maxMissKeys {
		t.Fatalf("miss ledger grew past its cap: %d", n)
	}
}

// /stats must stay backward compatible: the fields deploy/harbor/*.py parses are
// still present, and the new cmdfilter fields are additive-and-omitted-when-empty.
func TestSnapshotStaysBackwardCompatible(t *testing.T) {
	b, err := json.Marshal(NewAggregator().Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"requests", "tokens_before", "tokens_after", "saved_tokens",
		"savings_pct", "wasted_tokens", "bounces", "adjusted_saved", "components",
		"llm_calls", "cg_added_ms_avg", "upstream_ms_avg"} {
		if _, ok := m[k]; !ok {
			t.Errorf("/stats lost the %q field the harness parses", k)
		}
	}
	for _, k := range []string{"cmdfilter_families", "cmdfilter_filters", "cmdfilter_selector_misses"} {
		if _, ok := m[k]; ok {
			t.Errorf("%q should be omitted when empty", k)
		}
	}
}

// A cache component MUTATES without SAVING content tokens: cachesplit moves tokens out of a
// hashed prefix rather than removing them, so its value is which billing TIER they land in.
// "acted: 0" beside "mutated: 755" has been read as a broken component and filed as a bug
// twice — most recently against a mechanism that had run 3,000 times and was working exactly
// as designed. The rollup therefore states the reading instead of leaving it to be inferred.
func TestComponentVerdictDistinguishesMovedFromSkipped(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reports []components.Report
		want    string
	}{
		{"never ran", nil, "idle"},
		{"ran and always declined", []components.Report{{Component: "c", Skipped: true}}, "skipped"},
		{"changed the request but removed no content tokens", []components.Report{
			{Component: "c"}}, "moved"},
		{"removed content tokens", []components.Report{
			{Component: "c", TokensBefore: 100, TokensAfter: 40}}, "acted"},
	} {
		a := NewAggregator()
		for _, r := range tc.reports {
			a.Component(r)
		}
		snap := a.Snapshot()
		got := "idle"
		if cs, ok := snap.Components["c"]; ok {
			got = cs.Verdict
		}
		if got != tc.want {
			t.Errorf("%s: verdict = %q, want %q", tc.name, got, tc.want)
		}
	}
}
