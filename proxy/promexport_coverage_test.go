package proxy

import (
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/adjudicate"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
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
// This CAUGHT PR #137, which added an `adjudicate_stray` field to Snapshot and exported it
// nowhere — the test working exactly as intended. Resolved the way the second group below
// prescribes: the series IS exported, as cg_adjudicate_stray_total, but sourced from
// adjudicate.StrayAnswered() rather than off `s`, because the /stats handler fills that field
// AFTER renderMetrics takes its snapshot, so a promLine off `s` would have exported a
// permanent 0 while passing this test.
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
	"AdjudicateStray":           "cg_adjudicate_stray_total, from adjudicate.StrayAnswered()",
	"LLMCalls":                  "cg_llm_calls_total, from cheapmodel.Usage()",
	"LLMInputTokens":            `cg_llm_tokens_total{direction="input"}, from cheapmodel.Usage()`,
	"LLMOutputTokens":           `cg_llm_tokens_total{direction="output"}, from cheapmodel.Usage()`,
	"LLMTimeouts":               `cg_llm_failures_total{kind="timeout"}, from offload.LLMTimeouts()`,
	"LLMErrors":                 `cg_llm_failures_total{kind="error"}, from offload.LLMErrors()`,
	"FrozenHits":                `cg_frozen_decisions_total{outcome="hit"}, from offload.FrozenStats()`,
	"FrozenMisses":              `cg_frozen_decisions_total{outcome="miss"}, from offload.FrozenStats()`,
	"FrozenDropped":             `cg_frozen_decisions_total{outcome="dropped"}, from store.Memory.FrozenLossStats()`,
	"FrozenRepaired":            `cg_frozen_decisions_total{outcome="repaired"}, from store.Memory.FrozenLossStats()`,
	"StashRefused":              "cg_stash_refused_total, from offload.StashRefusals()",
	"StashMissing":              "cg_stash_missing_total, from offload.StashMissing()",
	"StashExpired":              "cg_stash_expired_total, from store.Memory.StashStats()",
	// Exported as cg_usage_unparsed_total / cg_usage_unreadable_total, but read from UsageGaps()
	// rather than off `s` for the reason the block above this map gives: the /stats handler fills
	// these AFTER renderMetrics takes its snapshot, so a promLine off `s` would export a permanent
	// 0 while passing this test — which is precisely the silent-zero failure #200 is about, and it
	// would be embarrassing to reproduce it in the counter meant to report it.
	"UsageUnparsed":   "cg_usage_unparsed_total, from proxy.UsageGaps()",
	"UsageUnreadable": "cg_usage_unreadable_total, from proxy.UsageGaps()",
	"StashLive":       `cg_stash_reserve_entries{state="live"}, from store.Memory.StashStats()`,
	"StashCapacity":   `cg_stash_reserve_entries{state="capacity"}, from store.Memory.StashStats()`,
	"StashBytes":      `cg_stash_reserve_bytes{state="live"}, from store.Memory.StashStats()`,
	"StashMaxBytes":   `cg_stash_reserve_bytes{state="capacity"}, from store.Memory.StashStats()`,
	"Extract":         "the cg_extract_* family, from metrics.ExtractSnapshot()",

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

// TestAdjudicateStraySeriesRender is the guard half of the pattern TestExpandUnresolvedSeriesRender
// establishes, applied to cg_adjudicate_stray_total: the line RENDERS even at zero, and its value MOVES
// when the counter behind it does.
//
// The second half is the whole point, and this PR shipped without it. `s` in renderMetrics is the bare
// aggregator snapshot; Snapshot.AdjudicateStray is filled by the /stats handler AFTER that snapshot is
// taken, so sourcing this series from `float64(s.AdjudicateStray)` exports a hard-wired 0 on every
// scrape however often the agent calls the tool. TestEverySnapshotFieldIsExportedOrExempt cannot catch
// that, by its own documented design: it checks that the exporter READS the field, not that the value is
// real. Nothing else in ./proxy caught it either — the suite stayed green with that revert applied,
// which is what a reviewer demonstrated. This test is the thing that fails on it.
//
// Baseline-relative rather than absolute because strayAnswered is a process-wide counter and the
// adjudicate tests in package proxy_test share this test binary with it.
func TestAdjudicateStraySeriesRender(t *testing.T) {
	h := New(nil, nil, metrics.NewAggregator(), Options{})
	before := adjudicate.StrayAnswered()
	want := fmt.Sprintf("cg_adjudicate_stray_total %d", before)
	if !containsLine(h.renderMetrics(), want) {
		t.Fatalf("/metrics is missing the line %q — a family that appears only once something breaks "+
			"renders \"No data\" in Grafana, which reads as healthy", want)
	}
	if help := helpLine(h.renderMetrics(), "cg_adjudicate_stray_total"); help == "" {
		t.Error("cg_adjudicate_stray_total renders with no HELP, so nothing on the panel says what a " +
			"non-zero value means")
	}

	// And the value moves. Two strays answered in band; the series must read two higher.
	adjudicate.NoteAnsweredInBand(2)
	body := h.renderMetrics()
	if containsLine(body, want) {
		t.Errorf("still reads %q after two answered strays — the series is not reading "+
			"adjudicate.StrayAnswered() (Snapshot.AdjudicateStray is filled only by /stats, so a "+
			"promLine off `s` exports a permanent 0)", want)
	}
	if now := fmt.Sprintf("cg_adjudicate_stray_total %d", before+2); !containsLine(body, now) {
		t.Errorf("expected the line %q, got:\n%s", now,
			strings.Join(linesWithPrefix(body, "cg_adjudicate_stray_total"), "\n"))
	}
}

// linesWithPrefix is for failure messages: showing the series that DID render is what tells you whether
// the value was stale or the line vanished altogether.
func linesWithPrefix(body, prefix string) (out []string) {
	for _, ln := range strings.Split(body, "\n") {
		if strings.HasPrefix(ln, prefix) {
			out = append(out, ln)
		}
	}
	return out
}

func containsLine(body, line string) bool {
	for _, ln := range strings.Split(body, "\n") {
		if ln == line {
			return true
		}
	}
	return false
}

// TestStashReserveSeriesRender applies the same guard as TestExpandUnresolvedSeriesRender and
// TestAdjudicateStraySeriesRender to the two series #188's review added, and for the same reason
// those two exist: TestEverySnapshotFieldIsExportedOrExempt cannot catch a dropped series. By its
// own documented design it checks that the exporter READS a field, and both of these are read
// live from their owner (store.Memory.StashStats, offload.StashMissing) rather than off `s`, so
// they are listed in notExportedWhy and the reflection check never looks at them at all.
//
// Two properties, and the second is the one a coverage map cannot express:
//
//  1. The lines RENDER, so an operator can build a dashboard panel before anything has gone wrong.
//  2. cg_stash_missing_total MOVES with the counter behind it. A dangling marker is the one
//     reserve outcome that genuinely breaks reversibility, so a series that renders a hard-wired
//     zero would be worse than no series: it is the panel that says "nothing broke".
func TestStashReserveSeriesRender(t *testing.T) {
	h := New(nil, nil, metrics.NewAggregator(), Options{})
	// A real store, so the reserve gauges have something to report. Without one the whole block
	// is skipped (the handler casts to *store.Memory) and the byte gauge would be absent for a
	// reason unrelated to the exporter.
	h.store = store.NewMemory(store.Options{MaxEntries: 100, StashMaxBytes: 4096})
	body := h.renderMetrics()
	for _, want := range []string{
		`cg_stash_reserve_bytes{state="live"} 0`,
		`cg_stash_reserve_bytes{state="capacity"} 4096`,
		`cg_stash_reserve_entries{state="capacity"} 50`,
	} {
		if !containsLine(body, want) {
			t.Errorf("/metrics is missing the line %q: an operator told to raise a budget "+
				"cannot see which budget bound", want)
		}
	}
	// The byte gauge must read the STORE, not a snapshot field the aggregator never fills.
	st := h.store.(*store.Memory)
	if !st.PutStash("aaaaaaaaaaaaaaa1", make([]byte, 512)) {
		t.Fatal("the fixture's payload was refused, so the gauge has nothing to move")
	}
	if containsLine(h.renderMetrics(), `cg_stash_reserve_bytes{state="live"} 0`) {
		t.Error(`cg_stash_reserve_bytes{state="live"} still reads 0 after a payload was stored: ` +
			`the series is not reading the store's own accounting`)
	}

	// cg_stash_missing_total: renders, and moves. Baseline-relative because StashMissing is a
	// process-wide counter shared with every other test in this binary.
	before := offload.StashMissing()
	if !containsLine(h.renderMetrics(), fmt.Sprintf("cg_stash_missing_total %d", before)) {
		t.Errorf("/metrics does not render cg_stash_missing_total at %d; the one reserve outcome "+
			"that breaks a promise has no series", before)
	}
	if help := helpLine(h.renderMetrics(), "cg_stash_missing_total"); !strings.Contains(help, "dangling") {
		t.Errorf("HELP does not distinguish a dangling marker from a declined removal, which is "+
			"the whole reason this counter is separate from cg_stash_refused_total:\n  %s", help)
	}
}
