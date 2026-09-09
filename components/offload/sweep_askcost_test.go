package offload

import (
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// THE DEFECT THESE EXIST TO CATCH, in one line: the econ trigger authorised nine adjudications in nine
// on the iteration 025 pre-flight, six of which removed nothing, because its break-even charged the
// cache-write and never the model call that decides what to remove.
//
// Every test below is written to fail against the arithmetic as it was, and each was checked by
// reverting the change rather than by reading it.

// econCtx is a request-shaped context with a known window and the deployment's own rates left unset, so
// agentRates falls back to the sonnet-class constants beside it. MaxCachedIdx -1 with CacheAware means
// "cache-aware, boundary unknown", which is the case the two window conventions disagree about.
func econCtx(window int) *components.Ctx {
	return &components.Ctx{CtxWindow: window, CacheAware: true, MaxCachedIdx: -1}
}

// bigReq builds a transcript of n messages of roughly `each` tokens, so a fixture can be large enough
// for a charged ask to genuinely pay for itself. Token counts here are schema.TextTokens' estimate of
// the string, not a literal, which is what every other econ fixture relies on too.
func bigReq(n, each int) *bschemas.BifrostChatRequest {
	req := &bschemas.BifrostChatRequest{}
	for i := 0; i < n; i++ {
		body := strings.Repeat("line "+strconv.Itoa(i)+" of the transcript\n", each)
		if i%2 == 1 {
			req.Input = append(req.Input, assistantMsg(body))
			continue
		}
		req.Input = append(req.Input, toolResultMsg(body))
	}
	return req
}

// COREF'S PATH IS UNTOUCHED, term for term. prefixRewritePays is shared with a component that decides
// with a deterministic index and pays for no call, so the refactor must be exactly a no-op there.
// Asserted against an independent recomputation of the ORIGINAL expression rather than against the new
// one, because comparing the new code to itself would pass however wrong it is.
func TestPrefixRewritePaysUnchangedWithoutAnAsk(t *testing.T) {
	req := bigReq(24, 400)
	for _, tc := range []struct {
		name              string
		window            int
		saved, shallowest int
	}{
		{"deep drop, big window", 200_000, 800, 2},
		{"late drop", 200_000, 800, 20},
		{"no turns left", 900, 800, 2},
		{"whole span removed", 200_000, 1 << 20, 0},
		{"nothing saved", 200_000, 0, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := econCtx(tc.window)
			need, have, ok := prefixRewritePays(req, tc.saved, tc.shallowest, c)
			wNeed, wHave, wOK := originalPrefixRewritePays(req, tc.saved, tc.shallowest, c)
			if need != wNeed || have != wHave || ok != wOK {
				t.Fatalf("coref's break-even moved: got (need=%d have=%d ok=%v), "+
					"original arithmetic says (need=%d have=%d ok=%v)", need, have, ok, wNeed, wHave, wOK)
			}
		})
	}
}

// THE BRANCH THAT DID THE DAMAGE. When every candidate sits at or past the cached boundary the prefix
// needs no rewriting, and the old code returned "authorised" unconditionally from there. That is
// precisely the branch on which the cache-write is free and the ask is therefore the ONLY cost, so it
// is the branch where an unconditional yes is least defensible. It fired on 5 of the pre-flight's 9.
func TestFreeRewriteStillHasToRepayTheAsk(t *testing.T) {
	req := bigReq(24, 400)
	c := econCtx(200_000)
	// The LAST message, removed entirely: the rewritten span is that one message, so `rewritten`
	// lands on zero and this is the free branch. Deliberately a MODEST saving — an inventory of a
	// million tokens would repay a $5 ask on its own and prove nothing about the branch.
	last := len(req.Input) - 1
	saved := schema.TextTokens(schema.MessageText(req.Input[last]))

	if _, _, ok := prefixRewritePays(req, saved, last, c); !ok {
		t.Fatal("fixture is wrong: this must be the free-rewrite branch, which authorises with no ask")
	}
	// The same batch, with an ask that costs real money, must not be waved through.
	need, have, ok := prefixRewritePaysCharging(req, saved, last, 5.00, 1, c)
	if ok {
		t.Errorf("a $5.00 adjudication was authorised because the REWRITE was free "+
			"(need=%d have=%d) — the ask was never a term in the decision", need, have)
	}
	if need == 0 {
		t.Errorf("need=0 for a priced ask: the free-rewrite early return is still short-circuiting")
	}
}

// The second half of the correction: `saved` is what MIGHT be dropped, and the model takes a subset.
// Discounting it has to scale the requirement, or the estimate is decorative.
func TestApprovalDiscountScalesTheRequirement(t *testing.T) {
	req := bigReq(40, 400)
	c := econCtx(200_000)
	full, _, _ := prefixRewritePaysCharging(req, 4_000, 2, 0.10, 1, c)
	third, _, _ := prefixRewritePaysCharging(req, 4_000, 2, 0.10, 1.0/3.0, c)
	if full == 0 {
		t.Fatal("fixture is wrong: need is 0 even undiscounted, so the discount cannot show")
	}
	if third < full*2 {
		t.Errorf("approval 1/3 moved need from %d to %d; expecting roughly 3x, so the discount "+
			"is not reaching the denominator", full, third)
	}
}

// A measured approval of zero must not let the component disable itself forever: declining every ask
// destroys the only source of evidence that could revise the estimate, and there is no path back.
func TestApprovalFloorPreventsPermanentSelfDisable(t *testing.T) {
	l := &askLedger{}
	for i := 0; i < minAskSamples+2; i++ {
		l.record(0.05, 10_000, 0) // asked, paid, removed nothing
	}
	cost, approval, measured := l.estimate(0)
	if !measured {
		t.Fatalf("%d asks recorded and the ledger still reports the prior", minAskSamples+2)
	}
	if approval <= 0 {
		t.Fatalf("approval %v: a zero estimate declines every future ask and can never be revised",
			approval)
	}
	if approval != approvalFloor {
		t.Errorf("approval = %v, want the floor %v", approval, approvalFloor)
	}
	if cost <= 0 {
		t.Errorf("cost = %v after recording $0.05 asks", cost)
	}
}

// The ledger has to LEARN, and has to say when it is still guessing — an estimate that silently stays
// at its prior is the shape that let min_inventory block everything downstream in silence.
func TestAskLedgerLearnsCostAndApprovalAfterWarmUp(t *testing.T) {
	l := &askLedger{}
	const prior = 0.123
	if cost, approval, measured := l.estimate(prior); measured || cost != prior || approval != 1 {
		t.Fatalf("a fresh ledger must report the prior and say so: cost=%v approval=%v measured=%v",
			cost, approval, measured)
	}
	l.record(0.04, 1_000, 250)
	if _, _, measured := l.estimate(prior); measured {
		t.Errorf("one ask is not a rate: the ledger trusted itself after a single sample")
	}
	for i := 0; i < minAskSamples; i++ {
		l.record(0.04, 1_000, 250)
	}
	cost, approval, measured := l.estimate(prior)
	if !measured {
		t.Fatalf("still on the prior after %d asks", minAskSamples+1)
	}
	if cost < 0.039 || cost > 0.041 {
		t.Errorf("cost = %v, want ~0.04 (mean of identical asks)", cost)
	}
	if approval < 0.24 || approval > 0.26 {
		t.Errorf("approval = %v, want ~0.25 (250 of 1,000 offered)", approval)
	}
}

// An ask with no offered mass is not evidence about a RATE, and folding it in would drag the
// denominator without adding information.
func TestAskLedgerIgnoresAnAskThatOfferedNothing(t *testing.T) {
	l := &askLedger{}
	for i := 0; i < minAskSamples+1; i++ {
		l.record(0.04, 0, 0)
	}
	if _, _, measured := l.estimate(0.5); measured {
		t.Error("empty asks were counted as samples, so the ledger now reports a rate over no mass")
	}
}

// THE TWO WINDOW CONVENTIONS LEAN OPPOSITE WAYS ON PURPOSE, and this is the one that is easy to get
// wrong by reaching for the helper that already exists. prefixRewriteWindow treats an unknown boundary
// as "all cached", which over-states a REWRITE and is therefore safe. The identical assumption
// under-states an ASK — everything looks like a cheap cache read — which is the direction that
// authorises. So askCostPrior must resolve the unknown case the other way.
func TestAskCostPriorTreatsAnUnknownBoundaryAsUncached(t *testing.T) {
	req := bigReq(24, 400)
	unknown := askCostPrior(req, econCtx(200_000))

	known := econCtx(200_000)
	known.MaxCachedIdx = len(req.Input) - 1 // the provider holds all of it
	cached := askCostPrior(req, known)

	if unknown <= cached {
		t.Errorf("unknown boundary priced at %v, fully-cached at %v: an unknown cache is being "+
			"treated as a cheap read, which is the direction that authorises the ask", unknown, cached)
	}
	// And the fresh side has to dominate, which is what the measured pre-flight split says: 28,594
	// fresh against 52,074 cached was $0.057 against $0.010.
	if unknown < cached*2 {
		t.Errorf("fresh tokens priced at only %v against %v cached: the split at the boundary is "+
			"not the term that matters here", unknown, cached)
	}
}

// originalPrefixRewritePays is the expression as it stood before the ask became a term, kept as an
// INDEPENDENT reference for the coref-unchanged test above. Deliberately a transcription rather than a
// call into the new code.
func originalPrefixRewritePays(req *bschemas.BifrostChatRequest, saved, shallowest int,
	c *components.Ctx) (need, have int, ok bool) {

	if c == nil || c.CtxWindow <= 0 {
		return 0, 0, true
	}
	if saved <= 0 {
		return 0, 0, false
	}
	end := prefixRewriteWindow(req, c)
	rewritten := 0
	for j := shallowest; j <= end && j < len(req.Input); j++ {
		rewritten += schema.TextTokens(schema.MessageText(req.Input[j]))
	}
	rewritten -= saved
	turns := estimateTurnsRemaining(schema.MessagesTokens(req), modelTurns(req), c.CtxWindow)
	if rewritten <= 0 {
		return 0, turns, true
	}
	need = int(math.Ceil(cacheWriteX * float64(rewritten) / float64(saved)))
	return need, turns, need <= turns
}

// PRESSURE IS RECORDED ON A DECLINE, which is the case that has no other record. A fired ask leaves a
// cg.sweep.ask row carrying req_tokens; a declined one leaves nothing, so if the econ decision does not
// carry pressure itself then "did it fire too early" is unanswerable for exactly the decisions that
// did not fire.
func TestEconDecisionCarriesWhereInTheWindowItWasTaken(t *testing.T) {
	e := newSweep(t, "econ_trigger: true\n")
	req := bigReq(24, 400)
	total := schema.MessagesTokens(req)
	c := econCtx(total * 4) // a quarter of the way through the window

	cands := candsFor(req, 3, 5)
	d := e.econPays(req, c, cands)
	if d.reqTokens != total {
		t.Errorf("reqTokens = %d, want %d", d.reqTokens, total)
	}
	if d.pressure < 0.2 || d.pressure > 0.3 {
		t.Errorf("pressure = %v, want ~0.25 (%d tokens of a %d window)",
			d.pressure, total, c.CtxWindow)
	}
	// And an unknown window must not produce a division artefact that reads as "empty context".
	if _, p := sweepPressure(req, econCtx(0)); p != 0 {
		t.Errorf("pressure = %v with no window; an unknown window must report 0, not a ratio", p)
	}
}

// THE OPPORTUNITY FLOOR, and the specific defect it closes: the econ trigger fires most eagerly exactly
// where its question is least answerable.
//
// Measured on the iteration 026 probe, the first ask of every pass landed on a 2,621-token transcript —
// 88% of it candidate output, nothing cached to rewrite, 70 projected turns to collect over — clearing
// the break-even by 3.3x at 4% context pressure, and removing nothing, correctly, because nothing had
// been superseded. Those are the ask ledger's WARM-UP samples, so the estimator calibrates on the one
// moment its question cannot be answered, and a floored approval then suppresses the asks that would
// correct it.
//
// Asserted as a PAIR, because either half alone passes against the wrong code: the floor must exclude
// an output that is too new, and must NOT exclude the same output once turns have accumulated after it.
// A gate that simply refuses everything would satisfy the first assertion.
func TestOpportunityFloorRefusesOutputsTooNewToJudge(t *testing.T) {
	asker := &labelAsker{verdict: "drop", needed: "none"}
	asker.cacheRead = 19595

	// The fixture's candidates sit near the END of the transcript, so few model turns follow them.
	fresh := sweepReqStocked()
	// ...and the same fixture with assistant turns appended, giving those candidates their opportunity.
	aged := sweepReqStocked()
	for i := 0; i < 12; i++ {
		aged.Input = append(aged.Input, assistantMsg("step "+strconv.Itoa(i)+": still working"))
	}

	for _, tc := range []struct {
		name    string
		req     *bschemas.BifrostChatRequest
		wantAsk bool
	}{
		{"too new — the ask must not happen", fresh, false},
		{"aged — the same candidates must be offered", aged, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asker.calls = 0
			e := newSweep(t, "econ_trigger: true\nmin_later_turns: 8\n")
			c := preExpiryCtx("s"+tc.name, asker, store.NewMemory(store.Options{}))
			c.IdleMs = 30 * 1000
			c.MaxCachedIdx = len(tc.req.Input) - 2

			rep := &components.Report{}
			if _, err := e.Offload(tc.req, rep, c); err != nil {
				t.Fatal(err)
			}
			asked := atomic.LoadInt64(&asker.calls) > 0
			if asked != tc.wantAsk {
				t.Fatalf("asked=%v want=%v — gates: %v", asked, tc.wantAsk, rep.Gates)
			}
			if !tc.wantAsk && rep.Gates["sweep_candidate_too_new"] == 0 {
				t.Errorf("declined without recording WHY the candidates were unusable: %v", rep.Gates)
			}
			if tc.wantAsk && rep.Gates["sweep_candidate_too_new"] != 0 {
				t.Errorf("aged candidates were still refused as too new: %v", rep.Gates)
			}
		})
	}
}

// The floor is OFF unless asked for. Without this, "we added an opportunity floor" and "we changed every
// deployment running the sweep" are the same commit.
func TestOpportunityFloorIsOffByDefault(t *testing.T) {
	asker := &labelAsker{verdict: "drop", needed: "none"}
	asker.cacheRead = 19595
	e := newSweep(t, "econ_trigger: true\n") // no min_later_turns
	req := sweepReqStocked()
	c := preExpiryCtx("s", asker, store.NewMemory(store.Options{}))
	c.IdleMs = 30 * 1000
	c.MaxCachedIdx = len(req.Input) - 2

	rep := &components.Report{}
	if _, err := e.Offload(req, rep, c); err != nil {
		t.Fatal(err)
	}
	if rep.Gates["sweep_candidate_too_new"] != 0 {
		t.Errorf("the floor fired without being configured: %v", rep.Gates)
	}
}

// THE REQUEST-LEVEL FLOOR, and the invariant that makes it safe: it suppresses COLLECTION, never the
// frozen replays. A floor that returned early would undo savings already earned — a later turn would
// re-send every removed output verbatim, breaking both the saving and the prefix's byte-stability.
//
// Three assertions, because each catches a different wrong implementation: below the floor there is no
// ask and the reason is recorded; above it the same fixture asks normally; and a floor that fires must
// not stop the component reporting on the pre-expiry trigger it still evaluated.
func TestRequestPressureFloorSuppressesCollectionNotReplays(t *testing.T) {
	asker := &labelAsker{verdict: "drop", needed: "none"}
	asker.cacheRead = 19595
	// A FRESH REQUEST PER SUBTEST. Sharing one is not safe here: Offload splices markers into
	// req.Input, so a later subtest sees every candidate as already-marked and skips it
	// (`empty_or_marker_present`), which reads as a floor that fired when it did not.

	// ONLY THE FLOOR VARIES. The window stays at preExpiryCtx's 1,000,000, where this fixture sits at
	// about 7% pressure and the econ trigger is already known to fire (see
	// TestSweepEconTriggerFiresOutsideTheWindow). Moving the window instead would change `have` as well
	// — a first attempt set it to 2x the request, which put pressure above the floor and left roughly
	// one turn of headroom, so the ECON gate declined and the test read as a floor that fired when it
	// had not.
	for _, tc := range []struct {
		name    string
		floor   string
		wantAsk bool
	}{
		{"below the floor", "min_pressure: 0.10\n", false},
		{"above the floor", "min_pressure: 0.01\n", true},
		{"floor unset", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asker.calls = 0
			req := sweepReqStocked()
			e := newSweep(t, "econ_trigger: true\n"+tc.floor)
			c := preExpiryCtx("p"+tc.name, asker, store.NewMemory(store.Options{}))
			c.IdleMs = 30 * 1000
			c.MaxCachedIdx = len(req.Input) - 2

			rep := &components.Report{}
			if _, err := e.Offload(req, rep, c); err != nil {
				t.Fatal(err)
			}
			if asked := atomic.LoadInt64(&asker.calls) > 0; asked != tc.wantAsk {
				t.Fatalf("asked=%v want=%v (gates: %v)", asked, tc.wantAsk, rep.Gates)
			}
			if !tc.wantAsk && rep.Gates["sweep_below_min_pressure"] == 0 {
				t.Errorf("suppressed without recording why (gates: %v)", rep.Gates)
			}
			if tc.wantAsk && rep.Gates["sweep_below_min_pressure"] != 0 {
				t.Errorf("floor fired when it should not have (gates: %v)", rep.Gates)
			}
			// The trigger-one counter is raised BEFORE the floor is consulted, so a fired floor must
			// not swallow it — otherwise a run cannot tell "the floor stopped us" from "the component
			// never woke up".
			if rep.Gates["not_in_pre_expiry_window"] == 0 {
				t.Errorf("the pre-expiry decision went unrecorded (gates: %v)", rep.Gates)
			}
		})
	}
}

// THE INVARIANT THE PRESSURE FLOOR MUST NOT BREAK: a frozen removal is still replayed on a turn the
// floor suppresses.
//
// This is why the floor sets a flag instead of returning. Without the replay, a later turn re-sends the
// removed output verbatim — undoing a saving already paid for AND breaking the byte-stability of the
// prefix the provider is caching, which costs a full cache-write. The comment at the floor asserts this;
// this test is what makes the assertion checkable, and a `return` in place of the flag fails it.
//
// Two Offload calls share one session and one store: the first removes something with the floor off, the
// second runs with the floor firing and must still show the marker rather than the original bytes.
func TestPressureFloorStillReplaysFrozenRemovals(t *testing.T) {
	st := store.NewMemory(store.Options{})
	asker := &labelAsker{verdict: "drop", needed: "none"}
	asker.cacheRead = 19595

	// Turn one: floor off, so the sweep acts and freezes its decision.
	first := sweepReqStocked()
	e1 := newSweep(t, "econ_trigger: true\n")
	c1 := preExpiryCtx("replay-sess", asker, st)
	c1.IdleMs = 30 * 1000
	c1.MaxCachedIdx = len(first.Input) - 2
	rep1 := &components.Report{}
	if _, err := e1.Offload(first, rep1, c1); err != nil {
		t.Fatal(err)
	}
	var markedIdx = -1
	for i := range first.Input {
		if strings.Contains(schema.MessageText(first.Input[i]), "cg:") {
			markedIdx = i
			break
		}
	}
	if markedIdx < 0 {
		t.Skip("turn one removed nothing on this fixture, so there is no frozen decision to replay")
	}

	// Turn two: same session and store, floor firing. The frozen removal must still be applied.
	second := sweepReqStocked()
	original := schema.MessageText(second.Input[markedIdx])
	e2 := newSweep(t, "econ_trigger: true\nmin_pressure: 0.99\n")
	c2 := preExpiryCtx("replay-sess", asker, st)
	c2.IdleMs = 30 * 1000
	c2.MaxCachedIdx = len(second.Input) - 2
	rep2 := &components.Report{}
	if _, err := e2.Offload(second, rep2, c2); err != nil {
		t.Fatal(err)
	}
	if rep2.Gates["sweep_below_min_pressure"] == 0 {
		t.Fatalf("fixture is wrong: the floor did not fire, so this proves nothing (gates: %v)",
			rep2.Gates)
	}
	got := schema.MessageText(second.Input[markedIdx])
	if got == original {
		t.Errorf("the frozen removal was NOT replayed on a floor-suppressed turn: message %d came "+
			"back verbatim, so a saving already paid for was undone and the cached prefix broken",
			markedIdx)
	}
	if !strings.Contains(got, "cg:") {
		t.Errorf("message %d carries no marker after replay: %q", markedIdx, firstLines(got, 2))
	}
}
