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
// T IS MEASURED ON THE REQUEST AS THE REMOVAL WILL LEAVE IT, not as it arrived — see
// turnsRemainingAfter. An earlier form measured it on the arriving request, which made the
// arithmetic self-defeating at exactly the pressure where a sweep is most wanted: at 90% of
// the window T read as zero, so no benefit could ever repay, even though removing half the
// transcript restores a long horizon. The restated consequence is narrower than the one
// this comment used to draw: a rewrite is charity when the removal is SMALL relative to the
// room it needs to buy back, which is a statement about the removal's size and not about
// where in the window it happens.
//
// A CALLER THAT PAYS A MODEL TO DECIDE has a second one-time cost, a discount on S, and a
// belief about what a removed token is worth. It takes prefixRewritePaysCharging instead:
//
//	cost    = 11.5 x W + ask         benefit = (S x approval x premium) x T
//
// `premium` is the reward premium: measured on iteration 024, the component paid $20.26 to
// bank $0.72 of cache savings — 28:1 against — while task reward rose 25% with 8 tasks
// better and none worse. So a removed token demonstrably delivers value the cache term does
// not see, and `premium` is where that belief is stated, in config, falsifiably.
//
// coref takes none of the three — its index is deterministic and free, and its selection is
// calibrated against the unadjusted expression — so it keeps prefixRewritePays, which is
// that same expression with ask 0, approval 1 and premium 1. See sweep_askcost.go.

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
	return prefixRewritePaysWith(req, saved, shallowest, rewritePricing{approval: 1, premium: 1}, c)
}

// rewritePricing carries the terms a caller that PAYS A MODEL to decide has and coref does not. A
// struct rather than four more positional parameters, because the call sites are the documentation:
// `rewritePricing{approval: 1, premium: 1}` reads as "no ask, no discount, no belief", which is exactly
// what coref is, and a reader does not have to count arguments to see it.
type rewritePricing struct {
	// askUSD is the price of the model call that decides WHAT to remove, in dollars.
	askUSD float64
	// approval is the fraction of `saved` that call is expected to actually take. 1 = no discount.
	approval float64
	// premium is how much a removed token is believed to be worth relative to the cache read it saves.
	// 1 = the unadjusted break-even. See extractSweepConfig.RewardPremium.
	premium float64
	// creditRemoval measures the turn horizon on the request AS THE REMOVAL WILL LEAVE IT rather than
	// as it arrived. See turnsRemainingAfter and #232.
	//
	// OPT-IN, and coref does not take it: coref calls prefixRewritePays (coref.go:390), which passes
	// creditRemoval false, so its arithmetic is untouched. THAT default is what protects coref's
	// calibration — not the state of prefixRewriteNet.
	//
	// AN EARLIER VERSION OF THIS COMMENT SAID OTHERWISE, and the correction is worth keeping. It claimed
	// "coref's drop SELECTION was calibrated against the uncredited form (prefixRewriteNet, which is
	// unchanged)", which read as a reason not to credit that function. coref never calls it:
	// prefixRewriteNet has exactly one caller, extract_llm_sweep's selectAffordableDrops. So the
	// asymmetry it described was protecting nothing, and it cost iteration 028's pre-flight 8 of 9 drops.
	creditRemoval bool
}

// prefixRewritePaysWith is prefixRewritePays plus the terms extract_llm_sweep's econ trigger needs and
// coref does not — see rewritePricing for each of them:
//
// See sweep_askcost.go for why both are measured rather than assumed, and for the pre-flight numbers
// that made them necessary.
//
// coref reaches this through prefixRewritePays with an empty-but-for-the-ones pricing, which reduces it
// to the original arithmetic TERM FOR TERM: it decides with a deterministic index, so there is no call
// to charge, no vote to discount, no belief to apply, and its horizon stays uncredited. That
// equivalence is asserted, not assumed — see TestPrefixRewritePaysUnchangedWithoutAnAsk.
func prefixRewritePaysWith(req *bschemas.BifrostChatRequest, saved, shallowest int,
	pricing rewritePricing, c *components.Ctx) (need, have int, ok bool) {

	askUSD, approval, premium := pricing.askUSD, pricing.approval, pricing.premium

	if c == nil || c.CtxWindow <= 0 {
		return 0, 0, true
	}
	if saved <= 0 {
		return 0, 0, false
	}
	if premium <= 0 {
		premium = 1
	}
	// EXPECTED mass, not offered mass. `saved` is the whole inventory; the model takes a subset.
	//
	// `premium` is the REWARD PREMIUM: how much a removed token is believed to be worth relative to
	// the cache read it saves. It multiplies the BENEFIT rather than dividing `need` at the end, which
	// is the same arithmetic but says the right thing — the claim is about what a removal delivers,
	// not about tolerating a cost. Note that it therefore shortens repayment for the ADJUDICATION too,
	// since need is a ratio: if a removed token is worth 20x, the call that produced it repays 20x
	// faster. That is intended, and it is asserted rather than left to be rediscovered — see
	// TestRewardPremiumScalesTheAskTermToo.
	//
	// 1 means "worth exactly its cache value", which is the original arithmetic and the default
	// everywhere except a config that opts in. See extractSweepConfig.RewardPremium for the evidence
	// behind any value above 1, and for why no evidence supports a value below it.
	expected := float64(saved) * approval
	eff := expected * premium
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
	// THE HORIZON IS MEASURED ON THE REQUEST AS IT WILL BE, not as it arrived. Removing `eff` tokens
	// is the very thing being priced, so pricing it against the pre-removal size asks "how many turns
	// remain if we do nothing" and then charges the removal against that answer. At high pressure the
	// two differ by everything: a 90k request against a 64k window has NO turns remaining and returns
	// 0, which no benefit can ever repay — while the same request with 52k removed sits at 60% of the
	// window with real turns ahead of it. Measured on iteration 024's own decisions, this is the
	// difference for 51 of 203 firings (25%), and for those a zero horizon refuses unconditionally: no
	// reward premium, however large, can clear ceil(m*need) <= 0. See issue #232.
	//
	// The growth RATE still comes from the pre-removal request, because the rate is a fact about the
	// history that has already happened; only the ROOM LEFT is a fact about the future. One number
	// used to serve both, which is why the credit could not simply be subtracted at the call site.
	reqNow := schema.MessagesTokens(req)
	after := reqNow
	if pricing.creditRemoval {
		// `expected`, NOT `eff`. The two differ by the premium, and the premium is a belief about what
		// a removed token is WORTH — it is not a claim that more tokens will be removed. Crediting
		// eff here silently multiplied the removal by the premium, so a premium of 20 pretended 20x
		// the mass had left the transcript and manufactured a horizon out of nothing. Caught by
		// TestRewardPremiumCannotAuthoriseAZeroHorizon, which is the assertion that exists to keep
		// the premium from reaching anything except the benefit term.
		after = reqNow - int(expected)
		if after < 1 {
			after = 1
		}
	}
	have = turnsRemainingAfter(reqNow, after, modelTurns(req), c.CtxWindow)
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
	return turnsRemainingAfter(reqTokens, reqTokens, turns, window)
}

// turnsRemainingAfter is estimateTurnsRemaining with the two roles reqTokens used to play separated:
//
//	reqBefore  the request as it stands — the GROWTH RATE is derived from it, because the rate is a
//	           property of the history that has already been observed
//	reqAfter   the request as it will be once the mutation under consideration is applied — the ROOM
//	           LEFT is derived from it
//
// Passing the same value for both reproduces the original expression term for term, which is what
// estimateTurnsRemaining above does and what keeps coref's arithmetic untouched.
//
// BOTH CALLERS NOW CREDIT THE REMOVAL. prefixRewriteNet did not until iteration 028, on a stated
// rationale that turned out to be misattributed — see the correction on rewritePricing.creditRemoval.
// coref's arithmetic is unaffected either way, because coref reaches this through prefixRewritePays,
// which passes creditRemoval false.
func turnsRemainingAfter(reqBefore, reqAfter, turns, window int) int {
	if window <= 0 || turns <= 0 || reqBefore <= 0 || reqAfter >= window {
		return 0
	}
	perTurn := reqBefore / turns
	if perTurn <= 0 {
		return 0
	}
	return (window - reqAfter) / perTurn
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
	// THE HORIZON IS CREDITED HERE TOO, and it has to be. This function is deciding WHETHER TO REMOVE
	// `saved` tokens, so measuring the turns remaining on the request as it ARRIVED asks "how long do we
	// last if we do nothing" and then charges the removal against that answer. #232 fixed exactly this in
	// prefixRewritePaysWith and left it standing here.
	//
	// MEASURED CONSEQUENCE of leaving it uncredited, from iteration 028's pre-flight: a 73,550-token
	// request against a 64,000 window has `reqAfter >= window`, so turnsRemainingAfter returns 0, so
	// S*T is 0 for every subset and net = -11.5*W. The walk below then keeps whichever subset has the
	// smallest W -- which is k=1, because k==1 is accepted unconditionally as the initial best. Observed:
	// the model authorised 9 drops, 8 were pruned, 105 tokens were removed across 64 requests.
	//
	// NO BELIEF TERM, and that is the difference from the trigger. There, the credit uses
	// `expected = saved * approval` because the ask has not happened and the mass is a projection. Here
	// the votes are IN HAND: `saved` is the actual mass of the subset being priced, so the post-removal
	// size is a fact about this decision rather than an estimate. The credit is less arguable here than
	// where it was first applied.
	reqNow := schema.MessagesTokens(req)
	after := reqNow - saved
	if after < 1 {
		after = 1
	}
	turns = turnsRemainingAfter(reqNow, after, modelTurns(req), c.CtxWindow)
	return float64(saved)*float64(turns) - cacheWriteX*float64(rewritten), rewritten, turns
}
