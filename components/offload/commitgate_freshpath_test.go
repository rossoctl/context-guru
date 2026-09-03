package offload

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// pricedCtx is a context whose saved-token rates are large enough for the savings counters to be
// OBSERVABLE. GrossValueUSD is round4 — 1e-4 granularity — and a fixture's saving is a few hundred
// tokens, so at real rates (~3e-7 USD/token) the recorded value rounds to 0.0000 and an assertion
// on it cannot see the defect. Two tests in this package were written without it and did not bind.
func pricedCtx(session string, st store.Store, model components.Model) *components.Ctx {
	return &components.Ctx{
		Session: session, Store: st, Ctx: context.Background(),
		Model:      components.ModelSpec{Static: model, Incoming: model},
		SelfRates:  components.TokenRates{Input: 10, CacheRead: 1, CacheWrite: 12.5, Output: 50},
		CacheAware: true, MaxCachedIdx: -1,
	}
}

// THE FRESH PATH must not book a saving, an accepted call or a ratio observation for a splice that
// did not happen.
//
// This was the half the first round missed. The metrics moved on the REPLAY path, while
// `runCall` — which computes them in a goroutine, before `wg.Wait()` and therefore before phase 3
// exists — still recorded `RecordExtractionSaving`, `RecordExtractionValue`, `e.ratios.observe`,
// `calls[k].Accepted` and `calls[k].SavedTokens` off the model reply alone. On a saturated reserve
// every phase-3 candidate is declined and the run reported the full saving anyway.
//
// The ratio feed is the most consequential of the five: it prices FUTURE calls, so a saving that
// never happened propagates into decisions about work not yet done.
func TestExtractLLMBooksNothingForAFreshSpliceItCouldNotStash(t *testing.T) {
	body := strings.Repeat("2026-08-31T10:00:00Z INFO worker: processed batch\n", 400)
	model := &shrinkingModel{}
	e := newTimeoutTestComponent(t, model)
	// A store that holds anything except a rewind payload: the model call succeeds, the reserve
	// refuses, phase 3 declines.
	spy := &spyStore{Memory: store.NewMemory(store.Options{MaxEntries: 400})}
	c := pricedCtx("fresh-gate", spy, model)
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("summarize the worker log"), toolResultMsg(body),
	}}
	rep := &components.Report{Component: "extract_llm"}
	valueBefore, savedBefore := extractGrossValue("extract_llm"), extractGrossSaved("extract_llm")
	ratioBefore := e.ratios.ratio()
	if _, err := e.Offload(req, rep, c); err != nil {
		t.Fatal(err)
	}
	// PRECONDITIONS: the model ran (so there was an outcome to book) and the splice was declined
	// (so booking it would be wrong). Without both, everything below passes vacuously.
	if model.calls == 0 {
		t.Fatal("the extraction model was never called, so nothing could have been booked")
	}
	if got := schema.MessageText(req.Input[1]); got != body {
		t.Fatalf("the candidate was spliced after all, so there is no declined splice to assert "+
			"about: %.80q", got)
	}
	if rep.Gates["stash_reserve_exhausted"] == 0 {
		t.Fatalf("the payload write was not refused (gates: %v)", rep.Gates)
	}

	if got := extractGrossSaved("extract_llm") - savedBefore; got != 0 {
		t.Errorf("RecordExtractionSaving booked %d tokens for a candidate that went upstream "+
			"verbatim", got)
	}
	if got := extractGrossValue("extract_llm"); got > valueBefore {
		t.Errorf("gross value rose from %v to %v for a splice that did not happen", valueBefore, got)
	}
	if got := e.ratios.ratio(); got != ratioBefore {
		t.Errorf("the ratio tracker moved %v -> %v on a declined splice. That ratio prices FUTURE "+
			"calls, so a saving that never happened now argues for making more calls whose output "+
			"will be refused the same way", ratioBefore, got)
	}
	for _, call := range rep.Calls {
		if call.Accepted {
			t.Error("the ledger row says accepted=true while the request kept its original — the " +
				"claim its own doc comment makes is that accepted is the never-worse outcome")
		}
		if call.SavedTokens != 0 {
			t.Errorf("the ledger row claims %d saved tokens for a candidate left verbatim",
				call.SavedTokens)
		}
	}
}

// The sweep's fresh path, same property.
//
// `adjudicate` recorded the saving while building its drop list, and the local
// `sweep_drop_would_not_shrink` pre-check does not cover phase 3's refusals: it is descriptor-only
// rather than marker-inclusive, and it cannot see the reserve at all.
func TestSweepBooksNothingForADropItCouldNotStash(t *testing.T) {
	asker := &labelAsker{verdict: "drop", needed: "none"}
	asker.cacheRead = 19595
	e := newSweepSmall(t, "")
	req := sweepReqStocked()
	originals := make([]string, len(req.Input))
	for i := range req.Input {
		originals[i] = schema.MessageText(req.Input[i])
	}
	spy := &spyStore{Memory: store.NewMemory(store.Options{MaxEntries: 400})}
	c := preExpiryCtx("s", asker, spy)
	c.SelfRates = components.TokenRates{Input: 10, CacheRead: 1, CacheWrite: 12.5, Output: 50}
	rep := &components.Report{Component: "extract_llm_sweep"}
	valueBefore := extractGrossValue("extract_llm_sweep")
	savedBefore := extractGrossSaved("extract_llm_sweep")
	if _, err := e.Offload(req, rep, c); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	if n := atomic.LoadInt64(&asker.calls); n != 1 {
		t.Fatalf("expected ONE prefix ask, got %d (gates: %v)", n, rep.Gates)
	}
	if rep.Events["sweep_dropped"] == 0 {
		t.Fatalf("the adjudicator dropped nothing, so no saving could have been booked "+
			"(gates: %v)", rep.Gates)
	}
	if rep.Gates["stash_reserve_exhausted"] == 0 {
		t.Fatalf("no drop was refused for want of a reserve slot (gates: %v)", rep.Gates)
	}
	for i := range req.Input {
		if schema.MessageText(req.Input[i]) != originals[i] {
			t.Fatalf("message %d was rewritten although its original could not be stored", i)
		}
	}
	if got := extractGrossSaved("extract_llm_sweep") - savedBefore; got != 0 {
		t.Errorf("the sweep booked %d saved tokens for drops that did not happen", got)
	}
	if got := extractGrossValue("extract_llm_sweep"); got > valueBefore {
		t.Errorf("sweep gross value rose from %v to %v for drops that did not happen",
			valueBefore, got)
	}
	for _, call := range rep.Calls {
		if call.Accepted {
			t.Error("the sweep's ledger row says accepted=true while every output went upstream " +
				"verbatim")
		}
		if call.SavedTokens != 0 {
			t.Errorf("the sweep's ledger row claims %d saved tokens for drops that were refused",
				call.SavedTokens)
		}
	}
}

// Every component that declines a removal for want of a reserve slot must COUNT it, or the counter
// the docs name as the signal to raise a budget is not a count of refusals.
func TestAgentDietRefusalsReachTheRefusalCounter(t *testing.T) {
	model := &silentModel{}
	c, err := newAgentDiet([]byte("delay_steps: 0\ncontext_steps: 1\nmin_step_tokens: 10\n" +
		"min_saved_tokens: 1\nmax_keep_ratio: 1.0\n"))
	if err != nil {
		t.Fatal(err)
	}
	d := c.(*AgentDiet)
	d.modelClient = model
	d.mode = markerFull
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("run the tests"),
		callMsg("t1"), toolResultMsgWithID("t1", strings.Repeat("pytest output line\n", 200)),
		callMsg("t2"), toolResultMsgWithID("t2", strings.Repeat("more output here\n", 200)),
	}}
	rep := &components.Report{Component: "agentdiet"}
	before := StashRefusals()
	if _, err := d.Offload(req, rep, ctxFor(store.NewMemory(store.Options{MaxEntries: 1}))); err != nil {
		t.Fatal(err)
	}
	if rep.Gates["stash_reserve_exhausted"] == 0 {
		t.Fatalf("agentdiet did not decline for want of a reserve slot, so there is no refusal to "+
			"count (gates: %v)", rep.Gates)
	}
	if got := StashRefusals() - before; got == 0 {
		t.Error("agentdiet declined a whole step's removals and StashRefusals() did not move. " +
			"docs/reference/config.md tells operators that stash_refused is THE signal to raise a " +
			"budget, and that it is deliberately upstream of expand_unresolved_missing — so a " +
			"deployment starving this component would read 0 while it declined every turn")
	}
}

func extractGrossValue(name string) float64 {
	if s := metricsExtractByComponent(name); s != nil {
		return s.GrossValueUSD
	}
	return 0
}

func extractGrossSaved(name string) int64 {
	if s := metricsExtractByComponent(name); s != nil {
		return s.GrossSavedTokens
	}
	return 0
}
