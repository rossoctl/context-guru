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

// econCtx is a request-shaped context with a known window. MaxCachedIdx -1 with CacheAware means
// "cache-aware, boundary unknown": the whole transcript is assumed cached.
func econCtx(window int) *components.Ctx {
	return &components.Ctx{CtxWindow: window, CacheAware: true, MaxCachedIdx: -1}
}

// Passing the same value for both reproduces the original expression, which is what keeps coref's
// arithmetic untouched. Asserted against a literal recomputation, not against the new code.
func TestTurnsRemainingAfterCollapsesToTheOriginal(t *testing.T) {
	for _, tc := range []struct{ req, turns, window int }{
		{20_000, 10, 100_000}, {99_999, 3, 100_000}, {100_000, 3, 100_000},
		{1, 1, 100_000}, {20_000, 0, 100_000}, {20_000, 10, 0},
	} {
		got := estimateTurnsRemaining(tc.req, tc.turns, tc.window)
		want := 0
		if tc.window > 0 && tc.turns > 0 && tc.req > 0 && tc.req < tc.window {
			if per := tc.req / tc.turns; per > 0 {
				want = (tc.window - tc.req) / per
			}
		}
		if got != want {
			t.Fatalf("estimateTurnsRemaining(%d,%d,%d) = %d, want %d", tc.req, tc.turns, tc.window, got, want)
		}
	}
}

// COREF's horizon is measured on the request AS IT ARRIVED. A request already past the window has no
// turns left, so nothing can repay and coref declines. The sweep's removed pricing credited the removal;
// coref never did, and must not start.
func TestCorefHorizonIsNotCredited(t *testing.T) {
	req := bigReq(24, 400)
	total := schema.MessagesTokens(req)
	c := econCtx(total * 7 / 10) // over-window: the case where crediting would change the answer
	if _, have, ok := prefixRewritePays(req, total/2, 2, c); have != 0 || ok {
		t.Fatalf("coref's path started crediting the removal: have=%d ok=%v, want 0/false", have, ok)
	}
}
