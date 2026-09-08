package offload

import (
	"strings"
	"sync/atomic"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/extract"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// The sweep's phase 3 had the same defect as extract_llm's, one file over.
//
// putResult ran BEFORE applySweepDrop, so a drop refused for want of a reserve slot left a frozen
// cg:res: record for a removal that never happened. The same-session replay path above reads that
// record and bypasses the depth gate on the reasoning that the bytes were already sent — so on a
// later turn with a free slot, the output is removed from inside the provider's cached prefix.
func TestSweepFreezesNoDecisionForADropThatDidNotHappen(t *testing.T) {
	asker := &labelAsker{verdict: "drop", needed: "none"}
	asker.cacheRead = 19595
	e := newSweepSmall(t, "")
	req := sweepReqStocked()
	originals := make([]string, len(req.Input))
	for i := range req.Input {
		originals[i] = schema.MessageText(req.Input[i])
	}
	spy := &spyStore{Memory: store.NewMemory(store.Options{MaxEntries: 400})}
	rep := &components.Report{}
	if _, err := e.Offload(req, rep, preExpiryCtx("s", asker, spy)); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	// PRECONDITIONS. The adjudicator must have been asked and must have said "drop", or phase 3
	// was never reached and every assertion below passes on a transcript nothing touched.
	if n := atomic.LoadInt64(&asker.calls); n != 1 {
		t.Fatalf("expected ONE prefix ask, got %d (gates: %v)", n, rep.Gates)
	}
	if rep.Gates["stash_reserve_exhausted"] == 0 {
		t.Fatalf("no drop was refused for want of a reserve slot, so the defect's precondition "+
			"never held (gates: %v, events: %v)", rep.Gates, rep.Events)
	}
	// The invariant.
	if got := spy.decisionWrites(); len(got) != 0 {
		t.Errorf("the sweep froze %d decision(s) for a drop that did not happen: %v.\nA later "+
			"turn replays such a record at any depth, removing the output from inside the "+
			"provider's cached prefix", len(got), got)
	}
	for i := range req.Input {
		if got := schema.MessageText(req.Input[i]); got != originals[i] {
			t.Errorf("message %d was rewritten although its original could not be stored", i)
		}
	}
}

// The two rejection reasons must be TELLABLE APART, and one of them was unreachable.
//
// `adjudicate` names what it judged spent; phase 3 decides what actually got dropped. When the
// adjudicator found nothing, the row says "nothing was spent". When it found plenty and the reserve
// refused every one, the row has to say something else — otherwise the reserve-exhausted case reads
// as a plain rejection, which is the whole reason the second reason exists.
//
// It was dead code on arrival: moving the metrics out of the verdict loop deleted `removed`'s only
// assignment, so `removed == 0` was always true, every adjudication stamped "nothing was spent",
// and the `Rejection == ""` guard on the second reason never opened. Nothing caught it because the
// variable was still read, so it compiled and the suite stayed green.
func TestSweepTellsAReserveRefusalApartFromNothingSpent(t *testing.T) {
	t.Run("nothing was spent", func(t *testing.T) {
		asker := &labelAsker{verdict: "keep", needed: "none"}
		asker.cacheRead = 19595
		e := newSweepSmall(t, "")
		st := store.NewMemory(store.Options{MaxEntries: 400})
		rep := &components.Report{Component: "extract_llm_sweep"}
		if _, err := e.Offload(sweepReqStocked(), rep, preExpiryCtx("s", asker, st)); err != nil {
			t.Fatal(err)
		}
		assertSweepRejection(t, rep, "adjudicated: nothing was spent")
	})
	t.Run("spent but no descriptor was smaller", func(t *testing.T) {
		// The third path into an empty `drop`, and the one that used to be reported as the
		// adjudicator finding nothing: it judged outputs spent and the descriptor-only never-worse
		// pre-check removed every one of them. Reachable on outputs barely above the floor, where a
		// shape descriptor can be larger than the output it describes.
		asker := &labelAsker{verdict: "drop", needed: "none"}
		asker.cacheRead = 19595
		// Built directly rather than through newSweep, which prepends its own min_tokens: 2000 —
		// this case needs a floor BELOW the descriptor's size, which is the whole point of it.
		built, err := newExtractSweep([]byte("min_tokens: 5\nmin_inventory: 1\n"))
		if err != nil {
			t.Fatal(err)
		}
		e := built.(*ExtractSweep)
		st := store.NewMemory(store.Options{MaxEntries: 400})
		req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
			userMsg("do the thing"),
			toolResultMsgWithID("t1", "six little words right here now\n"),
			toolResultMsgWithID("t2", "seven other little words right here\n"),
		}}
		rep := &components.Report{Component: "extract_llm_sweep"}
		if _, err := e.Offload(req, rep, preExpiryCtx("s", asker, st)); err != nil {
			t.Fatal(err)
		}
		if rep.Gates["sweep_drop_would_not_shrink"] == 0 {
			t.Fatalf("the never-worse pre-check never fired, so this is not the case under test "+
				"(gates: %v, events: %v)", rep.Gates, rep.Events)
		}
		if rep.Events["sweep_dropped"] != 0 {
			t.Fatalf("a drop got through, so `drop` was not empty for the reason under test")
		}
		// The reason no longer names the skip — the GATES do, and they cannot fall out of date when
		// a skip path is added. What the reason owes the operator is that the model judged output
		// spent, which is the distinction "nothing was spent" destroyed.
		assertSweepRejection(t, rep, "adjudicated 2 spent, but none became a drop (see gates)")
		if rep.Gates["sweep_drop_would_not_shrink"] == 0 {
			t.Error("the reason defers to the gates for WHICH skip it was, so the gate has to be there")
		}
	})
	t.Run("spent but every verdict refused its own obligation", func(t *testing.T) {
		// The path the enumerating version reported as "nothing was spent": the model answers drop
		// while naming something that still needs the output, so extract.Judge sets
		// RefusedObligation and every candidate is skipped. Included because deriving the reason
		// from the verdict is only worth anything if it covers a skip the enumeration missed.
		asker := &labelAsker{verdict: "drop", needed: "the pytest run in step 4"}
		asker.cacheRead = 19595
		e := newSweepSmall(t, "")
		st := store.NewMemory(store.Options{MaxEntries: 400})
		rep := &components.Report{Component: "extract_llm_sweep"}
		if _, err := e.Offload(sweepReqStocked(), rep, preExpiryCtx("s", asker, st)); err != nil {
			t.Fatal(err)
		}
		if rep.Gates["sweep_drop_refused_obligation"] == 0 {
			t.Fatalf("no verdict refused its obligation, so this is not the case under test "+
				"(gates: %v, events: %v)", rep.Gates, rep.Events)
		}
		if rep.Events["sweep_dropped"] != 0 {
			t.Fatal("a drop got through, so `drop` was not empty for the reason under test")
		}
		if len(rep.Calls) == 0 {
			t.Fatal("no ModelCall row was reported")
		}
		for _, call := range rep.Calls {
			if call.Rejection == "adjudicated: nothing was spent" {
				t.Error("the ledger says the adjudicator found nothing spent, when it judged " +
					"EVERY output spent and we declined each one for self-contradiction. That is " +
					"the conflation these reasons exist to prevent, and the enumerating version " +
					"reported exactly this")
			}
			if !strings.Contains(call.Rejection, "spent, but none became a drop") {
				t.Errorf("rejection = %q, want the spent-but-not-dropped reason", call.Rejection)
			}
		}
	})
	t.Run("spent but no drop could be applied", func(t *testing.T) {
		asker := &labelAsker{verdict: "drop", needed: "none"}
		asker.cacheRead = 19595
		e := newSweepSmall(t, "")
		// A store that refuses every payload: the adjudicator judges outputs spent and phase 3
		// declines all of them.
		spy := &spyStore{Memory: store.NewMemory(store.Options{MaxEntries: 400})}
		rep := &components.Report{Component: "extract_llm_sweep"}
		if _, err := e.Offload(sweepReqStocked(), rep, preExpiryCtx("s", asker, spy)); err != nil {
			t.Fatal(err)
		}
		if rep.Events["sweep_dropped"] == 0 {
			t.Fatal("the adjudicator judged nothing spent, so this is the OTHER case and the " +
				"assertion below would pass for the wrong reason")
		}
		if rep.Gates["stash_reserve_exhausted"] == 0 {
			t.Fatalf("no drop was refused for want of a reserve slot (gates: %v)", rep.Gates)
		}
		assertSweepRejection(t, rep, "adjudicated spent, but no drop could be applied")
	})
}

func assertSweepRejection(t *testing.T, rep *components.Report, want string) {
	t.Helper()
	if len(rep.Calls) == 0 {
		t.Fatal("no ModelCall row was reported, so there is no rejection reason to assert on")
	}
	for _, call := range rep.Calls {
		if call.Rejection != want {
			t.Errorf("rejection = %q, want %q.\nAn operator reading the ledger cannot tell a "+
				"turn where nothing was worth dropping from a turn where everything was and the "+
				"store had no room — and those want opposite responses", call.Rejection, want)
		}
		if call.Accepted {
			t.Error("the row says accepted=true alongside a rejection reason: the same " +
				"self-contradictory row this change set out to remove")
		}
	}
}

// A declined replay must record nothing — and the honest statement of what this test still pins is
// narrower than what it was written for.
//
// It was written when the saving was computed from the stored DESCRIPTOR, so a declined replay could
// book a non-zero figure and moving the recorder above the ok gate was observable. Measuring the
// spliced message instead — the right fix, and one the review asked for — made `saved == 0` TRUE BY
// CONSTRUCTION on a decline, because sweepDrop only calls SetMessageText on success. So the savings
// half of this test cannot fail any more, whatever the recorder's position.
//
// That is worth stating rather than quietly leaving a test that looks like a guard:
//
//   - What still BINDS here is rep.Replay, which sits inside the ok branch and would fire for a
//     message that was never rewritten. The mutation below is what proves it.
//   - What guards the SAVING is now TestSweepReplayBooksWhatTheReplayedMessageActuallySaved: revert
//     the basis to descriptor-only and it fails, which also restores this test's original premise.
//
// The general lesson, and the reason for this comment: a measurement change can void a test that
// never mentioned the measurement. Nothing in this function referred to the basis, and correcting
// the basis silently removed its only signal.
func TestSweepRecordsNoReplayForADropItDeclined(t *testing.T) {
	// The fixture has to sit in a NARROW gap, and getting it wrong makes this test vacuous: the
	// saving is computed from the descriptor ALONE, so the descriptor must be smaller than the
	// content (or no saving would be booked either way and the mutation escapes), while
	// descriptor+marker must NOT be (or the splice succeeds and there is no declined replay).
	// Searched rather than hard-coded, because both sides depend on the tokenizer.
	content := sweepDeclineFixture(t)
	e := newSweepSmall(t, "")
	asker := &labelAsker{verdict: "keep", needed: "none"}
	asker.cacheRead = 19595
	st := store.NewMemory(store.Options{MaxEntries: 400})
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("do the thing"), toolResultMsg(content),
	}}
	c := preExpiryCtx("s", asker, st)
	// PRICED HIGH, deliberately. The saving under test is a few tokens, and at real cache-read
	// rates (~3e-7 USD/token) that is ~1e-6 USD — which round4 reports as 0.0000, so a saving
	// wrongly booked would be indistinguishable from none and this test would pass against the
	// very defect it exists to catch. A rate of 1 USD/token makes the counter observable without
	// changing which BRANCH runs: savedTokenValue reads SelfRates.CacheRead on a warm cache-aware
	// turn, which is what preExpiryCtx sets up.
	c.SelfRates = components.TokenRates{Input: 10, CacheRead: 1, CacheWrite: 12.5, Output: 50}
	// Freeze a decision whose descriptor is LONGER than the content once the marker is added, so
	// the replay path is entered and then declines.
	putResult(c, extract.ContentKey(content), sweepDescriptor(content), "")
	if _, hit := getResult(c, extract.ContentKey(content)); !hit {
		t.Fatal("the fixture's frozen decision is not readable, so the replay path is not taken")
	}
	rep := &components.Report{Component: "extract_llm_sweep"}
	before, hitsBefore := sweepValueUSD(), sweepCacheHits()
	if _, err := e.Offload(req, rep, c); err != nil {
		t.Fatalf("Offload: %v", err)
	}
	// PRECONDITION, and this test was vacuous without it: an unchanged message is ALSO what a
	// component that never ran leaves behind, so "the message is verbatim and no saving was
	// booked" proves nothing until the replay branch is known to have been entered. The cache
	// lookup is recorded on entry to that branch, before any of the checks that can decline.
	if got := sweepCacheHits() - hitsBefore; got == 0 {
		t.Fatalf("the sweep recorded no result-cache HIT, so the replay path was not entered and "+
			"the assertions below would pass on a transcript nothing touched (gates: %v)", rep.Gates)
	}
	if got := schema.MessageText(req.Input[1]); got != content {
		t.Fatalf("the fixture was spliced after all (%q), so there is no declined replay to "+
			"assert about", got)
	}
	if rep.Replays != 0 {
		t.Errorf("rep.Replay fired %d time(s) for a replay that did not splice: the message went "+
			"upstream verbatim, so no replay reached the model", rep.Replays)
	}
	// Kept, but no longer load-bearing: with the wire basis a declined replay cannot book a saving,
	// so this asserts a structural property rather than guarding a reachable defect. See the doc
	// above for which test guards the basis itself.
	if after := sweepValueUSD(); after > before {
		t.Errorf("sweep gross value rose from %v to %v for a drop that did not happen", before, after)
	}
}

// sweepDeclineFixture finds a tool output for which the sweep's shape descriptor is a real
// reduction and the descriptor PLUS the marker is not. Fails loudly rather than returning a
// best effort: a fixture that misses the gap would make the caller pass against the bug.
func sweepDeclineFixture(t *testing.T) string {
	t.Helper()
	for n := 1; n <= 400; n++ {
		content := strings.Repeat("record of the audit log\n", n)
		desc := sweepDescriptor(content)
		withMarker := desc + "\n" + expand.Marker(hashKey(content)) +
			" [full output: call " + expand.ToolName + "]"
		if schema.TextTokens(desc) < schema.TextTokens(content) &&
			schema.TextTokens(withMarker) >= schema.TextTokens(content) {
			return content
		}
	}
	t.Fatal("no content size makes the descriptor a win while the descriptor+marker is not; the " +
		"never-worse decline this test needs is unreachable, so it would pass vacuously")
	return ""
}

func sweepValueUSD() float64 {
	if s := metricsExtractByComponent("extract_llm_sweep"); s != nil {
		return s.GrossValueUSD
	}
	return 0
}

// sweepCacheHits counts result-cache HITS, not lookups. CacheLookups counts misses too, so a
// precondition built on it was satisfied by the MISS recorded further down the loop — it "proved"
// the replay branch had been entered when it had not.
func sweepCacheHits() int64 {
	if s := metricsExtractByComponent("extract_llm_sweep"); s != nil {
		return s.CallsAvoided
	}
	return 0
}

// agentdiet pays a reflection model call over a whole step, and every plan it produces wants a
// payload. If the reserve cannot take even the smallest of them, the call is paid and every plan
// is then declined at commitMark — a model call for a step guaranteed to be left verbatim.
//
// Weaker than summarize's check on purpose: it probes with the SMALLEST candidate, so it skips
// only when nothing at all can be admitted and never declines a step whose plans might still fit.
func TestAgentDietDoesNotPayForAReflectionItCannotStash(t *testing.T) {
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
	// A reserve with no room at all.
	st := store.NewMemory(store.Options{MaxEntries: 1})
	rep := &components.Report{Component: "agentdiet"}
	if _, err := d.Offload(req, rep, ctxFor(st)); err != nil {
		t.Fatalf("Offload: %v", err)
	}
	if n := atomic.LoadInt64(&model.calls); n != 0 {
		t.Errorf("the reflection model was called %d time(s) although the reserve could not "+
			"hold ANY payload: every plan it produced would be declined at commitMark, so the "+
			"call is paid for a step that is guaranteed to be left verbatim", n)
	}
	if rep.Gates["stash_reserve_exhausted"] == 0 {
		t.Errorf("the skip was not recorded as stash_reserve_exhausted, so a run that stopped "+
			"reducing looks like a run with nothing to reduce (gates: %v)", rep.Gates)
	}
}

// The sweep's debug row must carry its OWN economics, and the ledger must carry the WIRE's.
//
// Two separate numbers that a single variable used to serve, which is how one of them was lost:
//
//   - `removed_tokens` on cg.sweep.ask is what the ADJUDICATOR judged it was freeing. Moving the
//     metrics out of the verdict loop deleted that variable's only assignment, so the field went
//     permanently 0 and the sweep's economics vanished from the debug log while everything still
//     compiled and passed.
//   - the ledger's SavedTokens is what actually reached the wire. Measured against the spliced
//     message rather than the descriptor, because in markerFull the text written is
//     descriptor + marker + recovery hint — a descriptor-only subtraction overstates every
//     candidate by the marker's tokens, and the comment on that line claims it is the wire figure.
func TestSweepReportsItsOwnEconomicsAndTheWiresSeparately(t *testing.T) {
	asker := &labelAsker{verdict: "drop", needed: "none"}
	asker.cacheRead = 19595
	e := newSweepSmall(t, "")
	st := store.NewMemory(store.Options{MaxEntries: 400})
	req := sweepReqStocked()
	originals := make([]string, len(req.Input))
	for i := range req.Input {
		originals[i] = schema.MessageText(req.Input[i])
	}
	ctx, buf := debugCtx(t)
	c := preExpiryCtx("s", asker, st)
	c.Ctx = ctx
	rep := &components.Report{Component: "extract_llm_sweep"}
	if _, err := e.Offload(req, rep, c); err != nil {
		t.Fatal(err)
	}
	if rep.Events["sweep_dropped"] == 0 {
		t.Fatal("nothing was dropped, so neither figure has anything to report")
	}

	// --- The adjudicator's own figure reaches the debug row.
	rows := records(t, buf, "cg.sweep.ask")
	if len(rows) != 1 {
		t.Fatalf("expected one cg.sweep.ask record, got %d", len(rows))
	}
	removedTokens, ok := rows[0]["removed_tokens"].(float64)
	if !ok {
		t.Fatalf("cg.sweep.ask has no numeric removed_tokens field: %v", rows[0])
	}
	if removedTokens == 0 {
		t.Error("cg.sweep.ask reports removed_tokens=0 on a turn that dropped content: the " +
			"adjudicator's own economics are no longer in the debug log, which is the only place " +
			"a run's sweep decisions can be reconstructed from")
	}

	// --- The ledger's figure is the wire's, to the token.
	wantWire := 0
	for i := range req.Input {
		if got := schema.MessageText(req.Input[i]); got != originals[i] {
			wantWire += schema.TextTokens(originals[i]) - schema.TextTokens(got)
		}
	}
	if wantWire <= 0 {
		t.Fatal("no message shrank, so there is no wire figure to compare against")
	}
	var ledger int
	for _, call := range rep.Calls {
		ledger += call.SavedTokens
	}
	if ledger != wantWire {
		t.Errorf("the ledger claims %d saved tokens; the messages actually sent shrank by %d.\n"+
			"A descriptor-only subtraction ignores the marker and recovery hint that go out with "+
			"every drop, so the figure overstates by that much per candidate", ledger, wantWire)
	}
}

// A REPLAY must be valued the same way the turn that made the decision was.
//
// The fresh path was corrected to measure against the spliced message; the replay path was left
// measuring against the stored descriptor. That made the component contradict ITSELF — the same
// drop worth more on every replay turn than on the turn it was made — and replays are the steady
// state, so most of the reported value came from the overstated side.
func TestSweepReplayBooksWhatTheReplayedMessageActuallySaved(t *testing.T) {
	asker := &labelAsker{verdict: "drop", needed: "none"}
	asker.cacheRead = 19595
	e := newSweepSmall(t, "")
	st := store.NewMemory(store.Options{MaxEntries: 400})
	c := preExpiryCtx("s", asker, st)
	// CacheRead 1 USD/token, so the recorded value equals the token count and clears round4's
	// 1e-4 step. repeatPerToken is the read rate on a warm cache-aware turn, which is what the
	// replay path credits at.
	c.SelfRates = components.TokenRates{Input: 10, CacheRead: 1, CacheWrite: 12.5, Output: 50}

	// Turn 1 takes the decisions.
	var r1 components.Report
	r1.Component = "extract_llm_sweep"
	if _, err := e.Offload(sweepReqStocked(), &r1, c); err != nil {
		t.Fatal(err)
	}
	if r1.Events["sweep_dropped"] == 0 {
		t.Fatal("turn 1 dropped nothing, so turn 2 has no decision to replay")
	}

	// Turn 2 replays them, and only turn 2's booking is measured.
	req2 := sweepReqStocked()
	before := capturedText(req2)
	valueBefore := extractGrossValue("extract_llm_sweep")
	r2 := components.Report{Component: "extract_llm_sweep"}
	if _, err := e.Offload(req2, &r2, c); err != nil {
		t.Fatal(err)
	}
	if r2.Replays == 0 {
		t.Fatalf("turn 2 replayed nothing (gates: %v, events: %v)", r2.Gates, r2.Events)
	}
	wire := 0
	for i := range req2.Input {
		if got := schema.MessageText(req2.Input[i]); got != before[i] {
			wire += schema.TextTokens(before[i]) - schema.TextTokens(got)
		}
	}
	if wire <= 0 {
		t.Fatal("no message shrank on the replay turn, so there is no wire figure to compare")
	}
	booked := extractGrossValue("extract_llm_sweep") - valueBefore
	if int(booked+0.5) != wire {
		t.Errorf("the replay booked %v tokens of value; the messages it sent shrank by %d.\n"+
			"Measuring the replay against the stored descriptor instead of the replayed message "+
			"makes the same drop worth more on every replay turn than on the turn it was made — "+
			"and replays are the steady state", booked, wire)
	}
}
