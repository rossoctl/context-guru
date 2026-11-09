package offload

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/store"
)

// The evidence seam, filled: the index's record reaches the prompt AND the prompt explains how to read
// it. `main` shipped the field empty with the note that teaching a model to read counters the prompt
// never carries is teaching it to read a field that does not exist; the converse — counters with no
// explanation — invites it to invent a reading. Both halves are asserted here.
func TestSweepEvidenceReachesTheAskAndExplainsItself(t *testing.T) {
	asker := &labelAsker{verdict: "keep", needed: "a", quote: "Find the auth timeout"}
	asker.cacheRead = 19595
	e := newSweep(t, "evidence: true\n")
	req := sweepReqCoref()
	c := preExpiryCtx("s", asker, store.NewMemory(store.Options{}))

	rep := &components.Report{}
	if _, err := askOffload(e, req, rep, c); err != nil {
		t.Fatal(err)
	}
	ask := asker.ask()
	if ask == "" {
		t.Fatal("no ask was made, so there is nothing to inspect")
	}
	if !strings.Contains(ask, "evidence: ") {
		t.Errorf("the inventory carried no evidence field:\n%s", firstLines(ask, 40))
	}
	// The INDEX'S OWN NUMBERS, not just the label. On this fixture the referenced output is reused twice
	// by later turns, so a run that reached the prompt with the field present but the counters empty --
	// a rendering that dropped them, or a record silently missing -- still fails here.
	if !strings.Contains(ask, "refs=2") {
		t.Errorf("the evidence field carried no reference count from the index:\n%s", firstLines(ask, 40))
	}
	// And the orphan output's zero, which is the OTHER direction: an index that reported references for
	// everything would be as useless as one that reported none.
	if !strings.Contains(ask, "refs=0") {
		t.Errorf("no output was reported unreferenced, so the index is not discriminating:\n%s",
			firstLines(ask, 40))
	}
	// A record below the index's size floor must say so rather than emit zeros, which would read as
	// "nothing referenced it" -- the one misreading that could turn a silent index into a drop.
	if strings.Contains(ask, "novel=0 refs=0 ref_age=never used_frac=0.00 later_turns=0") &&
		!strings.Contains(ask, "no index record") {
		t.Error("an output with no index record was rendered as all-zeros instead of saying so")
	}
	if !strings.Contains(ask, `HOW TO READ THE "evidence" FIELD`) {
		t.Error("the ask carries evidence counters but never explains them")
	}
	// The index must be presented as fallible, not authoritative. Without this the mechanism collapses
	// into the pre-filter that starved three iterations: a model told the index has decided has nothing
	// left to contribute, and the veto on the index's exact-match blind spot is what it is here for.
	if !strings.Contains(ask, "WITNESS, NOT A JUDGE") {
		t.Error("the evidence paragraph does not tell the model it may overrule the index")
	}
}

// With evidence off, NEITHER the field nor its explanation appears. Guards the pairing in both
// directions — a contract that explains a field it does not carry is the seam's original hazard.
func TestSweepWithoutEvidenceCarriesNeitherFieldNorExplanation(t *testing.T) {
	asker := &labelAsker{verdict: "keep", needed: "a", quote: "Find the auth timeout"}
	asker.cacheRead = 19595
	e := newSweep(t, "evidence: false\n")
	req := sweepReqStocked()
	c := preExpiryCtx("s", asker, store.NewMemory(store.Options{}))

	rep := &components.Report{}
	if _, err := askOffload(e, req, rep, c); err != nil {
		t.Fatal(err)
	}
	ask := asker.ask()
	if strings.Contains(ask, "evidence: ") {
		t.Error("evidence is off but the inventory carried one")
	}
	if strings.Contains(ask, `HOW TO READ THE "evidence" FIELD`) {
		t.Error("evidence is off but the contract still explains the field")
	}
}

// EVIDENCE IS NOT A FILTER. The single most expensive mistake in this component's history was a
// co-reference pre-filter that left about one candidate per request, silently turning a bulk
// adjudication arm into the per-output shape refuted at 6% live-kept — while the arm reported itself as
// bulk throughout. This asserts the inventory the model is SHOWN is identical with the index on and
// off, so the index can only ever inform the comparison, never thin it.
func TestEvidenceDoesNotThinTheInventory(t *testing.T) {
	count := func(yaml string) int {
		asker := &labelAsker{verdict: "keep", needed: "a", quote: "Find the auth timeout"}
		asker.cacheRead = 19595
		e := newSweep(t, yaml)
		c := preExpiryCtx("s", asker, store.NewMemory(store.Options{}))
		rep := &components.Report{}
		if _, err := askOffload(e, sweepReqCoref(), rep, c); err != nil {
			t.Fatal(err)
		}
		return strings.Count(asker.ask(), "\n  [")
	}
	withOut, with := count(""), count("evidence: true\n")
	if withOut == 0 {
		t.Fatal("fixture offered no candidates, so this proves nothing")
	}
	if with != withOut {
		t.Fatalf("the index thinned the inventory: %d candidates with evidence, %d without. "+
			"Evidence must inform the comparison, never filter it", with, withOut)
	}
}

// sweepReqCoref is the fixture for tests whose subject is the co-reference INDEX, and it exists because
// sweepReqStocked cannot serve them: every record the index forms on that fixture is
// `novel=0 refs=0 later_turns=0`, so a filter keyed on references removes nothing there and a test
// asserting "nothing was filtered" passes without being able to fail. That vacuity was found by
// introducing the filter and watching the test stay green.
//
// So this fixture contains outputs the index can form an opinion about IN BOTH DIRECTIONS: one whose
// identifiers later turns literally reuse, and one whose identifiers are never mentioned again.
func sweepReqCoref() *bschemas.BifrostChatRequest {
	var referenced, orphan strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&referenced, "ORDER-%05d sku_%06d shipped from depot NORTH-%02d\n", 10000+i, 880000+i, i%12)
	}
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&orphan, "TRACE-%05d span_%06d took %dms in handler QUIET-%02d\n", 70000+i, 550000+i, i%97, i%12)
	}
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("Reconcile the shipped orders against the depot manifest."),
		toolResultMsgWithID("toolu_referenced", referenced.String()),
		// Later turns reuse this output's identifiers CHARACTER FOR CHARACTER, which is the only kind of
		// reuse the index can see. That is the point of the fixture, and also the point of the blind spot
		// the model is asked to cover.
		assistantMsg("ORDER-10004 and ORDER-10007 both left depot NORTH-04; sku_880004 is the mismatch."),
		toolResultMsgWithID("toolu_orphan", orphan.String()),
		assistantMsg("The manifest agrees for ORDER-10004. Next I will check sku_880007."),
		userMsg("keep going"),
		assistantMsg("Still reconciling the remaining orders."),
	}}
	for i := 0; i < fixtureInventory; i++ {
		req.Input = append(req.Input, toolResultMsgWithID("toolu_filler_"+strconv.Itoa(i),
			strings.Repeat("record "+strconv.Itoa(i)+" of the audit log\n", 900)))
	}
	req.Input = append(req.Input, assistantMsg("Summarising now."))
	return req
}

func firstLines(s string, n int) string {
	ls := strings.Split(s, "\n")
	if len(ls) > n {
		ls = ls[:n]
	}
	return strings.Join(ls, "\n")
}

// THE ASK IS A TERM IN THE DECISION, on the same fixture that authorises when the boundary is known.
//

func newMemStore() store.Store { return store.NewMemory(store.Options{}) }
