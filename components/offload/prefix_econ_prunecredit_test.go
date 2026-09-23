package offload

import (
	"context"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
)

// THE FAILURE THESE PIN, measured on iteration 028's pre-flight rather than imagined: a 73,550-token
// request against a 64,000 window left `reqAfter >= window`, so the pruner's horizon was 0, so S*T was 0
// for every candidate subset and net reduced to -11.5*W. selectAffordableDrops then kept whichever subset
// had the smallest W -- k=1, because k==1 is accepted unconditionally as the initial best. The model
// authorised 9 drops, 8 were pruned, and 105 tokens were removed across 64 requests.
//
// The horizon has to be measured on the request AS THE REMOVAL WILL LEAVE IT, because that is the
// decision being priced. #232 established this for the trigger; these assert it for the pruner.
//
// SIZED EXPLICITLY, and every fixture asserts its own shape before asserting anything else. The first
// version of this test failed for a reason worth recording: it removed just enough to cross the window,
// leaving 2,000 tokens of headroom against a per-turn growth of 6,180, and `(window-after)/perTurn`
// floored to 0. That was the arithmetic being right, not the credit being absent -- so a fixture here has
// to leave headroom worth MORE THAN ONE TURN or it tests integer division instead of the credit.

// filler returns roughly n tokens of identifier-shaped text.
func filler(n int) string {
	unit := "alpha_beta/gamma-01 delta.epsilon:zeta 1234567 "
	var b strings.Builder
	for schema.TextTokens(b.String()) < n {
		b.WriteString(unit)
	}
	return b.String()
}

// pressuredReq builds a request with `turns` assistant messages and `nCand` tool results of `candTok`
// tokens each, so modelTurns, MessagesTokens and the candidate mass are all controlled.
func pressuredReq(turns, nCand, candTok int) (*bschemas.BifrostChatRequest, []sweepCand) {
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("Analyse the attached exports and produce the summary."),
	}}
	for i := 0; i < turns; i++ {
		req.Input = append(req.Input, asstMsg("step", "bash", `{}`))
	}
	body := filler(candTok)
	var cands []sweepCand
	for i := 0; i < nCand; i++ {
		req.Input = append(req.Input, toolResultMsg(body))
		cands = append(cands, sweepCand{i: len(req.Input) - 1, content: body})
	}
	return req, cands
}

func TestPrunerHorizonIsCreditedForTheRemoval(t *testing.T) {
	const window = 64000
	c := &components.Ctx{Ctx: context.Background(), CtxWindow: window}
	req, _ := pressuredReq(20, 10, 8000)
	reqNow := schema.MessagesTokens(req)
	perTurn := reqNow / modelTurns(req)
	if reqNow <= window {
		t.Fatalf("fixture must exceed the window or the assertions are vacuous: %d <= %d", reqNow, window)
	}

	// Too small to cross the window: the horizon is still 0, and correctly so.
	if _, _, turns := prefixRewriteNet(req, 500, len(req.Input)-1, c); turns != 0 {
		t.Errorf("a removal leaving the request over the window must report 0 turns, got %d", turns)
	}

	// Large enough to cross it AND to leave more than one turn of headroom. Under the uncredited form
	// this is 0 too, which is the defect.
	saved := reqNow - window + 4*perTurn
	_, _, turns := prefixRewriteNet(req, saved, len(req.Input)-1, c)
	t.Logf("reqNow=%d window=%d perTurn=%d | saved=%d -> after=%d turns=%d",
		reqNow, window, perTurn, saved, reqNow-saved, turns)
	if turns <= 0 {
		t.Fatalf("removing %d tokens leaves %d against a %d window with %d headroom (perTurn %d), so the "+
			"horizon must be positive; got %d. The uncredited form prices the removal against the "+
			"PRE-removal size and refuses every drop at high pressure.",
			saved, reqNow-saved, window, window-(reqNow-saved), perTurn, turns)
	}
}

// AND THE CONSEQUENCE THAT MATTERS: with the horizon credited, a batch of drops that collectively clears
// the window is no longer pruned to one. Asserted through selectAffordableDrops, because that is where
// the tokens were lost.
func TestAffordableDropsAreNotPrunedToOneAtHighPressure(t *testing.T) {
	const window = 64000
	c := &components.Ctx{Ctx: context.Background(), CtxWindow: window}
	e := &ExtractSweep{}
	req, cands := pressuredReq(20, 10, 8000)
	reqNow := schema.MessagesTokens(req)
	if reqNow <= window {
		t.Fatalf("fixture must exceed the window: %d", reqNow)
	}
	all := make([]int, len(cands))
	for i := range cands {
		all[i] = i
	}
	kept, pruned := e.selectAffordableDrops(req, c, cands, all)
	t.Logf("req=%d window=%d perTurn=%d | offered %d drops -> kept %d, pruned %d",
		reqNow, window, reqNow/modelTurns(req), len(all), len(kept), pruned)
	if len(kept) <= 1 {
		t.Errorf("kept %d of %d drops. Pruning a whole batch to one is the measured defect: every subset "+
			"scored net = -11.5*W because the horizon was 0, so the smallest W won.", len(kept), len(all))
	}
}
