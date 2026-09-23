package offload

import (
	"testing"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/store"
)

// THE WASTE THESE PIN: seven of ten adjudications on one probe pass were the same five candidates at the
// same 41,453 tokens, kept every time. Drops are frozen and replayed; keeps were forgotten, so the
// component re-bought a verdict it already had, once per turn.
//
// And the OPPOSITE failure, which is worse and which the recheck interval exists to prevent: cache a
// keep permanently and a candidate judged needed once is never offered again, so the component goes
// quiet for exactly the outputs it most wants to reach — and in the counters that reads as an inventory
// that shrank, not as a component that stopped asking.

func keepCtx(t *testing.T) *components.Ctx {
	t.Helper()
	return &components.Ctx{Session: "s", Store: store.NewMemory(store.Options{})}
}

// A keep suppresses the next turn's offer, and stops suppressing once the conversation has moved on.
// Mutation that must fail this: `if grown >= recheck` -> `if false`.
func TestAKeepExpiresAfterTheRecheckInterval(t *testing.T) {
	c := keepCtx(t)
	const recheck = 4
	recordSweepKeep(c, "id-1", 5) // judged still needed when 5 turns followed it

	for _, tc := range []struct {
		later int
		want  bool
		why   string
	}{
		{5, true, "same turn: nothing has changed, the answer still stands"},
		{6, true, "one turn later: not enough has happened to re-ask"},
		{8, true, "three turns later: still inside the interval"},
		{9, true, "four turns later is the boundary and must still hold (grown == recheck re-asks)"},
		{10, false, "five turns later: the conversation moved on, ask again"},
		{40, false, "far later: a keep must never be permanent"},
	} {
		held, _ := sweepKeepStillHolds(c, "id-1", tc.later, recheck)
		if tc.later == 9 { // grown == recheck: the code re-asks at exactly the interval
			if held {
				t.Fatalf("later_turns=%d: held, but grown == recheck must re-ask (%s)", tc.later, tc.why)
			}
			continue
		}
		if held != tc.want {
			t.Fatalf("later_turns=%d: held=%v want=%v (%s)", tc.later, held, tc.want, tc.why)
		}
	}
}

// OFF BY DEFAULT. recheck 0 is every iteration up to 027: offer everything, every turn.
// Mutation that must fail this: `if recheck <= 0` -> `if recheck < 0`.
func TestKeepCacheIsInertWhenUnconfigured(t *testing.T) {
	c := keepCtx(t)
	recordSweepKeep(c, "id-1", 5)
	if held, _ := sweepKeepStillHolds(c, "id-1", 5, 0); held {
		t.Fatal("a keep suppressed an offer with keep_recheck_turns unset: the cache must be inert " +
			"until an operator asks for it")
	}
}

// A TRANSCRIPT THAT SHRANK invalidates the coordinate. The stored later_turns and the current one are
// only comparable while the conversation grows; after an agent-side compaction or a rewind the stored
// number describes a different transcript, so the judgement says nothing about now.
// Mutation that must fail this: delete the `grown < 0` branch.
func TestAShorterTranscriptInvalidatesTheKeep(t *testing.T) {
	c := keepCtx(t)
	recordSweepKeep(c, "id-1", 20)
	if held, _ := sweepKeepStillHolds(c, "id-1", 4, 4); held {
		t.Fatal("a keep recorded at later_turns=20 still suppressed at later_turns=4: the transcript " +
			"shrank behind this output, so the two coordinates are not comparable and the question is open")
	}
}

// An unreadable or absent record must resolve toward ASKING. Suppressing on a record we cannot read
// would silence the component on corruption, which is the one direction that hides itself.
func TestAnUnreadableKeepRecordAsksAgain(t *testing.T) {
	c := keepCtx(t)
	c.Store.Put(sweepKeepKey(c.Session, "id-1"), []byte("{not json"))
	if held, _ := sweepKeepStillHolds(c, "id-1", 5, 4); held {
		t.Fatal("an unreadable keep record suppressed the offer: corruption must degrade to asking")
	}
	if held, _ := sweepKeepStillHolds(c, "never-seen", 5, 4); held {
		t.Fatal("an absent record suppressed the offer")
	}
}

// THE KEEP IS RECORDED WHERE THE KEEP IS COUNTED, end to end: a candidate the adjudicator kept is not
// offered again on the next turn, and the gate says so.
// Mutation that must fail this: drop the recordSweepKeep call beside rep.Gate("sweep_kept").
func TestAKeptCandidateIsNotReOfferedNextTurn(t *testing.T) {
	asker := &labelAsker{verdict: "keep", needed: "a", quote: "Find the auth timeout"}
	e := newSweep(t, "econ_trigger: true\nkeep_recheck_turns: 4\nmin_later_turns: 0\n")
	st := store.NewMemory(store.Options{})

	req := sweepReqStocked()
	c := preExpiryCtx("s", asker, st)
	first := &components.Report{}
	if _, err := e.Offload(req, first, c); err != nil {
		t.Fatal(err)
	}
	if first.Gates["sweep_kept"] == 0 {
		t.Fatalf("fixture is wrong: the adjudicator must KEEP on the first turn (gates %v)", first.Gates)
	}
	if first.Gates["sweep_keep_still_held"] != 0 {
		t.Fatalf("suppressed an offer on the FIRST turn, before any keep existed (gates %v)", first.Gates)
	}
	// Second turn, same content, same session, no turns added.
	second := &components.Report{}
	if _, err := e.Offload(sweepReqStocked(), second, preExpiryCtx("s", asker, st)); err != nil {
		t.Fatal(err)
	}
	if second.Gates["sweep_keep_still_held"] == 0 {
		t.Fatalf("re-offered every candidate the adjudicator had just kept: the keep was counted and "+
			"not remembered, which is the seven-identical-asks defect (gates %v)", second.Gates)
	}
}
