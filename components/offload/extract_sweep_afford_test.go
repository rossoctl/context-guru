package offload

import (
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
)

// DEPTH, NOT SIZE, is what makes a drop expensive, and these are the tests that have to be able to tell
// the difference. A drop's cost is the cache-WRITE over the span from the earliest dropped index to the
// cached boundary, charged once per pass -- so a small output dropped after something already being
// dropped is nearly free, while the same output dropped EARLIER than everything else sets W for the whole
// batch. A rule keyed on size cannot express that; this one is keyed on the walk.

// corefless helper: a transcript of n tool messages of the given per-message token weight, so index and
// mass can be varied independently.
func affordReq(n int, tokensEach int) *bschemas.BifrostChatRequest {
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("Reconcile the ledger."),
	}}
	for i := 0; i < n; i++ {
		req.Input = append(req.Input, toolResultMsgWithID(
			"toolu_a"+string(rune('a'+i)), padTokens(tokensEach)))
		req.Input = append(req.Input, assistantMsg("noted"))
	}
	return req
}

func padTokens(tok int) string {
	// ~1 token per 4 chars for this tokenizer's purposes; the tests compare relative magnitudes only.
	s := make([]byte, tok*4)
	for i := range s {
		s[i] = 'x'
		if i%40 == 39 {
			s[i] = '\n'
		}
	}
	return string(s)
}

func candsFor(req *bschemas.BifrostChatRequest, idx ...int) []sweepCand {
	var out []sweepCand
	for _, i := range idx {
		out = append(out, sweepCand{i: i, content: messageTextOf(req, i)})
	}
	return out
}

func messageTextOf(req *bschemas.BifrostChatRequest, i int) string {
	return schemaText(req, i)
}

// A drop set confined to the LATE part of the cached region is fully affordable and must survive intact.
func TestAffordableDropsKeepsALateBatchWhole(t *testing.T) {
	e := newSweep(t, "econ_trigger: true\n")
	req := affordReq(8, 2000)
	c := &components.Ctx{CtxWindow: 1_000_000, CacheAware: true, MaxCachedIdx: -1}
	cands := candsFor(req, 13, 15)
	kept, pruned := e.selectAffordableDrops(req, c, cands, []int{0, 1})
	if pruned != 0 || len(kept) != 2 {
		t.Fatalf("a late, cheap batch was pruned: kept=%d pruned=%d", len(kept), pruned)
	}
}

// The discriminating case: one TINY drop far EARLIER than the rest. It contributes almost no saving but
// extends the rewrite span across the whole transcript, so it must be pruned while the rest survive.
func TestAffordableDropsPrunesATinyEarlyDropThatSetsTheRewriteSpan(t *testing.T) {
	e := newSweep(t, "econ_trigger: true\n")
	req := affordReq(10, 3000)
	// Make message 1 tiny: it is the earliest, so including it rewrites everything after it.
	setMessageTextAt(req, 1, padTokens(30))
	c := &components.Ctx{CtxWindow: 40_000, CacheAware: true, MaxCachedIdx: -1}
	cands := candsFor(req, 1, 17, 19)
	kept, pruned := e.selectAffordableDrops(req, c, cands, []int{0, 1, 2})
	if pruned == 0 {
		t.Fatalf("the tiny early drop was applied; it sets W for the batch and cannot repay it "+
			"(kept=%d)", len(kept))
	}
	for _, k := range kept {
		if cands[k].i == 1 {
			t.Error("pruned something, but kept the early drop that was the expensive one")
		}
	}
	if len(kept) == 0 {
		t.Error("pruned everything; the two late drops were affordable on their own")
	}
}

// Under the PRE-EXPIRY trigger nothing is pruned, because the prefix being invalidated is about to
// expire and W is therefore nearly worthless. Pruning there would forgo real savings to protect nothing.
// Asserted end to end, since the gate is in Offload rather than in the selector.
func TestPreExpiryAppliesEveryVoteWithoutAffordabilityPruning(t *testing.T) {
	asker := &labelAsker{verdict: "drop", needed: "none"}
	asker.cacheRead = 19595
	e := newSweepSmall(t, "econ_trigger: true\n") // econ ON, but pre-expiry is what will fire
	c := preExpiryCtx("s", asker, newMemStore())
	// THE WINDOW MUST BE TIGHT ENOUGH THAT PRUNING WOULD BITE, or this test passes whether the guard
	// exists or not. Found the hard way: at preExpiryCtx's default 1M window every drop is affordable,
	// so removing the !preExpiry guard changed nothing and the first version of this test was vacuous.
	// At 40k the request already fills the window, T collapses to ~0, and every drop is unaffordable --
	// so anything surviving here survives BECAUSE of the pre-expiry guard.
	c.CtxWindow = 40_000
	rep := &components.Report{}
	if !e.sweeping(c) {
		t.Fatal("fixture is wrong: pre-expiry did not fire, so this proves nothing")
	}
	if _, err := e.Offload(sweepReqCoref(), rep, c); err != nil {
		t.Fatal(err)
	}
	if rep.Events["prefix_rewrite_repaid"] != 0 {
		t.Fatal("fixture is wrong: econ fired, so the pre-expiry path is not what is being tested")
	}
	if rep.Gates["drop_unaffordable_pruned"] != 0 {
		t.Errorf("pruned drops on the pre-expiry path, where the invalidated prefix is nearly "+
			"worthless (gates: %v)", rep.Gates)
	}
}
