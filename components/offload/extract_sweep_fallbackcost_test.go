package offload

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/cheapmodel"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
)

// billingModel is a real cheapmodel client pointed at a local server that reports usage. It exists
// because `recordingModel` reports none, and this test's whole subject is that the fallback's TOKENS
// are priced — a fake that bills nothing would let the assertion pass against a fix that plumbs
// nothing. Going through cheapmodel.Anthropic also exercises the actual recording path
// (recordUsageCache -> the call sink), which is what fallbackAsk now reads.
func billingModel(t *testing.T, reply string) (components.Model, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":` + jsonQuote(reply) + `}],` +
			`"usage":{"input_tokens":31000,"output_tokens":420,` +
			`"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`))
	}))
	return cheapmodel.Anthropic{BaseURL: srv.URL, APIKey: "k", Model: "claude-sonnet-5",
		MaxTokens: 2048}, srv.Close
}

// sweepSpend reads this component's recorded extraction economics out of /stats' breakdown.
func sweepSpend(t *testing.T) (cost float64, calls int64, source string) {
	t.Helper()
	s := metrics.ExtractSnapshot(0, 0, 0, 0)
	row := s.ByComponent["extract_llm_sweep"]
	if row == nil {
		return 0, 0, ""
	}
	return row.ExtractionCostUSD, row.Calls, row.CostSource
}

// REVIEW FINDING (#178, MAJOR). `fallbackAsk` is a SECOND model call, on the request's own frontier
// model, carrying a sampled copy of every candidate — the expensive path by construction. Its cost
// and its wall time were both dropped from this component's accounting:
//
//   - `r.rec` was built from the PREFIX ask's usage and assigned exactly once, and `fallbackAsk`
//     returned only (string, error) — no usage, no cost. So RecordExtractionSpend saw $0.
//   - RecordExtractionCall was called before two of the three fallback points, so the fallback's
//     seconds never reached avg_latency_ms either.
//
// Before this PR that spend still reached /stats by accident, through cheapmodel's process totals
// which the host passed in as the `cost` argument. Making components price their own spend removed
// the accident: as soon as any component records spend the host fallback is discarded, so
// `extract_llm` + sweep behind a non-Anthropic upstream — where the prefix asker is nil and the
// sweep ALWAYS falls back — reported a component whose entire justification is cost as free, with a
// purely positive net_value_usd.
//
// This drives the no-asker path, which is that exact scenario.
func TestSweepFallbackAskPricesItsOwnCall(t *testing.T) {
	model, done := billingModel(t, `[{"i":0,"needed_by":"none","quote":"","verdict":"keep"}]`)
	defer done()

	costBefore, callsBefore, _ := sweepSpend(t)
	e := newSweepSmall(t, "")
	// preExpiryCtx with NO prefix asker: any non-Anthropic route, which is the reviewer's scenario.
	c := preExpiryCtx("fallbackcost", nil, store.NewMemory(store.Options{}))
	c.Model = components.ModelSpec{Incoming: model, Static: model}

	rep := &components.Report{Component: "extract_llm_sweep"}
	if _, err := e.Offload(sweepReqStocked(), rep, c); err != nil {
		t.Fatal(err)
	}
	// PRECONDITIONS. Without them every assertion below could pass because nothing ran.
	if rep.Gates["sweep_no_asker"] != 1 {
		t.Fatalf("the no-asker path was not taken, so the fallback was never reached (gates: %v)",
			rep.Gates)
	}
	if rep.Events["sweep_fallback_used"] != 1 {
		t.Fatalf("the fallback did not run (events: %v gates: %v)", rep.Events, rep.Gates)
	}
	if len(rep.Calls) == 0 {
		t.Fatalf("no ModelCall was reported for an adjudication that made one (gates: %v)",
			rep.Gates)
	}

	// THE LEDGER ROW: the dashboard's per-call view must not show the expensive leg as free.
	rec := rep.Calls[0]
	if rec.CostUSD <= 0 {
		t.Errorf("the ledger row prices the fallback at $%v — the defect this test exists for; "+
			"31,000 prompt + 420 completion tokens on a frontier model is not free", rec.CostUSD)
	}
	if rec.PromptTokens != 31000 || rec.CompletionTokens != 420 {
		t.Errorf("the fallback's tokens did not reach the row: prompt=%d completion=%d, want "+
			"31000/420", rec.PromptTokens, rec.CompletionTokens)
	}
	if rec.Strategy != "fallback" {
		t.Errorf("Strategy = %q, want %q: no prefix ask happened on this route, so the row must "+
			"not claim a leg that did not exist", rec.Strategy, "fallback")
	}

	// AND /stats: the counters the operator and the alert rule read.
	costAfter, callsAfter, source := sweepSpend(t)
	if callsAfter-callsBefore != 1 {
		t.Errorf("recorded %d calls for this adjudication, want 1", callsAfter-callsBefore)
	}
	if costAfter-costBefore <= 0 {
		t.Errorf("extract_llm_sweep.extraction_cost_usd moved by $%v: the fallback's spend is "+
			"still missing from /stats", costAfter-costBefore)
	}
	if source != "component" {
		t.Errorf("cost_source = %q, want \"component\": the component priced this call itself",
			source)
	}
}

// The other two fallback paths reach it AFTER the prefix ask's record already exists, so they are
// where the fold matters: the row has to grow to cover both legs rather than keep the prefix ask's
// numbers. This drives the zero-cache-read path — a mistimed window, which the counters say is the
// common one — and pins that the adjudication books TWO calls, not one.
func TestSweepCountsBothLegsWhenItFallsBackAfterAsking(t *testing.T) {
	model, done := billingModel(t, `[{"i":0,"needed_by":"none","quote":"","verdict":"keep"}]`)
	defer done()

	// A prefix ask that succeeds but reads nothing from cache: usage is real (Fresh 40, Output 90)
	// and CacheRead is 0, so the component asks again through the expensive path.
	asker := &fakeAsker{reply: `[{"i":0,"needed_by":"none","quote":"","verdict":"keep"}]`, cacheRead: 0}
	_, callsBefore, _ := sweepSpend(t)
	e := newSweepSmall(t, "")
	c := preExpiryCtx("bothlegs", asker, store.NewMemory(store.Options{}))
	c.Model = components.ModelSpec{Incoming: model, Static: model}

	rep := &components.Report{Component: "extract_llm_sweep"}
	if _, err := e.Offload(sweepReqStocked(), rep, c); err != nil {
		t.Fatal(err)
	}
	if rep.Gates["sweep_prefix_cache_read_ZERO"] != 1 || rep.Events["sweep_fallback_used"] != 1 {
		t.Fatalf("the ask-then-fall-back path was not exercised (gates: %v events: %v)",
			rep.Gates, rep.Events)
	}
	if len(rep.Calls) == 0 {
		t.Fatal("no ModelCall reported")
	}
	// TWO model calls went out, so two must be counted. Booking the pair as one is what let the
	// mean latency and the call count describe the cheap leg alone.
	_, callsAfter, _ := sweepSpend(t)
	if n := callsAfter - callsBefore; n != 2 {
		t.Errorf("recorded %d calls, want 2 (the prefix ask AND the fallback)", n)
	}
	rec := rep.Calls[0]
	if rec.Strategy != "prefix_ask+fallback" {
		t.Errorf("Strategy = %q, want %q", rec.Strategy, "prefix_ask+fallback")
	}
	// The row's tokens must be the SUM: 40 fresh + 31,000 from the fallback, 90 + 420 output.
	if rec.PromptTokens != 40+31000 || rec.CompletionTokens != 90+420 {
		t.Errorf("row tokens = prompt %d / completion %d, want %d/%d — the row still shows one "+
			"leg", rec.PromptTokens, rec.CompletionTokens, 40+31000, 90+420)
	}
}

// erroringModel fails the completion, after enough wall time to be measurable in whole
// milliseconds. That combination is the subject: a fallback that BURNED SECONDS AND THEN FAILED.
type erroringModel struct{ calls int64 }

var errFallbackBroke = errors.New("fallback upstream refused the completion")

func (m *erroringModel) Complete(_ context.Context, _ string) (string, error) {
	m.calls++
	time.Sleep(8 * time.Millisecond)
	return "", errFallbackBroke
}

// REVIEW FINDING (#178, round 4). `foldFallback()` was called per site, and all three sites sit
// AFTER `if reply, err = runFallback(); err != nil { return nil, r }` — so a fallback that failed
// never reached the fold. `runFallback` has already booked the leg into metrics via `recordLeg`, so
// /stats counted the call and its seconds while the ledger row kept only the prefix ask's
// LatencyMs and a Strategy naming a leg that was no longer the only one that ran.
//
// Latency rather than dollars, because on an error the sink is empty — `recordUsageCache` is reached
// only after a successful decode on both backends and neither errors after billing. But the fallback
// is the SLOW leg by construction, so a failed one puts tens of seconds into avg_latency_ms against
// a row showing milliseconds.
//
// THE NO-ASKER PATH IS THE SEVERE ONE and it is what this test drives: there, `r.rec` does not exist
// yet when the fallback runs, so on an error the row was never built at all — Component stayed ""
// and Offload dropped the entire row on its `call.rec.Component != ""` guard. /stats was left
// reporting a call the ledger had no row for.
//
// ASSERTED AS CROSS-SURFACE AGREEMENT, deliberately. There is no marshalled surface for a ModelCall
// inside /stats — the ledger rows travel to the dash Event, not this payload — so a JSON-tag
// assertion would be testing dash's converter rather than this fix. The invariant that actually
// broke is that the two surfaces disagreed, so that is what is pinned: if /stats counts the call,
// the ledger must carry a row for it. And the assertion is on rep.Calls AFTER Offload, i.e. past
// the guard that was doing the dropping, not on the pre-guard value.
func TestAFailedFallbackStillLandsInTheLedger(t *testing.T) {
	model := &erroringModel{}
	_, callsBefore, _ := sweepSpend(t)

	e := newSweepSmall(t, "")
	c := preExpiryCtx("foldonerror", nil, store.NewMemory(store.Options{})) // no prefix asker
	c.Model = components.ModelSpec{Incoming: model, Static: model}

	rep := &components.Report{Component: "extract_llm_sweep"}
	if _, err := e.Offload(sweepReqStocked(), rep, c); err != nil {
		// Fail-open: a broken fallback must not surface an error to the pipeline.
		t.Fatalf("Offload returned an error; the component must fail open: %v", err)
	}

	// PRECONDITIONS. Each one is a way this test could otherwise pass on a fixture that simply
	// declined, which is the shape that makes an assertion vacuous.
	if rep.Gates["sweep_no_asker"] != 1 {
		t.Fatalf("the no-asker path was not taken (gates: %v)", rep.Gates)
	}
	if model.calls != 1 {
		t.Fatalf("the fallback model was called %d times, want 1 — nothing errored, so there is "+
			"no failed leg to account for", model.calls)
	}
	if rep.Gates["sweep_fallback_failed"] != 1 {
		t.Fatalf("the fallback did not FAIL, so this exercises the success path instead of the "+
			"error path under test (gates: %v)", rep.Gates)
	}
	// And /stats booked it, which is the half that was never in doubt and is what makes the
	// missing row a DISAGREEMENT rather than a symmetric omission.
	_, callsAfter, _ := sweepSpend(t)
	if n := callsAfter - callsBefore; n != 1 {
		t.Fatalf("/stats recorded %d calls for the failed fallback, want 1", n)
	}

	// THE FIX: the ledger has a row for the call /stats counted.
	if len(rep.Calls) != 1 {
		t.Fatalf("/stats counted a call and the ledger carries %d rows: the row is built after "+
			"the error return, so Component stayed \"\" and Offload dropped it on its "+
			"Component != \"\" guard. This is the round-4 finding.", len(rep.Calls))
	}
	rec := rep.Calls[0]
	if rec.Component != "extract_llm_sweep" {
		t.Errorf("row Component = %q, want extract_llm_sweep — without it the caller's guard "+
			"drops the row however complete the rest of it is", rec.Component)
	}
	if rec.LatencyMs <= 0 {
		t.Errorf("row LatencyMs = %v: the failed fallback's wall time did not reach the ledger, "+
			"so avg_latency_ms carries seconds the per-call view cannot account for", rec.LatencyMs)
	}
	if rec.Strategy != "fallback" {
		t.Errorf("row Strategy = %q, want \"fallback\": no prefix ask happened on this route",
			rec.Strategy)
	}
	if rec.Rejection == "" {
		t.Error("row Rejection is empty on a failed fallback, so the row cannot say why it " +
			"produced nothing — the distinction ModelCall.Rejection exists for")
	}
	// Identity fields must be real, not placeholders: a row the dashboard cannot attribute to a
	// candidate size or a model is barely better than no row.
	if rec.CandidateTokens <= 0 {
		t.Errorf("row CandidateTokens = %d, want the inventory's real size", rec.CandidateTokens)
	}
}

// The other shape: the prefix ask succeeded, the cache read was zero, and the fallback then FAILED.
// Here r.rec already exists, so the row was never dropped — it just kept the prefix ask's latency
// while /stats carried both legs. Latency-only, and the fold has to cover it too.
func TestAFailedFallbackAfterAskingStillGrowsTheRow(t *testing.T) {
	model := &erroringModel{}
	asker := &fakeAsker{reply: `[{"i":0,"needed_by":"none","quote":"","verdict":"keep"}]`, cacheRead: 0}
	_, callsBefore, _ := sweepSpend(t)

	e := newSweepSmall(t, "")
	c := preExpiryCtx("foldonerror2", asker, store.NewMemory(store.Options{}))
	c.Model = components.ModelSpec{Incoming: model, Static: model}

	rep := &components.Report{Component: "extract_llm_sweep"}
	if _, err := e.Offload(sweepReqStocked(), rep, c); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	if rep.Gates["sweep_prefix_cache_read_ZERO"] != 1 || rep.Gates["sweep_fallback_failed"] != 1 {
		t.Fatalf("wanted a successful ask with a zero cache read and then a FAILED fallback "+
			"(gates: %v)", rep.Gates)
	}
	// Two legs went out, so /stats booked two.
	_, callsAfter, _ := sweepSpend(t)
	if n := callsAfter - callsBefore; n != 2 {
		t.Fatalf("/stats recorded %d calls, want 2 (the ask AND the failed fallback)", n)
	}
	if len(rep.Calls) != 1 {
		t.Fatalf("want one ledger row for the adjudication, got %d", len(rep.Calls))
	}
	rec := rep.Calls[0]
	// The row must name both legs and carry both their seconds. The failed leg sleeps 8ms, so a row
	// still showing only the ask is detectable: the fake asker returns instantly.
	if rec.Strategy != "prefix_ask+fallback" {
		t.Errorf("row Strategy = %q, want \"prefix_ask+fallback\" — a failed fallback still ran",
			rec.Strategy)
	}
	if rec.LatencyMs < 8 {
		t.Errorf("row LatencyMs = %v, want at least the failed fallback's ~8ms: /stats has both "+
			"legs' seconds and the per-call view must not show only the fast one", rec.LatencyMs)
	}
}
