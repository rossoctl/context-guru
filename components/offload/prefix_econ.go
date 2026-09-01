package offload

import (
	"math"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
)

// Economics of deliberately mutating the provider's CACHED PREFIX.
//
// Every other offloader refuses to touch the prefix (Ctx.TailOnly) because breaking the
// prefix hash at index i forces the provider to cache-WRITE everything from i onward. Two
// components choose to pay that on purpose — coref, and extract_llm when
// allow_cached_prefix is set — so the price lives here rather than being reimplemented,
// slightly differently, in each of them.
//
//	cost    = W x (2.50 - 0.20) = 11.5 x W   (in cache-read-equivalents)
//	benefit = S x T x 0.20      =  S x T
//	worth it when  S x T > 11.5 x W
//
// S is the mass removed; W is the suffix the mutation forces the provider to re-write —
// counted from the shallowest mutated index to the CACHED boundary, because content past
// that boundary was never cached and would be written this turn regardless; T is how many
// turns remain to collect the saving on, which nobody has, so it is estimated from how
// fast the transcript has been growing.
//
// The consequence is counter-intuitive and worth restating wherever this is used: firing
// at 90% of the window means T is nearly zero — paying a rewrite for a saving collected
// once. The profitable moment to compact is EARLIER than the moment of maximum pressure.

// cacheWriteX is one cache-write in cache-read-equivalents: ($2.50 - $0.20) / $0.20 on
// Anthropic's published per-MTok prices. Shared with deploy/harbor/coref.py.
const cacheWriteX = 11.5

// Co-reference classifier defaults, shared by coref and by extract_llm's prefix
// pre-filter. One definition on purpose: if the two components classified the same output
// differently, the "free deterministic pre-filter" would be answering a different question
// from the component whose measurements calibrated it. Mirrors deploy/harbor/coref.py.
const (
	corefClosedDistDefault = 12
	corefOpenRepsDefault   = 3
	// corefMinLaterDefault is the opportunity floor: an output with fewer model turns after
	// it has not yet HAD a chance to be referenced, so "unreferenced" says nothing about it.
	corefMinLaterDefault = 8
)

// prefixRewriteWindow reports the last message index the provider is believed to already
// hold. An unknown boundary assumes the whole transcript is cached, which over-states the
// rewrite cost rather than under-stating it.
func prefixRewriteWindow(req *bschemas.BifrostChatRequest, c *components.Ctx) int {
	end := len(req.Input) - 1
	if c != nil && c.CacheAware && c.MaxCachedIdx >= 0 && c.MaxCachedIdx < end {
		end = c.MaxCachedIdx
	}
	return end
}

// prefixRewritePays applies S*T > 11.5*W for a mutation of `saved` tokens whose shallowest
// touched index is `shallowest`. Returns (needed T, estimated T, whether it clears).
//
// Always clears when the context window is unknown — the same convention as every
// fraction-based threshold in this package: an unresolvable threshold imposes no
// constraint rather than silently disabling the pass.
func prefixRewritePays(req *bschemas.BifrostChatRequest, saved, shallowest int, c *components.Ctx) (need, have int, ok bool) {
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
	rewritten -= saved // the removed mass is not part of what gets written back
	if rewritten <= 0 {
		return 0, estimateTurnsRemaining(schema.MessagesTokens(req), modelTurns(req), c.CtxWindow), true
	}
	need = int(math.Ceil(cacheWriteX * float64(rewritten) / float64(saved)))
	have = estimateTurnsRemaining(schema.MessagesTokens(req), modelTurns(req), c.CtxWindow)
	return need, have, need <= have
}

// estimateTurnsRemaining projects how many more turns fit before the request reaches the
// model's window, assuming the transcript keeps growing at the average rate it has so
// far. Crude on purpose: T only has to be right to an order of magnitude to separate
// "this rewrite pays for itself" from "this rewrite is charity", and every cheaper proxy
// (elapsed turns, observed step rate) is the same shape of guess.
func estimateTurnsRemaining(reqTokens, turns, window int) int {
	if window <= 0 || turns <= 0 || reqTokens <= 0 || reqTokens >= window {
		return 0
	}
	perTurn := reqTokens / turns
	if perTurn <= 0 {
		return 0
	}
	return (window - reqTokens) / perTurn
}

// modelTurns counts assistant messages — the closest thing in a request to "steps taken",
// which is the unit the growth rate is per.
func modelTurns(req *bschemas.BifrostChatRequest) int {
	n := 0
	for i := range req.Input {
		if req.Input[i].Role == bschemas.ChatMessageRoleAssistant {
			n++
		}
	}
	return n
}
