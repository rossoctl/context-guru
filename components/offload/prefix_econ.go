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
//
// A CALLER THAT PAYS A MODEL TO DECIDE has a second one-time cost and a discount on S, and
// takes prefixRewritePaysCharging instead:
//
//	cost    = 11.5 x W + ask         benefit = (S x approval) x T
//
// coref does not — its index is deterministic and free — so it keeps prefixRewritePays,
// which is that same expression with ask 0 and approval 1. See sweep_askcost.go.

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
	return prefixRewritePaysCharging(req, saved, shallowest, 0, 1, c)
}

// prefixRewritePaysCharging is prefixRewritePays plus the two terms extract_llm_sweep's econ trigger
// needs and coref does not:
//
//	askUSD    the price of the model call that decides WHAT to remove, in dollars
//	approval  the fraction of `saved` that call is expected to actually take
//
// See sweep_askcost.go for why both are measured rather than assumed, and for the pre-flight numbers
// that made them necessary.
//
// coref reaches this through prefixRewritePays with askUSD 0 and approval 1, which reduces it to the
// original arithmetic TERM FOR TERM: coref decides with a deterministic index, so there is no call to
// charge and no vote to discount. That equivalence is asserted, not assumed — see
// TestPrefixRewritePaysUnchangedWithoutAnAsk.
func prefixRewritePaysCharging(req *bschemas.BifrostChatRequest, saved, shallowest int,
	askUSD, approval float64, c *components.Ctx) (need, have int, ok bool) {

	if c == nil || c.CtxWindow <= 0 {
		return 0, 0, true
	}
	if saved <= 0 {
		return 0, 0, false
	}
	// EXPECTED mass, not offered mass. `saved` is the whole inventory; the model takes a subset.
	eff := float64(saved) * approval
	if eff <= 0 {
		return 0, 0, false
	}
	end := prefixRewriteWindow(req, c)
	rewritten := 0
	for j := shallowest; j <= end && j < len(req.Input); j++ {
		rewritten += schema.TextTokens(schema.MessageText(req.Input[j]))
	}
	rewritten -= saved // the removed mass is not part of what gets written back
	if rewritten < 0 {
		// CLAMPED, not returned on. This was `return 0, T, true` — an unconditional authorisation on
		// the one branch where the cache-write is free and the ask is therefore the ONLY cost, which
		// is how the econ trigger came to authorise 9 asks in 9 and lose $0.4322 doing it. With
		// askUSD 0 the arithmetic below still yields need 0 and ok true, so coref is untouched.
		rewritten = 0
	}
	have = estimateTurnsRemaining(schema.MessagesTokens(req), modelTurns(req), c.CtxWindow)
	// The ask converted into cache-read-equivalents, so it can be added to a cache-write term already
	// expressed that way: dollars / (dollars per cache-read token) = tokens.
	askEquiv := 0.0
	if askUSD > 0 {
		if _, read, _ := agentRates(c); read > 0 {
			askEquiv = askUSD / read
		}
	}
	onetime := cacheWriteX*float64(rewritten) + askEquiv
	if onetime <= 0 {
		return 0, have, true
	}
	need = int(math.Ceil(onetime / eff))
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

// prefixRewriteNet is prefixRewritePays' arithmetic with the terms exposed rather than reduced to a
// verdict: the NET benefit, in cache-read-equivalents, of removing `saved` tokens whose shallowest
// touched index is `shallowest`.
//
//	net = S*T - 11.5*W
//
// Exposed because choosing WHICH drops to apply is an optimisation over subsets, not a yes/no test, and
// a bool cannot be maximised. See selectAffordableDrops.
//
// Note the sign convention that makes the walk work: `shallowest` is the SMALLEST index, i.e. the
// EARLIEST message. Removing something earlier forces the provider to re-write everything after it, so
// W grows as the drop set reaches further back. A drop late in the cached region is nearly free; the
// same drop early in it can cost more than every other drop in the batch combined.
func prefixRewriteNet(req *bschemas.BifrostChatRequest, saved, shallowest int, c *components.Ctx) (net float64, rewritten, turns int) {
	if c == nil || c.CtxWindow <= 0 || saved <= 0 {
		return 0, 0, 0
	}
	end := prefixRewriteWindow(req, c)
	for j := shallowest; j <= end && j < len(req.Input); j++ {
		rewritten += schema.TextTokens(schema.MessageText(req.Input[j]))
	}
	rewritten -= saved // the removed mass is not written back
	if rewritten < 0 {
		rewritten = 0
	}
	turns = estimateTurnsRemaining(schema.MessagesTokens(req), modelTurns(req), c.CtxWindow)
	return float64(saved)*float64(turns) - cacheWriteX*float64(rewritten), rewritten, turns
}
