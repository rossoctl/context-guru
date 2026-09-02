package metrics

import (
	"encoding/json"
	"testing"

	"github.com/rossoctl/context-guru/components"
)

// REGRESSION (#176). The extraction counters were process-global, so the `extract` block in
// /stats was the SUM of extract_llm and extract_llm_sweep under a name that reads as one of
// them. MEASURED (iteration 023, arm B): 101 `calls` at 59,009 ms mean latency and a net value
// of -$1.162 were read as extract_llm's, while extract_llm's own debug record reported zero
// surviving candidates on all 374 requests and the sweep had made 96 asks. A per-component
// latency and cost claim from a pipeline containing both was unsafe, and an experiment's
// conclusion was drawn from one.
//
// The reproduction below is that shape in miniature: the sweep makes the expensive calls, the
// tail pass makes none and only replays. If the two are pooled, extract_llm shows the sweep's
// calls and the sweep's latency.
func TestExtractStatsAreScopedPerComponent(t *testing.T) {
	resetExtract()
	// extract_llm: no calls at all. Only frozen replays, which cost nothing and are worth
	// something, so it records value without ever recording a call.
	RecordExtractionValue("extract_llm", 0.004)
	RecordExtractionCacheLookup("extract_llm", true)
	// extract_llm_sweep: the expensive component. Two very slow asks.
	RecordExtractionCall("extract_llm_sweep", 58_000)
	RecordExtractionCall("extract_llm_sweep", 60_018)
	RecordExtractionSpend("extract_llm_sweep", 1.166)
	RecordExtractionSaving("extract_llm_sweep", 900)

	s := ExtractSnapshot(0, 0.30/1e6, 0, 0)
	if s.ByComponent == nil {
		t.Fatal("no per-component breakdown: the pooled figure is the only one available again")
	}
	tail, ok := s.ByComponent["extract_llm"]
	if !ok {
		t.Fatal("extract_llm missing from the breakdown")
	}
	sweep, ok := s.ByComponent["extract_llm_sweep"]
	if !ok {
		t.Fatal("extract_llm_sweep missing from the breakdown")
	}
	// THE DEFECT: extract_llm made no call, so nothing may credit it with one, and its mean
	// latency is not 59 seconds — it has no latency at all.
	if tail.Calls != 0 {
		t.Errorf("extract_llm credited with %d calls; it made none (this is #176)", tail.Calls)
	}
	if tail.AvgLatencyMs != 0 {
		t.Errorf("extract_llm avg_latency_ms = %v; it made no calls to be slow",
			tail.AvgLatencyMs)
	}
	if sweep.Calls != 2 {
		t.Errorf("extract_llm_sweep calls = %d, want 2", sweep.Calls)
	}
	if sweep.AvgLatencyMs != 59_009 {
		t.Errorf("extract_llm_sweep avg_latency_ms = %v, want 59009", sweep.AvgLatencyMs)
	}
	// And the money lands on the component that spent it. extract_llm is net POSITIVE (free
	// replays), the sweep net NEGATIVE — the pooled block reported one negative figure and
	// attributed it to the wrong one.
	if net, known := tail.Net(); !known || net <= 0 {
		t.Errorf("extract_llm net = %v (known=%v); it made no call, so its free replays worth "+
			"$0.004 are provably profitable and the figure must be known", net, known)
	}
	if net, known := sweep.Net(); !known || net >= 0 {
		t.Errorf("extract_llm_sweep net = %v (known=%v); it spent $1.166 to save 900 tokens",
			net, known)
	}
	// And the rows must say WHERE their cost came from, so "$0" and "no evidence" are not the
	// same reading. extract_llm made no calls, so its 0 is provable; the sweep priced its own.
	if tail.CostSource != costSourceNone {
		t.Errorf("extract_llm cost_source = %q, want %q", tail.CostSource, costSourceNone)
	}
	if sweep.CostSource != costSourceComponent {
		t.Errorf("extract_llm_sweep cost_source = %q, want %q",
			sweep.CostSource, costSourceComponent)
	}
	// The enclosing block stays the SUM — deploy/harbor/*.py parses it — so the fix must be
	// additive, not a re-scoping of the existing keys.
	if s.Calls != 2 {
		t.Errorf("pooled calls = %d, want 2 (the block stays the sum for the harness)", s.Calls)
	}
}

// The latency BRAKE reads these counters to decide whether speculative calls are still worth
// their wall clock (offload.tooSlowToExplore). Pooled, the sweep's ~59-second frontier-model
// asks braked extract_llm's cheap-model exploration on evidence from a different component and
// a different model — a decision path, not just a display.
func TestLatencyAccessorsAreScopedPerComponent(t *testing.T) {
	resetExtract()
	RecordExtractionCall("extract_llm_sweep", 59_000)
	RecordExtractionCall("extract_llm_sweep", 59_000)
	RecordExtractionCall("extract_llm", 900)

	if p50, n := ExtractionP50LatencyMs("extract_llm"); p50 != 900 || n != 1 {
		t.Errorf("extract_llm p50 = (%v,%d), want (900,1) — it must not see the sweep's asks",
			p50, n)
	}
	if avg, n := ExtractionAvgLatencyMs("extract_llm"); avg != 900 || n != 1 {
		t.Errorf("extract_llm mean = (%v,%d), want (900,1)", avg, n)
	}
	if p50, n := ExtractionP50LatencyMs("extract_llm_sweep"); p50 != 59_000 || n != 2 {
		t.Errorf("extract_llm_sweep p50 = (%v,%d), want (59000,2)", p50, n)
	}
}

// Extraction spend must come from the component that priced it, not from cheapmodel's
// process-global token totals through one rate card. Those totals include `summarize` and
// `agentdiet`, and price the sweep's frontier-model asks at the cheap model's rates.
func TestRecordedSpendBeatsTheHostsGlobalFigure(t *testing.T) {
	resetExtract()
	RecordExtractionSpend("extract_llm", 0.02)
	RecordExtractionSpend("extract_llm_sweep", 0.30)
	// The host offers $9.99 — every cheap-model call in the process, extraction or not.
	s := ExtractSnapshot(9.99, 0.30/1e6, 0, 0)
	if s.ExtractionCostUSD != 0.32 {
		t.Errorf("extraction_cost_usd = %v, want 0.32 (the components' own priced spend)",
			s.ExtractionCostUSD)
	}
	if s.ByComponent["extract_llm"].ExtractionCostUSD != 0.02 {
		t.Errorf("extract_llm cost = %v, want 0.02",
			s.ByComponent["extract_llm"].ExtractionCostUSD)
	}
	if s.CostSource != costSourceComponent {
		t.Errorf("cost_source = %q, want %q", s.CostSource, costSourceComponent)
	}
}

// REVIEW FINDING (#178). The host's figure is one process-global number covering every cheap-model
// call in the process — `summarize` and `agentdiet` included — so it cannot be split across rows.
// Applying it to the AGGREGATE ONLY produced two figures for one quantity that disagreed by the
// whole spend: the block said $9.99 while its only by_component row said $0, and an operator
// reading the row saw a comfortably positive net value on no cost evidence at all.
//
// The contract now: a row that priced nothing says so (`cost_source: unpriced`) and leaves
// net_value_usd NULL rather than publishing +grossValue. 0 dollars and 0 evidence are the same
// number and the opposite claim.
func TestAnUnpricedRowSaysSoInsteadOfClaimingZero(t *testing.T) {
	resetExtract()
	// A library embedding: calls are made, ModelCall.CostUSD is never filled, and the removal is
	// worth something — the exact shape that read as "profitable" before.
	RecordExtractionCall("extract_llm", 100)
	RecordExtractionValue("extract_llm", 0.05)
	s := ExtractSnapshot(9.99, 0.30/1e6, 0, 0)

	if s.ExtractionCostUSD != 9.99 || s.CostSource != costSourceHost {
		t.Errorf("aggregate = $%v from %q, want 9.99 from %q: with nothing priced, the host's "+
			"figure is all the information there is", s.ExtractionCostUSD, s.CostSource,
			costSourceHost)
	}
	row := s.ByComponent["extract_llm"]
	if row == nil {
		t.Fatal("extract_llm missing from the breakdown")
	}
	if row.CostSource != costSourceUnpriced {
		t.Errorf("row cost_source = %q, want %q — $0 must not be indistinguishable from "+
			"'no spend recorded'", row.CostSource, costSourceUnpriced)
	}
	if net, known := row.Net(); known {
		t.Errorf("row net_value_usd = %v, want null: the component made a call and priced none "+
			"of it, so nothing is known about whether it paid", net)
	}
	// The aggregate's net is always known — it always has a determined spend behind it.
	if _, known := s.Net(); !known {
		t.Error("the aggregate net must never be null")
	}
}

// REVIEW FINDING (#178), the other half. `anySpend` was one boolean across every component, so
// PARTIAL recording silently under-reported: if extract_llm priced its calls (the cheap card is
// never zero) while extract_llm_sweep priced none, the total became extract_llm's spend alone and
// the sweep's real dollars vanished — no fallback, no warning, a SMALLER number than the code
// published before this PR.
func TestPartialPricingDoesNotSilentlyUnderReportTheTotal(t *testing.T) {
	resetExtract()
	RecordExtractionCall("extract_llm", 100)
	RecordExtractionSpend("extract_llm", 0.02) // priced
	RecordExtractionCall("extract_llm_sweep", 59_000)
	// ...and the sweep prices nothing. The host saw $4.00 of cheap-model spend in the process.
	s := ExtractSnapshot(4.00, 0.30/1e6, 0, 0)

	if s.ExtractionCostUSD <= 0.02 {
		t.Errorf("aggregate = $%v: the recorded sum alone DROPS the unpriced component's "+
			"dollars, reporting less than the host already knew", s.ExtractionCostUSD)
	}
	if s.ExtractionCostUSD != 4.00 || s.CostSource != costSourceHost {
		t.Errorf("aggregate = $%v from %q, want 4.00 from %q (the larger of the floor and the "+
			"host's superset figure, named)", s.ExtractionCostUSD, s.CostSource, costSourceHost)
	}
	// And when the host's figure is NOT larger, the total is a floor and must say so rather than
	// pass itself off as the bill.
	resetExtract()
	RecordExtractionCall("extract_llm", 100)
	RecordExtractionSpend("extract_llm", 5.00)
	RecordExtractionCall("extract_llm_sweep", 59_000)
	s = ExtractSnapshot(1.00, 0.30/1e6, 0, 0)
	if s.ExtractionCostUSD != 5.00 || s.CostSource != costSourcePartial {
		t.Errorf("aggregate = $%v from %q, want 5.00 from %q", s.ExtractionCostUSD,
			s.CostSource, costSourcePartial)
	}
}

// REVIEW FOLLOW-UP (#178). `cost_source: partial` said the total was a FLOOR without saying what it
// was short OF, so a reader had to go and scan every by_component row for `cost_source: unpriced` —
// re-deriving an answer the snapshot had already computed, since it is the same test that set the
// aggregate's label. An operator who has to re-derive it will read the label and stop, and then the
// floor gets quoted as the bill.
//
// Asserted on the MARSHALLED PAYLOAD, not the Go field. That is the lesson of this PR's M13: a
// `cost_source` mutant tagged `json:"-"` survived the first vacuity pass precisely because the
// assertions read the struct.
func TestTheAggregateNamesWhichComponentIsUnpriced(t *testing.T) {
	resetExtract()
	RecordExtractionCall("extract_llm", 100)
	RecordExtractionSpend("extract_llm", 5.00) // priced
	RecordExtractionCall("extract_llm_sweep", 59_000)
	// ...and the sweep prices nothing, so the total is a floor and the sweep is why.
	b, err := json.Marshal(ExtractSnapshot(1.00, 0.30/1e6, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["cost_source"] != costSourcePartial {
		t.Fatalf("precondition: rendered cost_source = %v, want %q — without a floor there is "+
			"nothing for the list to name: %s", m["cost_source"], costSourcePartial, b)
	}
	raw, present := m["unpriced_components"]
	if !present {
		t.Fatalf("the rendered block does not name the unpriced component, so `partial` still "+
			"leaves the reader to scan every row: %s", b)
	}
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		t.Fatalf("unpriced_components = %v, want a non-empty list", raw)
	}
	if len(list) != 1 || list[0] != "extract_llm_sweep" {
		t.Errorf("unpriced_components = %v, want [extract_llm_sweep]: extract_llm priced its "+
			"call and must not be named, the sweep did not and must be", list)
	}

	// And it is OMITTED when every call priced itself — an empty list on a complete total would
	// read as a warning where there is nothing to warn about.
	resetExtract()
	RecordExtractionCall("extract_llm", 100)
	RecordExtractionSpend("extract_llm", 5.00)
	b2, _ := json.Marshal(ExtractSnapshot(1.00, 0.30/1e6, 0, 0))
	var m2 map[string]any
	_ = json.Unmarshal(b2, &m2)
	if m2["cost_source"] != costSourceComponent {
		t.Fatalf("precondition: rendered cost_source = %v, want %q", m2["cost_source"],
			costSourceComponent)
	}
	if _, present := m2["unpriced_components"]; present {
		t.Errorf("unpriced_components must be omitted when nothing is unpriced: %s", b2)
	}
}

// The breakdown has to survive JSON, because /stats is the only place anyone reads it and a
// field can be computed correctly and dropped by the encoder.
func TestByComponentSurvivesJSON(t *testing.T) {
	resetExtract()
	RecordExtractionCall("extract_llm_sweep", 59_000)
	b, err := json.Marshal(ExtractSnapshot(0.1, 0.30/1e6, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	by, ok := m["by_component"].(map[string]any)
	if !ok {
		t.Fatalf("by_component missing from the encoded block: %s", b)
	}
	row, ok := by["extract_llm_sweep"].(map[string]any)
	if !ok {
		t.Fatalf("extract_llm_sweep row missing: %s", b)
	}
	if row["avg_latency_ms"] != 59_000.0 {
		t.Errorf("avg_latency_ms = %v in the encoded row, want 59000", row["avg_latency_ms"])
	}
	// cost_source must survive the ENCODER, on the row and on the block. It is the field that
	// separates "$0 spent" from "no spend recorded", so a value computed correctly and dropped by
	// the json tag would put the reader back where #176 found them. Verified non-vacuous: tagging
	// the field `json:"-"` makes this fail.
	for _, k := range []string{"cost_source", "net_value_usd", "extraction_cost_usd"} {
		if _, ok := row[k]; !ok {
			t.Errorf("the encoded by_component row is missing %q: %s", k, b)
		}
		if _, ok := m[k]; !ok {
			t.Errorf("the encoded extract block is missing %q: %s", k, b)
		}
	}
	// This row made a call and priced none of it, so the rendered net must be JSON null and the
	// source must name the case. A 0 here reads as break-even on no evidence.
	if row["cost_source"] != "unpriced" {
		t.Errorf("rendered cost_source = %v, want \"unpriced\"", row["cost_source"])
	}
	if v, present := row["net_value_usd"]; !present || v != nil {
		t.Errorf("rendered net_value_usd = %v (present=%v), want null", v, present)
	}
	// It must not recurse: a nested row carrying its own breakdown would be an infinite
	// document, and the omitempty is what prevents it.
	if _, nested := row["by_component"]; nested {
		t.Error("the breakdown nests inside itself")
	}
	// And it must be OMITTED when nothing recorded, so a parser that does not know the key
	// sees no change at all.
	resetExtract()
	b2, _ := json.Marshal(ExtractSnapshot(0, 0.30/1e6, 0, 0))
	var m2 map[string]any
	_ = json.Unmarshal(b2, &m2)
	if _, present := m2["by_component"]; present {
		t.Errorf("by_component must be omitted when empty: %s", b2)
	}
}

// REGRESSION (#176, second half). `acted` is `Saved() > 0`, and a frozen decision replayed on a
// later turn saves tokens for free — so 2,291 replays and a handful of paid extractions landed
// in one counter. The measured snapshot showed `acted: 239` on a component whose own record
// proved it made no call, and that number was read as 239 paid extractions.
func TestActedSeparatesFreeReplaysFromPaidWork(t *testing.T) {
	a := NewAggregator()
	// Three requests that only REPLAYED a decision frozen on an earlier turn: tokens saved,
	// nothing spent, no model call.
	for i := 0; i < 3; i++ {
		r := components.Report{Component: "extract_llm", Kind: "offload",
			TokensBefore: 10_000, TokensAfter: 6_000}
		r.Replay("reapplied_same_session")
		a.Component(r)
	}
	// One request that actually paid for a call.
	a.Component(components.Report{Component: "extract_llm", Kind: "offload",
		TokensBefore: 10_000, TokensAfter: 6_000,
		Calls: []components.ModelCall{{Component: "extract_llm", Model: "haiku", CostUSD: 0.01}}})

	snap := a.Snapshot()
	cs, ok := snap.Components["extract_llm"]
	if !ok {
		t.Fatal("extract_llm missing from the rollup")
	}
	if cs.Acted != 4 {
		t.Fatalf("acted = %d, want 4 (the pre-existing key keeps its meaning)", cs.Acted)
	}
	if cs.ActedReplay != 3 {
		t.Errorf("acted_replay = %d, want 3 — free replays counted as paid work (this is #176)",
			cs.ActedReplay)
	}
	if cs.ActedFresh != 1 {
		t.Errorf("acted_fresh = %d, want 1", cs.ActedFresh)
	}
	if cs.ActedFresh+cs.ActedReplay != cs.Acted {
		t.Errorf("the split must partition acted: %d + %d != %d",
			cs.ActedFresh, cs.ActedReplay, cs.Acted)
	}
	// A deterministic component does fresh work every run and pays no model for it; it must
	// not read as replaying.
	a.Component(components.Report{Component: "dedup", Kind: "offload",
		TokensBefore: 100, TokensAfter: 40})
	if d := a.Snapshot().Components["dedup"]; d.ActedFresh != 1 || d.ActedReplay != 0 {
		t.Errorf("dedup: fresh=%d replay=%d, want 1/0", d.ActedFresh, d.ActedReplay)
	}
}
