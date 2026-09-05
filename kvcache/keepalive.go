package kvcache

import (
	"fmt"
	"time"
)

// ── how MANY keep-alives, decided per conversation ──────────────────────────
//
// Config.MaxPings is one number for the whole replay. That is the right shape for a
// hand-written arm — "hold every idle conversation for two intervals" is a policy an
// operator can state — but it is the wrong shape for a learned one, because the budget a
// conversation is worth depends on the conversation. On this deployment's corpus the
// budget a per-conversation policy chooses is 0 for 77.8% of idle spans and runs out to 8
// for a long tail, and collapsing that to a single K is most of the difference between the
// two.
//
// PingBudgeter is how a strategy says so. It is OPTIONAL and additive: a strategy that does
// not implement it is budgeted by Config.MaxPings exactly as before, which is every arm that
// exists today.

// PingBudgeter is an optional interface a Strategy may implement to choose the keep-alive
// budget per conversation instead of accepting Config.MaxPings for all of them.
//
// The (value, ok) shape is Predictor's, for the same reason: a policy with no opinion on a
// particular observation must be able to say so and fall back, rather than return a zero
// that reads as "never ping". ok=false means "use Config.MaxPings", not "use 0".
type PingBudgeter interface {
	// PingBudget is how many keep-alives to allow on the idle span starting at o.Now.
	// Config.MaxPings still caps the result: this can ask for fewer, never for more.
	PingBudget(o Observation) (budget int, ok bool)
}

// BudgetPolicy is the LEARNED keep-alive arm: it asks a Predictor how likely the
// conversation is to come back in each window a ping would protect, and buys the pings that
// pay for themselves.
//
// It is deliberately a decision layer over Predictor rather than a model of its own. The
// model is Python (deploy/harbor/kv_ttl_keepalive_policy.py), because that is where a fit
// belongs; what was missing on the Go side, and what this is, is the arithmetic that turns
// probabilities into a ping count. Handing it a stub Predictor is how it is tested.
//
// # THE GATE
//
// One ping costs Pricing.KeepAliveCost — a cache read over the prefix, plus the fresh input
// and the one output token a ping cannot avoid. A ping that lands while the entry is alive
// and is followed by the conversation actually returning converts the successor's cache
// WRITE into a cache READ, so it avoids
//
//	rescue = prefix × (write_5m_rate − cache_read_rate)
//
// Ping if the chance of that exceeds its cost, i.e. if rescue×h > ping. On the documented
// Anthropic multiples (read 0.1×, write 1.25× of base input) the prefix term cancels and the
// threshold is 0.1/1.15 = 8.70% — the same number for every prefix size and every model,
// which is why this reads a probability and not a token count. It is computed from
// o.Pricing rather than written down because a deployment whose rates differ has a different
// threshold, and one that has no rates at all has none.
//
// # WHY IT IS NOT THAT SIMPLE
//
// 8.70% is the MYOPIC bar, and it is too high. A ping does two things: it may rescue this
// window, and it keeps the entry alive so that the NEXT ping is still a cheap read rather
// than a 12.5× re-creation. The second is worth paying for on its own. So the budget is a
// backward induction over the windows a ping would protect,
//
//	V_j = max(0, rescue×h_j − ping + s_j×V_(j+1)),   V_(MaxK) = 0
//
// and the budget is the first j at which V_j is not positive — the FIRST, not the last,
// because the hazard is not monotone in j on real traffic and taking the last positive one
// buys a run of pings across a dead stretch to reach a live one. That option value lowers
// the effective bar to about 7.4% at the first sweep and is worth ~6% of the arm's net on
// the measured corpus.
//
// # WHAT IT IS WORTH
//
// Measured on the hosted deployment's capture: 34,577 idle spans over 16.9 days, 5-fold rolling
// origin, pings at 290s. Given as a share of the window's bill and of what `optimal` reaches,
// with per-ping efficiency indexed to flat MaxPings=6:
//
//	arm                     pings   off the bill   of the ceiling   net per ping
//	optimal (unreachable)   5,826         11.72%           100.0%          27.7x
//	flat MaxPings=6        95,640          6.96%            59.4%           1.0x
//	flat MaxPings=5        80,404          6.91%            58.9%           1.2x
//	BudgetPolicy            9,248          6.07%            51.8%           9.0x
//	flat MaxPings=2 (dflt) 33,591          5.19%            44.3%           2.1x
//
// So this is NOT the cheaper policy where pings are free: a constant beats it by 0.89 pp of the
// bill, 7.6 points of the ceiling, because the model ranks conversations well (AUC 0.93) and
// DOLLARS barely better than chance (AUC 0.70, falling to 0.57 on the largest prefixes, which
// carry 87% of the stake). What it is, is 9x more efficient PER PING, which is what matters
// where a tenant rate limit rather than money is what caps the budget.
// docs/how-to/kv-cache-keepalive-budget.md has the derivation and says plainly which to pick.
type BudgetPolicy struct {
	// Label is the name the dashboard groups by. Defaults to StrategyKeepAliveBudget.
	Label string
	// Predictor answers the reuse question. Required: with none, this arm has no opinion on
	// any observation and every budget falls back to Config.MaxPings.
	Predictor Predictor
	// Interval is the keep-alive cadence this policy assumes when it reasons about windows.
	// It MUST match the Config.PingIdle the simulator will actually ping at, or the windows
	// priced here are not the windows bought. Defaults to DefaultPingIdle.
	Interval time.Duration
	// MaxK caps the induction's horizon. Defaults to DefaultBudgetMaxK.
	MaxK int
	// MinPrefix is the prefix below which this arm does not bother pinging. A ping's fixed
	// overhead (ping_input, ping_output) does not scale with the prefix, so on a small enough
	// entry the arithmetic above is dominated by it. 0 disables the gate.
	MinPrefix int64
	// Semantics must match the Config's, because a ping's cost depends on whether the
	// provider accepts a zero-generation request.
	Semantics Semantics
}

// DefaultBudgetMaxK is how far the induction looks ahead.
//
// Eight, because at DefaultPingIdle that is ~37 minutes of holding, and on the measured
// corpus 34.3% of the stake sits on spans that return LATER than that — unreachable at any
// budget, since holding a five-minute entry that long costs more than the write it avoids.
// Looking further therefore adds cost and no reachable rescues.
const DefaultBudgetMaxK = 8

// StrategyKeepAliveBudget is this arm's default label, in the registry's hyphenated wire
// style. It is deliberately NOT in the registry: like Custom, this arm cannot be built from a
// name alone — it needs a Predictor — so a caller constructs it and passes it to Simulate,
// and the registry keeps its promise that every name in it resolves.
const StrategyKeepAliveBudget = "keepalive-budget"

func (b BudgetPolicy) Name() string {
	if b.Label != "" {
		return b.Label
	}
	return StrategyKeepAliveBudget
}

func (b BudgetPolicy) Describe() string {
	return fmt.Sprintf("A predictor's per-window reuse probabilities against the rates' own "+
		"break-even, compounded over up to %d keep-alives so the option value of staying "+
		"alive is counted. Chooses the budget per conversation.", b.maxK())
}

func (b BudgetPolicy) interval() time.Duration {
	if b.Interval > 0 {
		return b.Interval
	}
	return DefaultPingIdle
}

func (b BudgetPolicy) maxK() int {
	if b.MaxK > 0 {
		return b.MaxK
	}
	return DefaultBudgetMaxK
}

// Decide holds the prefix at five minutes, and pings only when the budget it would choose is
// at least one.
//
// It does not offer the 1-hour tier. That is not an omission: on this corpus the share of
// spans that close inside an hour but outside five minutes exceeds the share closing inside
// five minutes by 2.35 percentage points, and the 1-hour tier needs 65.22 points to break
// even against a 5-minute write. HistoricalProbability is the arm that reasons about tiers;
// this one reasons about ping counts.
func (b BudgetPolicy) Decide(o Observation) Action {
	if k, ok := b.PingBudget(o); ok && k >= 1 {
		return ActionPing5m
	}
	return ActionWrite5m
}

// PingBudget is the backward induction described on the type.
//
// It returns ok=false — "no opinion, use Config.MaxPings" — when there is no predictor, when
// the model declines the observation, when the rates are unknown so no break-even exists, or
// when the prefix is below MinPrefix. Every one of those is a case where a number invented
// here would be worse than the configured default.
func (b BudgetPolicy) PingBudget(o Observation) (int, bool) {
	if b.Predictor == nil || !o.Pricing.Known || o.CachedTokens <= 0 {
		return 0, false
	}
	if b.MinPrefix > 0 && o.CachedTokens < b.MinPrefix {
		return 0, true // a real decision: too small to be worth a ping's fixed overhead
	}
	h, s, ok := b.Windows(o)
	if !ok {
		return 0, false
	}
	rescue := float64(o.CachedTokens) * (o.Pricing.Write5m - o.Pricing.CacheRead)
	ping := o.Pricing.KeepAliveCost(o.CachedTokens, b.Semantics)
	n := b.maxK()
	// V[n] is 0: past the horizon there is nothing left to buy.
	v := make([]float64, n+1)
	for j := n - 1; j >= 0; j-- {
		v[j] = rescue*h[j] - ping + s[j]*v[j+1]
		if v[j] < 0 {
			v[j] = 0
		}
	}
	// The FIRST j that is not worth buying ends the schedule. See the type comment.
	budget := 0
	for j := 0; j < n; j++ {
		if v[j] <= 0 {
			break
		}
		budget = j + 1
	}
	return budget, true
}

// Windows converts the Predictor's CUMULATIVE answers into the two per-sweep quantities the
// induction needs, both conditional on the conversation still being idle when the ping fires.
//
// Exported for one reason: deploy/harbor/kv_ttl_keepalive_drift_test.go pins these two vectors
// against the Python port alongside the budget itself, because a budget that agrees for the
// wrong reason — a hazard scaled wrong and a survival scaled inversely wrong — is a guard that
// has already stopped working. It is also the honest way to inspect why the policy chose what
// it chose. ok=false where the predictor declines, matching PingBudget.
//
// Ping j (1-based) fires at j×interval after o.Now and refreshes the entry to
// j×interval+lifetime, so the window it and it alone protects is
//
//	(deadline standing before it, j×interval + lifetime]
//
// where that deadline is lifetime for the first ping and (j−1)×interval+lifetime after. Then
// with F(x) = ReuseProbability(o, x), the CONDITIONAL quantities are
//
//	h_j = (F(t_j + life) − F(d_j)) / (1 − F(t_j))     this ping rescues
//	s_j = (1 − F(t_(j+1)))       / (1 − F(t_j))       reaches the next sweep
//
// Dividing by the survivor share is the whole reason this is not just a lookup: the decision
// at sweep j is only ever faced by a conversation that has ALREADY stayed idle that long, so
// an unconditional probability answers a question nobody asks. A predictor whose F is not
// monotone would produce a negative h; that is clamped rather than trusted.
func (b BudgetPolicy) Windows(o Observation) (h, s []float64, ok bool) {
	n := b.maxK()
	iv := b.interval()
	life := TTL5m.Lifetime()
	h, s = make([]float64, n), make([]float64, n)
	for j := 1; j <= n; j++ {
		tj := time.Duration(j) * iv
		alive, aok := b.Predictor.ReuseProbability(o, tj)
		if !aok {
			return nil, nil, false
		}
		survive := 1 - alive
		if survive <= 0 {
			// Certain to have returned by t_j, so no later ping can be needed. Everything from
			// here on is worth nothing, which the zeros already say.
			return h, s, true
		}
		deadline := life
		if j > 1 {
			deadline = time.Duration(j-1)*iv + life
		}
		fEnd, e1 := b.Predictor.ReuseProbability(o, tj+life)
		fStart, e2 := b.Predictor.ReuseProbability(o, deadline)
		fNext, e3 := b.Predictor.ReuseProbability(o, tj+iv)
		if !e1 || !e2 || !e3 {
			return nil, nil, false
		}
		if hj := (fEnd - fStart) / survive; hj > 0 {
			h[j-1] = min(hj, 1)
		}
		if sj := (1 - fNext) / survive; sj > 0 {
			s[j-1] = min(sj, 1)
		}
	}
	return h, s, true
}
