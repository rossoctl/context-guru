package offload

import (
	"math"
	"strconv"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
)

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
	need, have, ok := prefixRewritePaysWith(req, saved, last, rewritePricing{askUSD: 5.00, approval: 1, premium: 1, creditRemoval: true}, c)
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
	full, _, _ := prefixRewritePaysWith(req, 4_000, 2, rewritePricing{askUSD: 0.10, approval: 1, premium: 1, creditRemoval: true}, c)
	third, _, _ := prefixRewritePaysWith(req, 4_000, 2, rewritePricing{askUSD: 0.10, approval: 1.0 / 3.0, premium: 1, creditRemoval: true}, c)
	if full == 0 {
		t.Fatal("fixture is wrong: need is 0 even undiscounted, so the discount cannot show")
	}
	if third < full*2 {
		t.Errorf("approval 1/3 moved need from %d to %d; expecting roughly 3x, so the discount "+
			"is not reaching the denominator", full, third)
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
