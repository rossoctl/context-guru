package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/metrics"
)

// statsGoldenTopLevel is the /stats contract. deploy/harbor/*.py parses this
// payload to produce every published benchmark result, so a rename or a removal
// silently invalidates the reproduction path — a far worse failure than a build
// break, because the harness would keep running and report zeros.
//
// The rule this test enforces: fields may be ADDED, never renamed or removed.
// Adding a field here alongside the new key is the intended way to change it.
var statsGoldenTopLevel = []string{
	"actual_baseline_tokens",
	// Stray calls the agent made to the proxy-injected adjudication tool. Added to the reviewed
	// contract rather than loosening the assertion, per the rule above: it is the only figure that
	// can show a "do not call this yourself" description having stopped working.
	"adjudicate_stray",
	"adjusted_saved",
	// agentdiet_* are the same three fail-open figures for the `agentdiet` baseline,
	// which owns its own per-call budget (a window of steps sits between extract_llm's
	// single tool output and summarize's whole span). Separate keys because it runs in
	// its own arm: folded into llm_*, an agentdiet arm whose every reflection call
	// expired would report llm_timeouts 0 and read as having nothing to reduce.
	"agentdiet_call_timeout_ms",
	"agentdiet_errors",
	"agentdiet_timeouts",
	"attempted_tokens",
	"bounces",
	"cache_read_tokens",
	"cache_write_tokens",
	"cg_added_ms_avg",
	"compaction_resets",
	"components",
	"expand_unresolved_malformed",
	// The alertable half of reversibility: a marker id this proxy could have minted that resolved
	// to nothing, i.e. a cut advertised as reversible that was not. Added to the reviewed contract
	// rather than loosening the assertion, per the rule above. Nothing already here can substitute
	// for it — wasted_tokens counts successful re-serves, so a broken stash reads identically to a
	// session that never called expand.
	"expand_unresolved_missing",
	"extract",
	"fresh_input_tokens",
	"frozen_dropped",
	"frozen_flips",
	"frozen_hits",
	"frozen_misses",
	"frozen_repaired",
	"frozen_tokens",
	"llm_calls",
	// llm_call_timeout_ms / llm_errors / llm_timeouts make the compaction model's
	// fail-open path countable. Without them an arm whose extract_llm kept hitting its
	// per-call deadline is indistinguishable from an arm with little to compact — it
	// reads as FASTER, because it silently stopped working. The budget travels with the
	// counts because a timeout total means nothing without the ceiling it was measured
	// against. (llmd_smoke's collect.py parses all three into cg_llm_* row fields.)
	"llm_call_timeout_ms",
	"llm_errors",
	"llm_input_tokens",
	"llm_output_tokens",
	"llm_timeouts",
	"llm_truncated",

	"mode",
	"observe_hypothetical_requests",
	"output_tokens",
	"potential_overhead_ms_avg",
	"potential_saved_tokens",
	"potential_savings_pct",
	"projected_optimized_tokens",
	"requests",
	"saved_tokens",
	"savings_pct",
	"savings_pct_attempted",
	"savings_pct_new_input",
	"sse_buffered",
	"sse_buffered_pct",
	"sse_expand_after_stream",
	"sse_streamed",
	"sse_ttfb_ms_avg",
	"sse_ttfb_ms_avg_buffered",
	// The rewind reserve (#187, #188). stash_refused is the LEADING indicator for
	// expand_unresolved_missing, which cannot move until the agent happens to call expand — so
	// a run that had stopped being able to promise reversibility read as healthy. stash_missing
	// is its OPPOSITE and is listed separately on purpose: a refusal means nothing became
	// irreversible, while a missing payload means a dangling marker went out. The two shared one
	// key until the #188 review pointed out that made the safe case indistinguishable from the
	// dangerous one. stash_bytes/stash_max_bytes are the reserve's other budget — entries are a
	// poor proxy for memory in this namespace. Added to the reviewed contract rather than
	// loosening the assertion, per the rule above.
	//
	// In alphabetical order like the rest: the first four landed appended in #188 and broke the
	// ordering, which the test does not catch (it compares sets) and a reader does.
	"stash_bytes",
	"stash_capacity",
	"stash_expired",
	"stash_live",
	"stash_max_bytes",
	"stash_missing",
	"stash_refused",
	// summarize_* are the same three figures for `summarize`, which owns a SEPARATE
	// budget: its call covers the whole middle of the transcript (~57k prompt tokens
	// measured) rather than one tool output, so the two components cannot share a
	// ceiling — and a summarize-only pipeline reports llm_timeouts 0 however badly its
	// own deadline is being hit. (collect.py parses these into cg_summarize_* fields.)
	"summarize_call_timeout_ms",
	"summarize_errors",
	"summarize_timeouts",
	"sync_enforced",
	"tokens_after",
	"tokens_before",
	"top_discarded",
	"top_passthrough",
	"upstream_ms_avg",
	"upstream_ms_avg_bypassed",
	"wasted_tokens",
}

// statsGoldenComponent is the per-component object's contract. swebench.py reads
// saved_tokens, saved_tokens_unique, overcount_ratio, runs, acted and duration_ms
// by name.
var statsGoldenComponent = []string{
	"acted",
	// acted_fresh / acted_replay partition acted by whether the saving cost anything (#176).
	// `acted` alone counted a frozen decision replayed on a later turn — free, no model call —
	// in the same figure as the call that derived it, so a measured `acted: 239` beside
	// `reapplied_same_session: 2,291` was read as 239 paid extractions on a component that
	// made none. Added to the reviewed contract rather than loosening the assertion.
	"acted_fresh",
	"acted_replay",
	"discarded_changes",
	"duration_ms",
	"mutated",
	"overcount_ratio",
	"reverted",
	"runs",
	"saved_tokens",
	"saved_tokens_unique",
	// verdict reads the other three: a component that MUTATED without SAVING content tokens
	// is doing its job (cachesplit moves tokens between billing tiers rather than removing
	// them), and "acted: 0" beside "mutated: 755" has twice been filed as a bug against a
	// mechanism that was working. Stating the reading is cheaper than explaining it again.
	"verdict",
}

// harnessRequiredFields are the exact keys deploy/harbor reads. Listed separately
// and explicitly so the coupling is documented at the point of enforcement.
var harnessRequiredFields = []string{
	"requests", "tokens_before", "tokens_after", "saved_tokens", "savings_pct",
	"wasted_tokens", "bounces", "adjusted_saved", "components", "top_passthrough",
	"llm_calls", "llm_input_tokens", "llm_output_tokens",
	"cg_added_ms_avg", "upstream_ms_avg", "upstream_ms_avg_bypassed",
}

func TestStatsShapeIsUnchanged(t *testing.T) {
	agg := metrics.NewAggregator()
	// Populate enough that the component map is present and non-empty.
	agg.RecordAddedLatency(3)
	agg.RecordUpstreamLatency(100, false)
	agg.RecordUpstreamLatency(120, true)
	agg.RecordExpand(50)
	agg.RecordUsage(10, 1000, 100, 20)
	agg.RecordEligibility(400, 600)
	h := New(nil, nil, agg, Options{})

	w := httptest.NewRecorder()
	h.stats(w, httptest.NewRequest("GET", "/stats", nil))
	if w.Code != 200 {
		t.Fatalf("/stats -> %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q; want application/json", ct)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("/stats is not a JSON object: %v\n%s", err, w.Body.String())
	}
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Every golden key must still be present. This is the half that protects the
	// harness.
	have := map[string]bool{}
	for _, k := range keys {
		have[k] = true
	}
	for _, want := range statsGoldenTopLevel {
		if !have[want] {
			t.Errorf("/stats lost field %q — deploy/harbor/*.py parses it; fields may be added, never renamed or removed", want)
		}
	}
	// And the explicitly documented harness dependencies, spelled out again so the
	// failure message names the consumer.
	for _, want := range harnessRequiredFields {
		if !have[want] {
			t.Errorf("/stats lost %q, which deploy/harbor reads by name; the published benchmark reproduction breaks silently", want)
		}
	}
	// New keys are fine, but they must be recorded in the golden list so a reviewer
	// sees the payload growing on purpose.
	golden := map[string]bool{}
	for _, k := range statsGoldenTopLevel {
		golden[k] = true
	}
	for _, k := range keys {
		if !golden[k] {
			t.Errorf("/stats gained field %q; add it to statsGoldenTopLevel so the contract stays reviewed", k)
		}
	}
}

func TestStatsComponentShapeIsUnchanged(t *testing.T) {
	agg := metrics.NewAggregator()
	agg.Component(mkReport("extract", 1000, 800))
	h := New(nil, nil, agg, Options{})
	w := httptest.NewRecorder()
	h.stats(w, httptest.NewRequest("GET", "/stats", nil))

	var got struct {
		Components map[string]map[string]json.RawMessage `json:"components"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	comp, ok := got.Components["extract"]
	if !ok {
		t.Fatalf("component missing from /stats: %s", w.Body.String())
	}
	have := map[string]bool{}
	for k := range comp {
		have[k] = true
	}
	for _, want := range statsGoldenComponent {
		if !have[want] {
			t.Errorf("component object lost %q — deploy/harbor/swebench.py reads it by name", want)
		}
	}
	golden := map[string]bool{}
	for _, k := range statsGoldenComponent {
		golden[k] = true
	}
	for k := range comp {
		if !golden[k] {
			t.Errorf("component object gained %q; record it in statsGoldenComponent", k)
		}
	}
	// The internal dedup working set must never be serialized — it is unbounded and
	// meaningless to a consumer.
	if have["seenKeys"] || have["seen_keys"] {
		t.Error("the unique-savings working set leaked into /stats")
	}
}

func TestStatsWithNoAggregatorStaysAnEmptyObject(t *testing.T) {
	h := New(nil, nil, nil, Options{})
	w := httptest.NewRecorder()
	h.stats(w, httptest.NewRequest("GET", "/stats", nil))
	if got := w.Body.String(); got != "{}" {
		t.Errorf("/stats with no aggregator = %q; want {} (the harness treats {} as 'not ready')", got)
	}
}

// TestStatsNewFieldsCarryHonestValues checks the ADDED fields actually report the
// semantics they claim, not just that they exist.
func TestStatsNewFieldsCarryHonestValues(t *testing.T) {
	agg := metrics.NewAggregator()
	agg.Run(mkRunReport(1000, 800))
	agg.RecordEligibility(400, 600)
	// No usage recorded: the new-input ratio must be 0, NOT 100% from dividing
	// savings by themselves.
	snap := agg.Snapshot()
	if snap.SavedTokens != 200 {
		t.Fatalf("saved = %d", snap.SavedTokens)
	}
	if snap.SavingsPctAttempted != 50 {
		t.Errorf("savings_pct_attempted = %v; want 50 (200 saved of 400 attempted)", snap.SavingsPctAttempted)
	}
	if snap.SavingsPctNewInput != 0 {
		t.Errorf("savings_pct_new_input = %v with no usage data; must be 0, never ~100", snap.SavingsPctNewInput)
	}
	if snap.FrozenTokens != 600 {
		t.Errorf("frozen_tokens = %d; want 600", snap.FrozenTokens)
	}

	// With usage data it becomes computable: 200 / (100 fresh + 300 write + 200) = 33.3%.
	agg.RecordUsage(100, 5000, 300, 50)
	snap = agg.Snapshot()
	if snap.CacheReadTokens != 5000 || snap.CacheWriteTokens != 300 || snap.OutputTokens != 50 {
		t.Errorf("usage tiers wrong: %+v", snap)
	}
	if p := snap.SavingsPctNewInput; p < 33.3 || p > 33.4 {
		t.Errorf("savings_pct_new_input = %v; want ~33.33", p)
	}
}

// mkReport / mkRunReport build minimal component/run reports for the shape tests.
func mkReport(name string, before, after int) components.Report {
	return components.Report{Component: name, Kind: "offload",
		TokensBefore: before, TokensAfter: after, DurationMs: 1.5,
		CacheKeys: []string{"k1"}}
}

func mkRunReport(before, after int) components.RunReport {
	return components.RunReport{Session: "s", TokensBefore: before, TokensAfter: after}
}
