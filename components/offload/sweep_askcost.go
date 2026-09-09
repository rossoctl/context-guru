package offload

import (
	"sync"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
)

// PRICING THE QUESTION, not just the answer.
//
// prefixRewritePays asks whether MUTATING the cached prefix pays: a one-time cache-write set against a
// saving collected on every remaining turn. For coref that is the entire cost, because coref decides
// with a deterministic index and asks nobody. For extract_llm_sweep's econ trigger it is not — the
// component must PAY A MODEL to learn which outputs are spent, and that call is billed whether or not
// the answer turns out to be "drop something".
//
// MEASURED, iteration 025 pre-flight (one 64k LOCA task, arm B, cg-i025-proxy-v01):
//
//	9 asks authorised, 9 of 9 by the econ trigger    $0.4339 spent
//	6 of the 9 removed NOTHING                       8,613 tokens removed in total
//	needTurns 0 on 5 asks, 1 on the other 4          haveTurns 38-53 throughout
//	                                                 net_value_usd -$0.4322
//
// Two independent reasons the old test could not have declined a single one of them:
//
//  1. THE ASK WAS FREE. The numerator was `cacheWriteX * rewritten` and nothing else, so the
//     adjudication's own price was not a term in the decision at all — and on this workload that price
//     was the whole cost, because the rewrite collapsed to zero on most asks.
//  2. `saved` IS WHAT MIGHT BE DROPPED, NOT WHAT WILL BE. econPays sums the entire candidate
//     inventory, but the model takes a subset, and six asks in nine it took none. econPays' own comment
//     already said S was an upper bound; what it never had was an estimate of HOW loose.
//
// And the two compounded precisely where each did the most damage. `rewritten <= 0` — every candidate
// at or past the cached boundary, so no prefix needs re-writing — took an early return that authorised
// UNCONDITIONALLY. That is the one branch on which the cache-write is genuinely free, which makes the
// ask the ONLY cost, i.e. the branch where the missing term was the entire answer. It fired on 5 of 9.

// askLedger is what this component's own adjudications have actually cost, and how much of the mass it
// offered them actually came back removed.
//
// It exists so both corrections above are MEASUREMENTS rather than literals — the same reason
// agentRates prefers Ctx.SelfRates to the constants beside it. A hardcoded approval rate would be one
// workload's average wearing a threshold's authority, and this component has already been burned once
// by exactly that (defaultMinInventory's 10, unreachable on this benchmark, blocking everything
// downstream in silence).
//
// PROCESS-WIDE, not per-session, and deliberately: the price of a call and the model's willingness to
// act on an inventory are properties of the workload and the prompt, not of one transcript, and a
// per-session ledger would spend each session's first asks re-paying for what the previous session
// already learned. The cost is that a session whose workload genuinely differs inherits its
// predecessor's rate; `estFromMeasurement` in cg.sweep.econ is what makes that visible rather than
// assumed.
type askLedger struct {
	mu       sync.Mutex
	asks     int
	costUSD  float64
	offered  int
	approved int
}

// record books one completed adjudication: what it cost, the candidate mass it was shown, and the mass
// that actually reached the wire as a result.
//
// `approved` is the wire-level saving, not the adjudicator's vote, and that is the point: the ratio
// this builds has to map "mass I am about to offer" onto "mass I will actually stop sending", so every
// later stage that shrinks the outcome — selectAffordableDrops' pruning, tryMark's refusals, the
// marker's own tokens — belongs inside the measured fraction rather than outside it.
func (l *askLedger) record(costUSD float64, offered, approved int) {
	if l == nil || offered <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.asks++
	l.costUSD += costUSD
	l.offered += offered
	l.approved += approved
}

// estimate reports what the NEXT ask should be assumed to cost, and what fraction of the mass offered
// to it should be assumed to come back. `measured` says whether either figure came from this
// component's own asks or from the priors.
//
// `prior` supplies the cost until enough asks have completed to average — see askCostPrior.
func (l *askLedger) estimate(prior float64) (costUSD, approval float64, measured bool) {
	if l == nil {
		return prior, 1, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.asks < minAskSamples {
		// The prior on APPROVAL is 1.0, i.e. exactly what the gate assumed before this file existed.
		// Deliberate: the opening asks are the measurement, and starting pessimistic would decline the
		// very calls that produce the data. Bounded by minAskSamples, so it is a warm-up and not a
		// policy.
		return prior, 1, false
	}
	costUSD = l.costUSD / float64(l.asks)
	approval = float64(l.approved) / float64(l.offered)
	// FLOORED, and this is a ratchet guard rather than a fudge. A measured approval of exactly zero
	// would drive the expected saving to zero, decline every subsequent ask, and thereby destroy the
	// only source of new evidence — the component would disable itself permanently on the strength of
	// however few asks happened to come back empty, with no path back. The floor keeps a large enough
	// inventory able to clear the bar, which is what lets the estimate be revised.
	if approval < approvalFloor {
		approval = approvalFloor
	}
	return costUSD, approval, true
}

const (
	// minAskSamples is how many completed asks the ledger wants before it trusts its own averages over
	// the priors. Three, because the quantity being estimated is a RATE and a single ask can only ever
	// report 0% or 100% of its inventory — the pre-flight's own asks came back 0,1327,2300,0,0,0,0,0,
	// 5291 tokens, a sequence whose first three entries average to something usable and whose first
	// one does not.
	minAskSamples = 3
	// approvalFloor is the lowest approval rate the estimator will report. See estimate's ratchet note.
	approvalFloor = 0.05
	// askReplyTokens is the completion budget one adjudication is assumed to spend. The reply is a vote
	// list bounded by maxAskItems, so it is small and nearly constant — unlike the prompt, which grows
	// with the transcript.
	askReplyTokens = 500
	// askOutputX is output price over input price when the rate card prices input but not output. 5x is
	// the ratio across Anthropic's current cards ($2/$10 for the sonnet-5 this deployment bills at,
	// $1/$5 for haiku-4-5), and it applies to askReplyTokens only, so being wrong here moves the
	// estimate by a few percent.
	askOutputX = 5.0
)

// askCostPrior estimates the price of an adjudication BEFORE one has been made, from the shape of the
// request it would read.
//
// The ask re-reads the transcript on the REQUEST's own model — there is no `model` block on this
// component, by construction, because only that model's cache holds the transcript — so the bill is
// dominated by whatever part of the transcript the provider does NOT already hold. Measured on the
// pre-flight's ninth ask: 28,594 fresh tokens against 52,074 cached, which at sonnet-5's $2.00/$0.20
// per MTok is $0.057 fresh against $0.010 cached. The split at the cache boundary is therefore the
// term that matters.
//
// WHY THIS DOES NOT USE prefixRewriteWindow, which already knows where that boundary is believed to be:
// its convention on an UNKNOWN boundary is to assume the whole transcript is cached, because for a
// rewrite cost that over-states and is therefore safe. For an ASK cost the identical assumption
// under-states — everything looks like a cheap cache read — and under-stating the ask is exactly the
// failure this file exists to fix. So the unknown case is resolved the other way here: nothing is
// assumed cached. The two conventions are opposite ON PURPOSE, and each leans against authorising.
//
// The prompt's own rules and inventory lines are not counted. They are a few hundred tokens against a
// transcript of tens of thousands, and this figure governs only the first minAskSamples asks.
func askCostPrior(req *bschemas.BifrostChatRequest, c *components.Ctx) float64 {
	if c == nil || req == nil {
		return 0
	}
	fresh, read, _ := agentRates(c)
	cachedTo := -1
	if c.CacheAware && c.MaxCachedIdx >= 0 {
		cachedTo = c.MaxCachedIdx
	}
	cached, uncached := 0, 0
	for j := range req.Input {
		t := schema.TextTokens(schema.MessageText(req.Input[j]))
		if j <= cachedTo {
			cached += t
		} else {
			uncached += t
		}
	}
	out := c.SelfRates.Output
	if out <= 0 {
		out = fresh * askOutputX
	}
	return float64(cached)*read + float64(uncached)*fresh + askReplyTokens*out
}

// candMass is the candidate inventory's total token mass and its shallowest (earliest) index — the two
// inputs the prefix break-even takes. Shared by the econ test and by the ledger's booking so the mass
// the gate PRICED and the mass it is later judged against are the same number computed one way.
func candMass(cands []sweepCand) (mass, shallowest int) {
	if len(cands) == 0 {
		return 0, 0
	}
	shallowest = cands[0].i
	for _, cd := range cands {
		mass += schema.TextTokens(cd.content)
		if cd.i < shallowest {
			shallowest = cd.i
		}
	}
	return mass, shallowest
}
