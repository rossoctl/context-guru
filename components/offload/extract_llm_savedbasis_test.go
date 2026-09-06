package offload

import (
	"context"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// summarizingModel returns a Starlark program that keeps the first lines of the body AND sets a
// SUMMARY, so the splice takes the marker-plus-summary shape that `apply` actually writes:
//
//	projected + "\n[" + summary + "] " + marker + recovery hint
//
// shrinkingModel is not enough for these tests. Its reply defines a `transform` function and never
// assigns OUTPUT, so the code leg produces nothing and the deterministic fallback supplies the
// projection — with an empty summary. That fixture exercises the marker overhead (~23 tokens) but
// not the SUMMARY, which is the dominant term in the gap under test (#195): exercising the shape
// the splice writes is not the same as exercising it with the subject of the fix inside it.
type summarizingModel struct {
	calls   int
	summary string
}

func (m *summarizingModel) Complete(_ context.Context, _ string) (string, error) {
	m.calls++
	// 40 of the body's 400 lines: enough to clear the acceptance check's keep-ratio floor (a
	// result below ~5% of the body is refused as insane, which is what a one-line OUTPUT hit),
	// small enough that the compaction is large and the fixture's numbers are unambiguous.
	return "```python\nOUTPUT = \"\\n\".join(INPUT.split(\"\\n\")[0:40])\nSUMMARY = \"" +
		m.summary + "\"\n```", nil
}

// A realistic one-line digest, kept just under clipSummary's 120-rune bound so the assertions can
// look for it verbatim in the spliced text — a longer one comes back clipped with an ellipsis and
// `Contains` would fail for a reason that has nothing to do with the subject. Tens of tokens, an
// order of magnitude more than the marker and hint the sweep's fix was about.
const basisSummary = "worker log: 400 identical INFO batch lines, no ERROR or WARN anywhere, " +
	"no batch reported a failure"

// THE FRESH PATH must book the saving the WIRE saw: the candidate minus the message as spliced.
//
// It booked `before - TextTokens(projection)` instead, carried out of runCall. The message that
// goes upstream is the projection PLUS the summary, the marker and the recovery hint, so the figure
// overstated every candidate by all three — and the summary dominates that, so the overstatement is
// variable per candidate and does not average out. #195.
//
// What the broken version did, so this test can be audited from outside: on the fixture below it
// booked TextTokens(body) - TextTokens("<first line>\n"), while the message it sent was
// "<first line>\n[<summary>] <<cg:HASH>> [full output: call cg_expand]". Both figures are large and
// positive; they differ by the summary + marker + hint, which is what the assertions below pin.
//
// Two consumers make it more than a reporting artefact: metrics.RecordExtractionSaving (the /stats
// figure two arms of a comparison are read from) and e.ratios.observe, which prices the NEXT call —
// so an optimistic saving argued for spending more.
func TestExtractLLMBooksWhatTheSplicedMessageActuallySaved(t *testing.T) {
	model := &summarizingModel{summary: basisSummary}
	e := newTimeoutTestComponent(t, model) // min_tokens: 1, strategy: code, gate off
	st := store.NewMemory(store.Options{MaxEntries: 400})
	body := strings.Repeat("2026-08-31T10:00:00Z INFO worker: processed batch\n", 400)
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("summarize the worker log"), toolResultMsg(body),
	}}
	c := pricedCtx("saved-basis-fresh", st, model)
	rep := &components.Report{Component: "extract_llm"}
	savedBefore := extractGrossSaved("extract_llm")
	if _, err := e.Offload(req, rep, c); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}

	// PRECONDITIONS. The call happened, the splice happened, and the spliced text carries the
	// SUMMARY — without the last one the fixture measures the marker overhead alone and the gap
	// this test exists for is not in it.
	if model.calls == 0 {
		t.Fatal("the extraction model was never called, so nothing was booked")
	}
	spliced := schema.MessageText(req.Input[1])
	if spliced == body {
		t.Fatal("the candidate was not spliced, so there is no saving to attribute")
	}
	if !strings.Contains(spliced, basisSummary) {
		t.Fatalf("the spliced message carries no summary segment, so the dominant term in the "+
			"projection-vs-wire gap is absent from this fixture: %.200q", spliced)
	}
	// The projection is exactly what the splice put before the summary segment, i.e. the model's
	// own result — the text the broken version subtracted. LastIndex, not Index: markElisions may
	// have added "... N lines elided ..." notes inside the projection, and the summary segment is
	// the final line by construction.
	projection := spliced[:strings.LastIndex(spliced, "\n[")]
	wire := schema.TextTokens(body) - schema.TextTokens(spliced)
	projected := schema.TextTokens(body) - schema.TextTokens(projection)
	if wire <= 0 {
		t.Fatal("the message did not shrink, so there is no wire figure to compare against")
	}
	// THE FIXTURE MUST SEPARATE THE TWO IMPLEMENTATIONS. If the summary, marker and hint were
	// free, both bases would produce the same number and every assertion below would hold for
	// the projection-based code too.
	if projected <= wire {
		t.Fatalf("the projection-based figure (%d) is not larger than the wire figure (%d), so "+
			"this fixture cannot tell the two bases apart", projected, wire)
	}

	// THE POSITIVE: the booked figure is the wire's, to the token.
	if got := int(extractGrossSaved("extract_llm") - savedBefore); got != wire {
		t.Errorf("RecordExtractionSaving booked %d tokens; the message it sent shrank by %d "+
			"(the projection-only figure is %d). extract_llm_sweep books the spliced message, so "+
			"two arms of a comparison were not measuring the same thing — and this figure also "+
			"feeds the ratio tracker the economic gate spends against", got, wire, projected)
	}
	if len(rep.Calls) == 0 {
		t.Fatal("no ModelCall row was reported, so the ledger's figure cannot be checked")
	}
	ledger := 0
	for _, call := range rep.Calls {
		ledger += call.SavedTokens
	}
	if ledger != wire {
		t.Errorf("the ledger claims %d saved tokens; the message sent shrank by %d (projection "+
			"only: %d)", ledger, wire, projected)
	}
}

// A REPLAY must be valued on the same basis as the turn that made the decision.
//
// The replay path credited `content - TextTokens(cached.Projected)` at the repeat rate. Correcting
// only the fresh path would leave the component contradicting ITSELF — the same compaction worth
// more on every replay turn than on the turn it was made — and replays are the steady state, so
// most of the reported value would come from the overstated side. The sweep was in exactly that
// state for one commit (c442670).
func TestExtractLLMReplayBooksWhatTheReplayedMessageActuallySaved(t *testing.T) {
	model := &summarizingModel{summary: basisSummary}
	e := newTimeoutTestComponent(t, model)
	st := store.NewMemory(store.Options{MaxEntries: 400})
	c := pricedCtx("saved-basis-replay", st, model)
	// CacheRead is 1 USD/token in pricedCtx, so the credited value equals the token count and
	// clears round4's 1e-4 step. repeatPerToken is the read rate on a warm cache-aware turn,
	// which is what the replay path credits at.
	body := strings.Repeat("2026-08-31T10:00:00Z INFO worker: processed batch\n", 400)
	newReq := func() *bschemas.BifrostChatRequest {
		return &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
			userMsg("summarize the worker log"), toolResultMsg(body),
		}}
	}

	// Turn 1 takes the decision.
	r1 := components.Report{Component: "extract_llm"}
	if _, err := e.Offload(newReq(), &r1, c); err != nil {
		t.Fatal(err)
	}
	if model.calls == 0 {
		t.Fatal("turn 1 made no call, so turn 2 has no frozen decision to replay")
	}

	// Turn 2 replays it, and only turn 2's booking is measured.
	req2 := newReq()
	before := capturedText(req2)
	valueBefore := extractGrossValue("extract_llm")
	r2 := components.Report{Component: "extract_llm"}
	if _, err := e.Offload(req2, &r2, c); err != nil {
		t.Fatal(err)
	}
	if r2.Replays == 0 {
		t.Fatalf("turn 2 replayed nothing (gates: %v, events: %v)", r2.Gates, r2.Events)
	}
	spliced := schema.MessageText(req2.Input[1])
	if spliced == before[1] {
		t.Fatal("the replay rewrote nothing, so there is no wire figure to compare against")
	}
	if !strings.Contains(spliced, basisSummary) {
		t.Fatalf("the replayed message carries no summary segment, so the dominant term in the "+
			"gap under test is absent: %.200q", spliced)
	}
	projection := spliced[:strings.LastIndex(spliced, "\n[")]
	wire := schema.TextTokens(before[1]) - schema.TextTokens(spliced)
	projected := schema.TextTokens(before[1]) - schema.TextTokens(projection)
	if wire <= 0 {
		t.Fatal("the replayed message did not shrink, so there is no wire figure to compare")
	}
	if projected <= wire {
		t.Fatalf("the projection-based figure (%d) is not larger than the wire figure (%d), so "+
			"this fixture cannot tell the two bases apart", projected, wire)
	}
	booked := extractGrossValue("extract_llm") - valueBefore
	if int(booked+0.5) != wire {
		t.Errorf("the replay credited %v tokens of value; the message it sent shrank by %d "+
			"(projection only: %d). Valuing a replay against the stored projection makes the same "+
			"compaction worth more on every replay turn than on the turn it was made — and "+
			"replays are the steady state", booked, wire, projected)
	}
}

// A SINGLE-FLIGHT FOLLOWER's message shrinks too, and its saving must NOT be booked a second time.
//
// This test exists because of what the wire measurement did to the `out[k].called` guard. While
// `saved` was computed in runCall's accept branch, a follower — which returns before that branch —
// carried saved == 0, so the guard prevented nothing and its own comment said as much. Phase 3 is a
// different matter: a follower's slot holds a non-empty `projected`, so it does not take the
// `projected == ""` skip, it splices, and the wire measurement stores a POSITIVE saving for it.
// `called` is now the only thing between that number and RecordExtractionSaving, e.ratios.observe
// and calls[k].SavedTokens — so the guard needs a test, and it had none.
//
// The contract it upholds is metrics.RecordExtractionSaving's own: "count each distinct compaction
// once — the caller dedups by content key". Four byte-identical outputs are one compaction derived
// by one call, spliced into four messages. Booking four would quadruple the reported saving of one
// model call, and would feed the ratio tracker four observations of work done once.
//
// IT ALSO GUARDS THE BASIS, beyond the purpose stated above, and that is worth writing down because
// a reader deciding what this test protects will otherwise under-read it: changing phase 3's basis
// in place — `before - TextTokens(out[k].projected)` rather than the spliced message — fails this
// test too, at 3,953 against 3,907. Found by the reviewing session on #216 with a variant mutation.
//
// AND THE DEDUP RIDES A SCHEDULING RACE: the leader must still hold the single-flight key when the
// three followers reach extractInflight.Do, and summarizingModel.Complete is pure string work that
// returns immediately. Verified 50/50 at -count=50. Deliberately left as is, because a lost race
// FATALS on the deduped_inflight_extraction and model.calls preconditions rather than passing
// vacuously — so a red here says whether the guard broke or the race was simply lost, which is the
// property that matters. Do not read a failure on those two lines as a defect in the guard.
func TestExtractLLMDoesNotBookASingleFlightFollowersSaving(t *testing.T) {
	// Byte-identical bodies, so all four share one extraction key and three become followers.
	// The text is distinct from every other fixture here because extractInflight's group is
	// process-wide and a collision would make this test measure another test's call.
	body := strings.Repeat("savedbasis follower fixture, identical across all four candidates\n", 400)
	const identical = 4
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("summarize these four identical outputs"),
	}}
	for i := 0; i < identical; i++ {
		req.Input = append(req.Input, toolResultMsg(body))
	}
	model := &summarizingModel{summary: basisSummary}
	e := newTimeoutTestComponent(t, model)
	rep := &components.Report{Component: "extract_llm"}
	c := &components.Ctx{
		Session: "savedbasis-follower", Ctx: context.Background(),
		Store: store.NewMemory(store.Options{MaxEntries: 400}), CtxWindow: 1_000_000,
		Model: components.ModelSpec{Static: model, Incoming: model},
		// Caching off, so the tail gate lets every candidate through and all four reach the
		// concurrent phase — the same reason TestConcurrentCallsDoNotRaceOnTheGateHistogram does it.
		CacheAware: false, MaxCachedIdx: -1,
	}
	savedBefore := extractGrossSaved("extract_llm")
	if _, err := e.Offload(req, rep, c); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}

	// PRECONDITIONS. Followers ran, one call was made, and — the one that makes this test about
	// phase 3 rather than about a slot that never got there — EVERY message was spliced,
	// followers included.
	if got := rep.Gates["deduped_inflight_extraction"]; got != identical-1 {
		t.Fatalf("deduped_inflight_extraction = %d, want %d: without followers there is no "+
			"double-booking to prevent (gates: %v)", got, identical-1, rep.Gates)
	}
	if model.calls != 1 {
		t.Fatalf("the model was called %d times; single-flight was supposed to collapse the four "+
			"identical candidates into one call", model.calls)
	}
	spliced := schema.MessageText(req.Input[1])
	for i := 1; i <= identical; i++ {
		if got := schema.MessageText(req.Input[i]); got == body {
			t.Fatalf("message %d went upstream verbatim, so this fixture does not exercise a "+
				"follower whose message shrank", i)
		}
	}
	oneWire := schema.TextTokens(body) - schema.TextTokens(spliced)
	if oneWire <= 0 {
		t.Fatal("no message shrank, so there is no saving to book once or four times")
	}

	// THE POSITIVE: one compaction's worth is booked, from four spliced messages.
	if got := int(extractGrossSaved("extract_llm") - savedBefore); got != oneWire {
		t.Errorf("RecordExtractionSaving booked %d tokens for %d messages spliced from ONE model "+
			"call; one compaction is worth %d. RecordExtractionSaving's contract is to count each "+
			"distinct compaction once, and the ratio tracker that prices future calls is fed from "+
			"the same place — so booking a follower's saving both inflates the reported figure and "+
			"argues for making more calls on evidence of work done once", got, identical, oneWire)
	}
	if len(rep.Calls) != 1 {
		t.Fatalf("got %d ledger rows for one model call: %+v", len(rep.Calls), rep.Calls)
	}
	if rep.Calls[0].SavedTokens != oneWire {
		t.Errorf("the ledger row claims %d saved tokens; the message its call compacted shrank by "+
			"%d", rep.Calls[0].SavedTokens, oneWire)
	}
}
