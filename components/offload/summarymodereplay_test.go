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

// THE DEFECT
//
// Under marker_mode summary/off an offloader takes a deliberate LOSSY drop: nothing is stashed and
// no <<cg:HASH>> is written, so the component returns no cache keys. components/pipeline.go treats
// that combination — the request shrank, no cache keys, not Skipped — as a contract violation and
// REVERTS the component, unless rep.Irreversible says the loss was chosen.
//
// commitMark's non-full branch sets that flag, so the turn that TAKES the decision is fine. Every
// later turn replays the frozen decision through reapplyFrozen, which never set it. So from turn 2
// onward, for the whole session:
//
//	turn 1: keys=[] Irreversible=true  -> kept
//	turn 2: keys=[] Irreversible=false -> REVERTED, transcript sent verbatim
//
// A revert is not merely a lost saving. Earlier turns sent the reduced bytes, so sending the
// original re-writes the provider's whole cached suffix at ~11.5x the read price — and it happens
// on every turn, for every message the component had reduced. The component reports itself as
// working: it acts, it computes a replacement, and the pipeline throws it away afterwards.
//
// Found while auditing replay paths for a related review; the defect is independent of that work.
//
// NOT A SUBSTITUTE for the `mask via reapplyFrozen` subtest of
// TestADegradedModeReplayDeclaresItselfIrreversible, and do not delete that one as redundant to
// this. The two have different SUBJECTS that happen to share an assertion: this asserts a property
// of reapplyFrozen, while the subtest asserts that all four replay branches are held to the same
// assertion through the same helper — uniformity, not the property.
//
// They fail differently, which is the test of whether both are needed. Special-case the helper so it
// suits extract_llm and extract_sweep_drop but not reapplyFrozen and the table catches it while this
// test stays green; break reapplyFrozen itself and both fire. Dropping the subtest loses the first
// case entirely, and that is the case that matters for a helper three other branches depend on.
func TestASummaryModeReplayIsNotRevertedFromTurnTwoOnward(t *testing.T) {
	body := strings.Repeat("a line of log output that goes on for a while\n", 60)
	st := store.NewMemory(store.Options{})
	c, err := newMask([]byte("keep_recent: 0\nmin_tokens: 20\nmarker_mode: summary\n"))
	if err != nil {
		t.Fatal(err)
	}
	comp := c.(components.Offload)
	ctx := &components.Ctx{Ctx: context.Background(), Session: "s", Store: st, MaxCachedIdx: -1}
	req := func() *bschemas.BifrostChatRequest {
		m := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleTool}
		schema.SetMessageText(&m, body)
		return &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{m}}
	}

	// Turn 1 takes the decision. This turn was never broken — commitMark sets the flag — and it is
	// asserted so a fixture that stopped reaching summary mode fails here rather than passing below.
	r1 := components.Report{Kind: "offload"}
	if _, err := comp.Offload(req(), &r1, ctx); err != nil {
		t.Fatal(err)
	}
	if !r1.Irreversible || len(r1.CacheKeys) != 0 {
		t.Fatalf("turn 1: Irreversible=%v keys=%v, want true/none — the fixture is not in a "+
			"degraded marker mode, so turn 2 is not the case under test",
			r1.Irreversible, r1.CacheKeys)
	}

	// Turn 2 replays it, through reapplyFrozen.
	req2 := req()
	r2 := components.Report{Kind: "offload"}
	if _, err := comp.Offload(req2, &r2, ctx); err != nil {
		t.Fatal(err)
	}
	if got := schema.MessageText(req2.Input[0]); got == body {
		t.Fatalf("turn 2 did not replay the frozen decision, so the revert precondition is not " +
			"reachable and this assertion would pass vacuously")
	}
	if r2.Skipped {
		t.Fatal("turn 2 reported Skipped, so the pipeline would not revert it and this test " +
			"proves nothing")
	}
	if len(r2.CacheKeys) != 0 {
		t.Fatalf("turn 2 returned cache keys (%v); in summary mode nothing is stashed, so the "+
			"fixture is not exercising the degraded path", r2.CacheKeys)
	}
	if !r2.Irreversible {
		t.Error("a replayed summary-mode decision rewrote the message, returned NO cache key and " +
			"did not set rep.Irreversible. That is exactly what components/pipeline.go reverts as " +
			"\"offload dropped content without stashing a cache_key\", so from turn 2 onward the " +
			"whole component is discarded and the transcript goes upstream verbatim — flipping " +
			"content earlier turns had already sent reduced, at ~11.5x the read price, every turn")
	}
}
