package proxy

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/metrics"
)

// TestEverySnapshotFieldIsExportedOrExempt.
//
// promexport.go is a hand-written list of promLine calls and nothing made it keep up with
// metrics.Snapshot, so fields fell out silently. expand_unresolved_malformed / _missing
// did: metrics.go calls the second one "the ALERTABLE one" and it reached /stats and the
// dashboard but never Prometheus, which is the only one of the three an operator can page
// on. The gap was invisible because a metric that was never added looks exactly like a
// metric nobody wanted.
//
// So: every field of Snapshot must either be read by the exporter or be listed in
// notExportedWhy with a reason. Adding a field to Snapshot without doing one of the two
// FAILS THE BUILD.
//
// EXPECTED TO FAIL ON PR #137, and that is the test working as intended: that PR adds an
// `adjudicate_stray` field to Snapshot and exports it nowhere. The fix is to export it, or
// to list it below with an honest reason — not to delete this test.
//
// What it checks is that the exporter READS the field, not that a particular series
// renders, and NOT that the value is real: `s` in renderMetrics is the bare aggregator
// snapshot, so any field the /stats handler fills afterwards is zero there and a promLine
// off it exports a permanent 0. Reflection CANNOT see that: a field read from `s` passes
// this test whatever the value. cg_frozen_decisions_total was exactly that bug — all four
// outcomes read s.Frozen*, which renderMetrics never fills, so the series exported 0 on
// every scrape (fixed in this change by sourcing it as /stats does; a test that catches the
// class would have to render against a snapshot with a distinct value per field, which
// needs a seam renderMetrics does not have). Rendered shape is covered by
// promexport_help_test.go, promexport_components_test.go and TestExpandUnresolvedSeriesRender.
func TestEverySnapshotFieldIsExportedOrExempt(t *testing.T) {
	src, err := os.ReadFile("promexport.go")
	if err != nil {
		t.Fatal(err)
	}
	rt := reflect.TypeOf(metrics.Snapshot{})
	for i := range rt.NumField() {
		name := rt.Field(i).Name
		read := regexp.MustCompile(`\bs\.` + name + `\b`).Match(src)
		why, exempt := notExportedWhy[name]
		switch {
		case read && exempt:
			t.Errorf("Snapshot.%s IS read by the exporter now, so its entry (%q) is stale — "+
				"delete it from notExportedWhy", name, why)
		case !read && !exempt:
			t.Errorf("Snapshot.%s reaches /stats but no promLine in promexport.go reads it, "+
				"so it never reaches Prometheus. Export it, or add it to notExportedWhy with "+
				"a reason it should not be a cg_* series.", name)
		}
	}
}

// notExportedWhy is why a Snapshot field is not read by the exporter. One line each, and
// the honest reason: an entry here is a claim that /metrics loses nothing by it, so "not
// exported yet" is spelled out as such rather than dressed up as a decision.
var notExportedWhy = map[string]string{
	// Derived: PromQL computes these from series that ARE exported, and a second series
	// would be a number that can disagree with its own inputs.
	"AdjustedSaved":  "cg_saved_tokens_total - cg_wasted_tokens_total",
	"FrozenFlips":    `cg_frozen_decisions_total{outcome="dropped"} - {outcome="repaired"}`,
	"SSEBufferedPct": "a ratio of the two cg_sse_streams_total paths",

	// Exported, but read live from the counter's owner instead of off the Snapshot — same
	// numbers, and /stats prices them identically on purpose. For a field the /stats
	// handler fills AFTER taking the snapshot this is required rather than a preference:
	// renderMetrics holds the bare aggregator snapshot, where those fields are zero.
	"ExpandUnresolvedMalformed": `cg_expand_unresolved_total{reason="malformed"}, from expand.Unresolved()`,
	"ExpandUnresolvedMissing":   `cg_expand_unresolved_total{reason="missing"}, from expand.Unresolved()`,
	"LLMCalls":                  "cg_llm_calls_total, from cheapmodel.Usage()",
	"LLMInputTokens":            `cg_llm_tokens_total{direction="input"}, from cheapmodel.Usage()`,
	"LLMOutputTokens":           `cg_llm_tokens_total{direction="output"}, from cheapmodel.Usage()`,
	"LLMTimeouts":               `cg_llm_failures_total{kind="timeout"}, from offload.LLMTimeouts()`,
	"LLMErrors":                 `cg_llm_failures_total{kind="error"}, from offload.LLMErrors()`,
	"FrozenHits":                `cg_frozen_decisions_total{outcome="hit"}, from offload.FrozenStats()`,
	"FrozenMisses":              `cg_frozen_decisions_total{outcome="miss"}, from offload.FrozenStats()`,
	"FrozenDropped":             `cg_frozen_decisions_total{outcome="dropped"}, from store.Memory.FrozenLossStats()`,
	"FrozenRepaired":            `cg_frozen_decisions_total{outcome="repaired"}, from store.Memory.FrozenLossStats()`,
	"Extract":                   "the cg_extract_* family, from metrics.ExtractSnapshot()",

	// Not numbers. Prometheus has no string sample, and a list of names would have to
	// become a label — which is what the cg_component_* family already is.
	"Mode":             "a string; the operating mode is a label, not a measurement",
	"ObserveNotice":    "prose banner for /stats readers",
	"ObserveLLMNotice": "prose banner for /stats readers",
	"TopPassthrough":   `component NAMES; the counts are cg_component_runs_total{outcome="mutated"}`,
	"TopDiscarded":     `component NAMES; the count is cg_component_runs_total{outcome="discarded"}`,
	"KeepAlive":        "typed `any`; the host fills it with a ledger this package cannot name",

	// Configured budgets, not measurements: constant for the process's life, so a series
	// would only ever restate a flag. Read them off /stats when a *_timeouts is non-zero.
	"LLMCallTimeoutMs":       "configured budget, not a measurement",
	"SummarizeCallTimeoutMs": "configured budget, not a measurement",
	"AgentDietCallTimeoutMs": "configured budget, not a measurement",

	// cmdfilter attribution is keyed by filter and by output SELECTOR, and the selector set
	// is open by design (it is the backlog of filters worth writing) — not a bounded label
	// set the way the component names are.
	"CmdfilterFamilies": "unbounded label set; dashboard-only",
	"CmdfilterFilters":  "unbounded label set; dashboard-only",
	"CmdfilterMisses":   "unbounded label set; dashboard-only",

	// Observe mode's HYPOTHETICALS. metrics.go keeps them in a namespace that shares no key
	// with an enforced metric so nothing can sum a projection into a saving; as cg_* series
	// they would sit beside the enforced ones in the same metric browser, which is the
	// confusion that invariant exists to prevent.
	"ObserveRequests":          "observe-mode hypothetical",
	"ActualBaselineTokens":     "observe-mode hypothetical",
	"ProjectedOptimizedTokens": "observe-mode hypothetical",
	"PotentialSavedTokens":     "observe-mode hypothetical",
	"PotentialSavingsPct":      "observe-mode hypothetical",
	"PotentialComponents":      "observe-mode hypothetical",
	"PotentialOverheadMsAvg":   "observe-mode hypothetical",
	"ObserveQueue":             "observe-mode off-path pool; its drops undercount the hypotheticals, so they belong with them",

	// NOT EXPORTED YET, and each of these arguably should be. Listed rather than left
	// silent: this change deliberately adds ONE family (cg_expand_unresolved_total, the
	// alertable one) instead of growing the exposition by fourteen series inside a
	// dashboard PR. Moving any entry out of this map is a small, self-contained change.
	"LLMTruncated":          "NOT EXPORTED YET — full price, zero result; a real alert candidate",
	"SummarizeTimeouts":     "NOT EXPORTED YET — summarize's fail-open path is invisible in Prometheus",
	"SummarizeErrors":       "NOT EXPORTED YET — as above",
	"AgentDietTimeouts":     "NOT EXPORTED YET — agentdiet's fail-open path, same gap",
	"AgentDietErrors":       "NOT EXPORTED YET — as above",
	"SyncEnforced":          "NOT EXPORTED YET — the machine-readable 'we did modify requests'",
	"CompactionResets":      "NOT EXPORTED YET — agent self-compaction restarting the cached prefix",
	"UpstreamMsAvgBypassed": "NOT EXPORTED YET — the bypassed baseline half of cg_upstream_latency_ms",
	"SSETTFBMsAvgBuf":       "NOT EXPORTED YET — buffered responses' time-to-last-byte",
	"SSEExpandAfterStream":  "NOT EXPORTED YET — the SSE peek's price; alert candidate",
	"AttemptedTokens":       "NOT EXPORTED YET — the honest savings denominator",
	"FrozenTokens":          "NOT EXPORTED YET — what cache-awareness left alone",
	"SavingsPctAttempted":   "NOT EXPORTED YET — saved/attempted, the ratio that does not trend to 0",
	"SavingsPctNewInput":    "NOT EXPORTED YET — saved/(fresh+cache_write+saved)",
}

// TestExpandUnresolvedSeriesRender: both reasons are present with a value even at zero (a
// family that appears only once something breaks renders "No data" in Grafana, which reads
// as healthy — the same argument as refusalReasons), and both track the real counter.
func TestExpandUnresolvedSeriesRender(t *testing.T) {
	h := New(nil, nil, metrics.NewAggregator(), Options{})
	zero := []string{
		`cg_expand_unresolved_total{reason="malformed"} 0`,
		`cg_expand_unresolved_total{reason="missing"} 0`,
	}
	for _, want := range zero {
		if !containsLine(h.renderMetrics(), want) {
			t.Errorf("/metrics is missing the line %q", want)
		}
	}
	if help := helpLine(h.renderMetrics(), "cg_expand_unresolved_total"); !strings.Contains(help, "ALERTABLE") {
		t.Errorf("HELP does not say which of the two reasons to page on:\n  %s", help)
	}

	// And the values move. A 16-hex id is one this proxy mints, so it classifies as
	// `missing` — the alertable case; "nothex" cannot be one, so it is `malformed`.
	expand.NoteUnresolved("0123456789abcdef")
	expand.NoteUnresolved("nothex")
	body := h.renderMetrics()
	for _, still := range zero {
		if containsLine(body, still) {
			t.Errorf("still reads 0 after a recorded failure: %q — the series is not reading "+
				"expand's counters (Snapshot.ExpandUnresolved* are filled only by /stats)", still)
		}
	}
}

func containsLine(body, line string) bool {
	for _, ln := range strings.Split(body, "\n") {
		if ln == line {
			return true
		}
	}
	return false
}
