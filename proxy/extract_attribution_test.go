package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/metrics"
)

// The attribution fix has to reach the SURFACE, not just the counter. /stats is where the
// misattribution was read (#176) — 101 calls and a 59-second mean charged to a component whose
// own log said it made none — and the statsHandler builds the extract block itself, so a
// correctly scoped counter can still arrive pooled or be dropped entirely by the handler.
//
// Asserted on the rendered body for that reason.
func TestStatsRendersPerComponentExtractionAttribution(t *testing.T) {
	// These counters are process-global by construction (one proxy, one process), so the
	// assertions below are stated against a BASELINE read rather than against zero: another
	// test in this package that exercised extract_llm for real would otherwise make this one
	// fail for a reason that has nothing to do with #176.
	base := metrics.ExtractSnapshot(0, 0, 0, 0)
	baseTailCalls, baseTailCost := int64(0), 0.0
	if r := base.ByComponent["extract_llm"]; r != nil {
		baseTailCalls, baseTailCost = r.Calls, r.ExtractionCostUSD
	}

	// The measured shape: the sweep pays for the calls, extract_llm only replays.
	metrics.RecordExtractionCall("extract_llm_sweep", 59_009)
	metrics.RecordExtractionSpend("extract_llm_sweep", 1.20)
	// Value but no call and no lookup: that IS extract_llm's measured behaviour here (frozen
	// replays are worth something and cost nothing), and it deliberately touches no counter
	// another test in this package asserts an absolute value on.
	metrics.RecordExtractionValue("extract_llm", 0.0038)

	agg := metrics.NewAggregator()
	// And the acted split: three free replays against one paid call, which is the pair that
	// `acted: 239` beside `reapplied_same_session: 2,291` could not distinguish.
	for i := 0; i < 3; i++ {
		r := components.Report{Component: "extract_llm", Kind: "offload",
			TokensBefore: 10_000, TokensAfter: 6_000}
		r.Replay("reapplied_same_session")
		agg.Component(r)
	}
	agg.Component(components.Report{Component: "extract_llm", Kind: "offload",
		TokensBefore: 10_000, TokensAfter: 6_000,
		Calls: []components.ModelCall{{Component: "extract_llm", Model: "haiku", CostUSD: 0.01}}})

	h := New(nil, nil, agg, Options{})
	w := httptest.NewRecorder()
	h.stats(w, httptest.NewRequest("GET", "/stats", nil))

	var got struct {
		Extract struct {
			Calls       int64  `json:"calls"`
			CostSource  string `json:"cost_source"`
			ByComponent map[string]struct {
				Calls             int64    `json:"calls"`
				AvgLatencyMs      float64  `json:"avg_latency_ms"`
				ExtractionCostUSD float64  `json:"extraction_cost_usd"`
				NetValueUSD       *float64 `json:"net_value_usd"`
				CostSource        string   `json:"cost_source"`
			} `json:"by_component"`
		} `json:"extract"`
		Components map[string]struct {
			Acted       int64 `json:"acted"`
			ActedFresh  int64 `json:"acted_fresh"`
			ActedReplay int64 `json:"acted_replay"`
		} `json:"components"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("/stats is not the expected shape: %v\n%s", err, w.Body.String())
	}

	if len(got.Extract.ByComponent) == 0 {
		t.Fatalf("/stats serves no extract.by_component, so the pooled figure is again the "+
			"only one an operator can read: %s", w.Body.String())
	}
	sweep, ok := got.Extract.ByComponent["extract_llm_sweep"]
	if !ok {
		t.Fatalf("extract_llm_sweep absent from extract.by_component: %s", w.Body.String())
	}
	tail := got.Extract.ByComponent["extract_llm"]
	// THE DEFECT, at the surface: the calls and the latency belong to the sweep alone.
	if sweep.Calls == 0 || sweep.AvgLatencyMs == 0 {
		t.Errorf("the sweep's own calls/latency did not reach /stats: %+v", sweep)
	}
	if tail.Calls != baseTailCalls {
		t.Errorf("/stats credits extract_llm with %d calls (baseline %d); it made none here, "+
			"and the sweep's must not leak into its row (#176)", tail.Calls, baseTailCalls)
	}
	if sweep.ExtractionCostUSD == 0 {
		t.Error("the sweep's spend did not reach /stats, so its net value is unchecked")
	}
	if tail.ExtractionCostUSD != baseTailCost {
		t.Errorf("extract_llm charged $%v (baseline $%v) it did not spend",
			tail.ExtractionCostUSD, baseTailCost)
	}
	// cost_source has to reach the WIRE, on the block and on every row: it is what separates a
	// provable $0 from an unknown, and the statsHandler builds this block itself so the field can
	// be computed and dropped. The sweep priced its own call; extract_llm made none, so its 0 is
	// provable rather than unknown.
	if got.Extract.CostSource == "" {
		t.Errorf("/stats serves no extract.cost_source: %s", w.Body.String())
	}
	if sweep.CostSource != "component" {
		t.Errorf("sweep cost_source = %q on the wire, want \"component\"", sweep.CostSource)
	}
	if tail.CostSource != "none" {
		t.Errorf("extract_llm cost_source = %q on the wire, want \"none\" (no calls, so $0 is "+
			"the true figure and not a missing one)", tail.CostSource)
	}
	if sweep.NetValueUSD == nil {
		t.Error("the sweep priced its own call, so its net_value_usd must not be null on the wire")
	}

	cs, ok := got.Components["extract_llm"]
	if !ok {
		t.Fatalf("extract_llm absent from components: %s", w.Body.String())
	}
	if cs.Acted != 4 {
		t.Errorf("acted = %d, want 4 (the pre-existing key must keep its meaning)", cs.Acted)
	}
	if cs.ActedReplay != 3 || cs.ActedFresh != 1 {
		t.Errorf("/stats acted split = fresh %d / replay %d, want 1/3 — without it a component "+
			"amortizing frozen work reads identically to one making paid calls",
			cs.ActedFresh, cs.ActedReplay)
	}
}

// REVIEW FOLLOW-UP (#178), at the surface. metrics computes `unpriced_components`; the statsHandler
// builds the extract block itself, so the list can be correct and never reach the wire. Same reason
// the rest of this file asserts on the rendered body.
//
// A component name of its own rather than one of the two real ones: these counters are
// process-global for the whole package run, and TestStatsRendersPerComponentExtractionAttribution
// asserts ABSOLUTE values on `extract_llm` and `extract_llm_sweep` (zero calls, zero cost). Making
// either of them unpriced here would break that test through shared state, for a reason unrelated to
// what either test is about. What is under test is the aggregate's list, not the name in it.
func TestStatsNamesTheUnpricedComponent(t *testing.T) {
	const unpriced = "extract_llm_unpriced_fixture"
	metrics.RecordExtractionCall(unpriced, 1200) // calls, and no RecordExtractionSpend

	h := New(nil, nil, metrics.NewAggregator(), Options{})
	w := httptest.NewRecorder()
	h.stats(w, httptest.NewRequest("GET", "/stats", nil))

	var got struct {
		Extract struct {
			CostSource         string   `json:"cost_source"`
			UnpricedComponents []string `json:"unpriced_components"`
		} `json:"extract"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("/stats is not the expected shape: %v\n%s", err, w.Body.String())
	}
	// PRECONDITION: the total is incomplete, so there is something for the list to name. Without
	// this the assertion below could pass vacuously on a snapshot that had nothing to report.
	if got.Extract.CostSource != "partial" && got.Extract.CostSource != "host_total" {
		t.Fatalf("cost_source = %q, want partial or host_total: an unpriced call must make the "+
			"total incomplete, or this test proves nothing", got.Extract.CostSource)
	}
	if len(got.Extract.UnpricedComponents) == 0 {
		t.Fatalf("/stats serves an incomplete total with no unpriced_components, so an operator "+
			"cannot tell WHAT it is short of: %s", w.Body.String())
	}
	var found bool
	for _, n := range got.Extract.UnpricedComponents {
		if n == unpriced {
			found = true
		}
	}
	if !found {
		t.Errorf("unpriced_components = %v on the wire, missing %q — the component that made an "+
			"unpriced call is the one the reader needs named",
			got.Extract.UnpricedComponents, unpriced)
	}
}
