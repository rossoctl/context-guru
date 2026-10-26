package offload

import (
	"testing"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/thread"
)

// #729. After the cold apply, the entry the request writes ends at a text turn, which the provider never
// reads. The sweep names its last tool_result as the cache anchor so the host marks it.

// The cold apply names the last tool_result of the request it changed.
func TestAColdApplyNamesTheLastToolResultAsTheCacheAnchor(t *testing.T) {
	e, st, _ := heldApply(t, "anchor-cold", 3)
	c := coldCtx("anchor-cold", st)
	if _, err := e.Offload(applyConv(3, 1), &components.Report{Component: "extract_llm_sweep"}, c); err != nil {
		t.Fatal(err)
	}
	if c.CacheAnchorToolID != "toolu_wire_2" {
		t.Fatalf("CacheAnchorToolID = %q, want the last tool_result, toolu_wire_2", c.CacheAnchorToolID)
	}
}

// A request the sweep did not change gets no anchor: no held answer, and a warm request that holds one.
func TestARequestTheSweepDidNotChangeNamesNoAnchor(t *testing.T) {
	resetSweepLedgerForTest()
	t.Cleanup(resetSweepLedgerForTest)
	e := shippedSweep(t, "")
	c := coldCtx("anchor-none", newMemStore())
	if _, err := e.Offload(applyConv(3, 1), &components.Report{Component: "extract_llm_sweep"}, c); err != nil {
		t.Fatal(err)
	}
	if c.CacheAnchorToolID != "" {
		t.Fatalf("no held answer, but CacheAnchorToolID = %q", c.CacheAnchorToolID)
	}

	e2, st, _ := heldApply(t, "anchor-warm-held", 3)
	w := warmCtx("anchor-warm-held", st)
	if _, err := e2.Offload(applyConv(3, 1), &components.Report{Component: "extract_llm_sweep"}, w); err != nil {
		t.Fatal(err)
	}
	if w.CacheAnchorToolID != "" {
		t.Fatalf("a warm request left the answer held, but CacheAnchorToolID = %q", w.CacheAnchorToolID)
	}
}

// Every warm turn after the cold apply replays the drops and names the SAME anchor, so the breakpoint
// stays on the same block and the bytes stay stable.
func TestTheWarmTurnsAfterTheColdApplyKeepTheAnchorOnTheSameBlock(t *testing.T) {
	e, st, _ := heldApply(t, "anchor-replay", 3)
	if _, err := e.Offload(applyConv(3, 1), &components.Report{Component: "extract_llm_sweep"}, coldCtx("anchor-replay", st)); err != nil {
		t.Fatal(err)
	}
	for n := 2; n <= 4; n++ {
		next := applyConv(3, n)
		// The agent adds a new tool exchange after the anchor: the anchor must not move to it.
		next.Input = append(next.Input, toolResultMsgWithID("toolu_new", "fresh output"))
		c := warmCtx("anchor-replay", st)
		if _, err := e.Offload(next, &components.Report{Component: "extract_llm_sweep"}, c); err != nil {
			t.Fatal(err)
		}
		if c.CacheAnchorToolID != "toolu_wire_2" {
			t.Fatalf("warm turn %d: CacheAnchorToolID = %q, want toolu_wire_2 (unmoved)", n, c.CacheAnchorToolID)
		}
	}
}

// A summarizer splice that named its own anchor keeps it.
func TestTheSweepDoesNotOverwriteASplicesAnchor(t *testing.T) {
	e, st, _ := heldApply(t, "anchor-splice", 3)
	c := coldCtx("anchor-splice", st)
	c.CacheAnchorToolID = "toolu_splice"
	if _, err := e.Offload(applyConv(3, 1), &components.Report{Component: "extract_llm_sweep"}, c); err != nil {
		t.Fatal(err)
	}
	if c.CacheAnchorToolID != "toolu_splice" {
		t.Fatalf("CacheAnchorToolID = %q, want the splice's toolu_splice", c.CacheAnchorToolID)
	}
}

// The anchor record is metadata only: an id.
func TestTheAnchorRecordIsAToolIDOnly(t *testing.T) {
	e, st, _ := heldApply(t, "anchor-meta", 3)
	if _, err := e.Offload(applyConv(3, 1), &components.Report{Component: "extract_llm_sweep"}, coldCtx("anchor-meta", st)); err != nil {
		t.Fatal(err)
	}
	if got := getDropIndex(st, thread.Key("anchor-meta", "")).Anchor; got != "toolu_wire_2" {
		t.Fatalf("stored anchor = %q, want toolu_wire_2", got)
	}
}

// A thread that has an anchor, on a request the sweep changed nothing in (none of its drops are in
// that conversation), names no anchor.
func TestAnAnchoredThreadNamesNoAnchorOnARequestTheSweepLeftAlone(t *testing.T) {
	e, st, _ := heldApply(t, "anchor-unchanged", 3)
	if _, err := e.Offload(applyConv(3, 1), &components.Report{Component: "extract_llm_sweep"}, coldCtx("anchor-unchanged", st)); err != nil {
		t.Fatal(err)
	}
	other := deepCandidates(0) // a conversation that holds none of the dropped outputs
	other.Input = append(other.Input, toolResultMsgWithID("toolu_other", "short"))
	c := warmCtx("anchor-unchanged", st)
	if _, err := e.Offload(other, &components.Report{Component: "extract_llm_sweep"}, c); err != nil {
		t.Fatal(err)
	}
	if c.CacheAnchorToolID != "" {
		t.Fatalf("the sweep changed nothing, but CacheAnchorToolID = %q", c.CacheAnchorToolID)
	}
}
