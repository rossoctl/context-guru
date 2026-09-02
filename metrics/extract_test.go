package metrics

import (
	"encoding/json"
	"testing"
)

// Net-after-cost is the honest headline (#28 part F): a component that saves 197k tokens
// worth $0.059 while spending $3.26 must report NEGATIVE, not a proud gross figure.
func TestNetValueGoesNegativeWhenUnderwater(t *testing.T) {
	resetExtract()
	// The measured Terminal-Bench shape: ~197,548 unique tokens saved at the cache-read
	// rate ($0.30/MTok) against $3.26 of extraction spend.
	RecordExtractionSaving("extract_llm", 197548)
	s := ExtractSnapshot(3.26, 0.30/1e6, 0, 0)
	net, known := s.Net()
	if !known {
		t.Fatalf("the aggregate net must always be known, got null: %+v", s)
	}
	if net >= 0 {
		t.Fatalf("net must be negative when spend exceeds value: net=%v gross=%v cost=%v",
			net, s.GrossValueUSD, s.ExtractionCostUSD)
	}
	// And the ratio must reproduce the issue's ~8x-underwater claim to the right order.
	if ratio := s.ExtractionCostUSD / s.GrossValueUSD; ratio < 40 {
		t.Logf("cost/value ratio = %.1fx (issue reported ~8x against a different value basis)", ratio)
	}
}

// All the part-F counters must be exposed, including the ones that justify the component:
// calls avoided by cache and calls suppressed by the gate.
func TestExtractSnapshotExposesAllCounters(t *testing.T) {
	resetExtract()
	RecordExtractionCall("extract_llm", 450)
	RecordExtractionCall("extract_llm", 550)
	RecordExtractionCacheLookup("extract_llm", true)
	RecordExtractionCacheLookup("extract_llm", true)
	RecordExtractionCacheLookup("extract_llm", false)
	RecordExtractionSuppressed("extract_llm", "suppressed: cache-aware, saving below call cost")
	RecordExtractionSaving("extract_llm", 1200)
	RecordExtractionReason("extract_llm", "high context pressure")

	// 1,200 own saved tokens at a rate chosen to give gross value exactly $0.50.
	s := ExtractSnapshot(0.024, 0.5/1200, 800, 0)
	if s.Calls != 2 {
		t.Errorf("Calls = %d, want 2", s.Calls)
	}
	if s.CallsAvoided != 2 {
		t.Errorf("CallsAvoided = %d, want 2", s.CallsAvoided)
	}
	if s.CallsSuppressed != 1 {
		t.Errorf("CallsSuppressed = %d, want 1", s.CallsSuppressed)
	}
	if s.GrossSavedTokens != 1200 {
		t.Errorf("GrossSavedTokens = %d, want 1200", s.GrossSavedTokens)
	}
	if s.AvgLatencyMs != 500 {
		t.Errorf("AvgLatencyMs = %v, want 500", s.AvgLatencyMs)
	}
	want := 2.0 / 3.0
	if d := s.CacheHitRate - want; d > 1e-9 || d < -1e-9 {
		t.Errorf("CacheHitRate = %v, want %v", s.CacheHitRate, want)
	}
	if net, known := s.Net(); !known || net != 0.476 {
		t.Errorf("NetValueUSD = %v (known=%v), want 0.476", net, known)
	}
	// The trigger reason must be recoverable — an operator's first question.
	if s.TopReason == "" || len(s.Reasons) != 2 {
		t.Errorf("reasons not exposed: top=%q reasons=%v", s.TopReason, s.Reasons)
	}
}

// A zero prompt-cache read while calls climb is the evidence that part A's breakpoint is
// inert on this model. It must be visible in /stats, not inferred.
func TestPromptCacheReadZeroIsReported(t *testing.T) {
	resetExtract()
	for i := 0; i < 5; i++ {
		RecordExtractionCall("extract_llm", 400)
	}
	s := ExtractSnapshot(0.06, 0.30/1e6, 0, 0)
	if s.Calls != 5 || s.PromptCacheReadTokens != 0 {
		t.Fatalf("expected 5 calls with 0 cache reads, got calls=%d read=%d",
			s.Calls, s.PromptCacheReadTokens)
	}
}

// /stats must stay backward compatible: deploy/harbor/*.py parses these exact keys, so a
// rename or removal breaks the benchmark harness. Fields are ADDED, never changed.
func TestSnapshotJSONKeysAreBackwardCompatible(t *testing.T) {
	a := NewAggregator()
	b, err := json.Marshal(a.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	// Every key the harness reads today.
	for _, k := range []string{
		"requests", "tokens_before", "tokens_after", "saved_tokens", "savings_pct",
		"wasted_tokens", "bounces", "adjusted_saved", "components", "top_passthrough",
		"llm_calls", "llm_input_tokens", "llm_output_tokens",
		"cg_added_ms_avg", "upstream_ms_avg", "upstream_ms_avg_bypassed",
	} {
		if _, ok := m[k]; !ok {
			t.Errorf("/stats lost backward-compatible key %q", k)
		}
	}
	// The new block must be omitted when absent rather than serialized as null, so a
	// parser that does not know about it sees no change at all.
	if _, present := m["extract"]; present {
		t.Error(`"extract" must be omitted when unset (omitempty)`)
	}

	// And when present, it must carry the net figure.
	snap := a.Snapshot()
	xs := ExtractSnapshot(1.0, 0.30/1e6, 0, 0)
	snap.Extract = &xs
	b2, _ := json.Marshal(snap)
	var m2 map[string]any
	_ = json.Unmarshal(b2, &m2)
	ext, ok := m2["extract"].(map[string]any)
	if !ok {
		t.Fatal(`"extract" block missing when set`)
	}
	for _, k := range []string{"calls", "calls_avoided", "calls_suppressed",
		"extraction_cost_usd", "gross_value_usd", "net_value_usd",
		"prompt_cache_read_tokens", "avg_latency_ms", "cache_hit_rate"} {
		if _, ok := ext[k]; !ok {
			t.Errorf("extract block missing %q", k)
		}
	}
}

// resetExtract clears the per-component counters so assertions are independent. Dropping the
// whole registry rather than zeroing each field: a counter added later would otherwise leak
// across tests silently, which is the failure mode a reset helper exists to prevent.
func resetExtract() {
	xRegMu.Lock()
	xReg = map[string]*xCounters{}
	xRegMu.Unlock()
}

// REGRESSION (H3, reviewer-verified): /stats must value extract_llm's OWN savings, never
// the pipeline total. The bug was `ExtractSnapshot(cost, snap.SavedTokens*rate, ...)` — every
// component's savings priced against extract_llm's cost alone, which on a preset like
// codesmart displays the component as comfortably POSITIVE while its own arithmetic proves
// it negative. It inverts the conclusion in the single field an operator reads.
//
// The signature now takes a RATE and applies it internally to GrossSavedTokens, so the
// mistake is unrepresentable. This test fails if anyone re-wires it to a pre-multiplied
// total, because that is exactly the class of bug that silently outlives a PR.
func TestNetValueUsesComponentOwnSavingsNotPipelineTotal(t *testing.T) {
	resetExtract()
	// The component itself saved 1,000 tokens and spent $0.05.
	RecordExtractionSaving("extract_llm", 1000)
	const rate = 0.30 / 1e6 // cache-read rate per token
	s := ExtractSnapshot(0.05, rate, 0, 0)

	wantGross := 1000 * rate
	if d := s.GrossValueUSD - round4(wantGross); d > 1e-9 || d < -1e-9 {
		t.Fatalf("GrossValueUSD = %v, want %v (1,000 own tokens x rate)", s.GrossValueUSD, round4(wantGross))
	}
	if net, known := s.Net(); !known || net >= 0 {
		t.Fatalf("net must be negative (got %v, known=%v): spent $0.05 to save $%.6f",
			net, known, wantGross)
	}
	// The pipeline-wide figure in a real run is orders of magnitude larger. Prove the
	// snapshot is NOT reading anything like it: had a 2,000,000-token pipeline total leaked
	// in at this rate, gross would be ~$0.60 and net would flip positive.
	if s.GrossValueUSD > 0.01 {
		t.Fatalf("GrossValueUSD = %v looks like a pipeline-wide total, not this component's",
			s.GrossValueUSD)
	}
	if s.GrossSavedTokens != 1000 {
		t.Fatalf("GrossSavedTokens = %d, want 1000", s.GrossSavedTokens)
	}
}

// The latency brake reads this accessor, so it must report the observed mean and the call
// count (0 calls => no signal, must not read as "fast").
func TestExtractionAvgLatencyMs(t *testing.T) {
	resetExtract()
	if avg, calls := ExtractionAvgLatencyMs("extract_llm"); avg != 0 || calls != 0 {
		t.Fatalf("with no calls expected (0,0), got (%v,%d)", avg, calls)
	}
	RecordExtractionCall("extract_llm", 4000)
	RecordExtractionCall("extract_llm", 8000)
	avg, calls := ExtractionAvgLatencyMs("extract_llm")
	if calls != 2 || avg != 6000 {
		t.Fatalf("got (%v,%d), want (6000,2)", avg, calls)
	}
}
