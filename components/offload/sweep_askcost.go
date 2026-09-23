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
	mu      sync.Mutex
	asks    int
	costUSD float64
	// Pooled mass, kept alongside the per-bucket mass rather than derived from it: the pooled figure is
	// what a bucket with no history of its own shrinks toward, so it has to exist before any bucket does.
	offered  int
	approved int
	// APPROVAL IS A CURVE IN INVENTORY SIZE, NOT A SCALAR (issue #230), and keeping it as a scalar is
	// what shut the component down on a measured probe. The component's own evidence: shown one output,
	// 6% live-kept on haiku and 14% on sonnet; shown ~15, 58%; batch 3-6 dropped a genuinely-spent
	// output 2 times in 4; batch 10 dropped it 4 in 4. A ratio pooled over asks is dominated by whatever
	// regime the gate happened to admit -- and the gate admits THIN asks, because `have` is remaining
	// window room and shrinks as pressure rises. So the estimate was measured at three candidates and
	// then applied to twelve, which declined every rich ask and denied the ledger the only evidence that
	// would have revised it.
	//
	// Three buckets, not a fitted curve: with this much data a fit would be noise wearing a shape. The
	// splits are the inflection the measurements above report.
	buckets map[int]*askBucket
}

// askBucket is one inventory-size regime's mass. Split out so a rich ask is priced by rich-ask history.
type askBucket struct {
	offered  int
	approved int
}

// approvalBucket maps an inventory size onto its regime. Below 7 is where a drop is documented to be a
// guess; 10 and above is where judgement was measured sound; between them is the transition.
func approvalBucket(inventory int) int {
	switch {
	case inventory < 7:
		return 0
	case inventory < 10:
		return 1
	default:
		return 2
	}
}

// record books one completed adjudication: what it cost, the candidate mass it was shown, and the mass
// that actually reached the wire as a result.
//
// `approved` is the wire-level saving, not the adjudicator's vote, and that is the point: the ratio
// this builds has to map "mass I am about to offer" onto "mass I will actually stop sending", so every
// later stage that shrinks the outcome — selectAffordableDrops' pruning, tryMark's refusals, the
// marker's own tokens — belongs inside the measured fraction rather than outside it.
func (l *askLedger) record(costUSD float64, offered, approved, inventory int) {
	if l == nil || offered <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.asks++
	l.costUSD += costUSD
	l.offered += offered
	l.approved += approved
	if l.buckets == nil {
		l.buckets = map[int]*askBucket{}
	}
	b := l.buckets[approvalBucket(inventory)]
	if b == nil {
		b = &askBucket{}
		l.buckets[approvalBucket(inventory)] = b
	}
	b.offered += offered
	b.approved += approved
}

// estimate reports what the NEXT ask should be assumed to cost, and what fraction of the mass offered
// to it should be assumed to come back. `measured` says whether either figure came from this
// component's own asks or from the priors.
//
// `prior` supplies the cost until enough asks have completed to average — see askCostPrior.
func (l *askLedger) estimate(prior float64, inventory int) (costUSD, approval float64, measured bool) {
	if l == nil {
		return prior, approvalPrior, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// COST AND APPROVAL NO LONGER SHARE ONE SWITCH, and they never should have. The cost is an AVERAGE
	// over asks, so it genuinely needs a few asks before it beats the prior — minAskSamples is the right
	// instrument for it. The approval is a MASS RATIO, whose precision scales with offered mass rather
	// than with how many calls produced it, and gating it on an ask count is what produced a 20x cliff:
	// on a measured probe the third ask completed, the switch flipped, and a pooled ratio of 0.0124
	// declined every one of the next 48 evaluations.
	costUSD, measured = prior, false
	if l.asks >= minAskSamples {
		costUSD, measured = l.costUSD/float64(l.asks), true
	}
	return costUSD, l.approvalLocked(inventory), measured
}

// approvalLocked is the estimate itself. Caller holds the lock.
//
//	approval = (approved + k*prior) / (offered + k)
//
// MASS-WEIGHTED SHRINKAGE, and each property answers a measured failure:
//
//   - k is in TOKENS OF OFFERED MASS, not asks. Three 2k-token asks were three units of evidence to the
//     old estimator and are 6k units to this one, which is the right unit for a mass ratio. It is also
//     what makes the estimate move continuously instead of switching.
//   - the bucket shrinks toward the POOLED estimate once any mass has been seen anywhere, so a regime
//     with no history of its own inherits the component's overall experience rather than fixed optimism.
//     That is issue #230 as arithmetic: a twelve-candidate ask is not priced by three-candidate history.
//   - the FLOOR STAYS. Shrinkage does not subsume it, which a simulation over 384 decision points showed
//     directly: at k=300k the unfloored form converged to 0.033, below the floor, and made 11 fewer asks
//     than the floored one at 128k. The floor is an optimistic bias that keeps the component asking, and
//     that is worth keeping for the reason its original note gives — a zero estimate destroys the only
//     source of evidence that could revise it.
func (l *askLedger) approvalLocked(inventory int) float64 {
	prior := approvalPrior
	if l.offered > 0 {
		prior = (float64(l.approved) + approvalShrinkTokens*approvalPrior) /
			(float64(l.offered) + approvalShrinkTokens)
	}
	var off, app int
	if b := l.buckets[approvalBucket(inventory)]; b != nil {
		off, app = b.offered, b.approved
	}
	v := (float64(app) + approvalShrinkTokens*prior) / (float64(off) + approvalShrinkTokens)
	if v < approvalFloor {
		v = approvalFloor
	}
	if v > 1 {
		v = 1
	}
	return v
}

const (
	// minAskSamples is how many completed asks the ledger wants before it trusts its own averages over
	// the priors. Three, because the quantity being estimated is a RATE and a single ask can only ever
	// report 0% or 100% of its inventory — the pre-flight's own asks came back 0,1327,2300,0,0,0,0,0,
	// 5291 tokens, a sequence whose first three entries average to something usable and whose first
	// one does not.
	minAskSamples = 3
	// approvalFloor is the lowest approval rate the estimator will report. See approvalLocked's note on
	// why shrinkage does not replace it.
	approvalFloor = 0.05
	// approvalShrinkTokens is the shrinkage pseudo-count, in tokens of offered mass: the estimate sits at
	// the prior until this much mass has been offered, and leaves it continuously thereafter.
	//
	// ONE MILLION, chosen from a simulation over 384 decision points that ran the ledger sequentially and
	// compared estimators against an identical, swept outcome model. The ordering was stable in all six
	// cells (three outcome models x two windows): k=1M floored bucketed beat k=300k, which beat the
	// unfloored form, which beat a pooled form, which beat the shipped pooled ratio. At 64k under the
	// pessimistic outcome model it made 132 asks against the shipped 36 and removed 68,955 tokens against
	// 7,329 — 9.4x.
	//
	// WHAT THE VALUE MEANS IN PRACTICE, because it is large and should not be mistaken for a small
	// correction: the ledger belongs to a component instance and outlives any one session, so a million
	// tokens of offered mass is hours of traffic rather than one task. Within a single short session the
	// discount therefore barely engages — which is deliberate, and is the asymmetry argument: one ask
	// measured $0.036-$0.049 on the probe, while a removal that never happened costs reward. Erring
	// toward asking is the cheap direction.
	approvalShrinkTokens = 1_000_000.0
	// approvalPrior is what the estimate reports before any mass has been offered — 1.0, i.e. what the
	// gate assumed before this file existed. The opening asks ARE the measurement, and starting
	// pessimistic declines the very calls that would produce the data.
	approvalPrior = 1.0
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
