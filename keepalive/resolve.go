package keepalive

import (
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/tenant"
)

// CampaignDecision is what a learned campaign tells the resolver about one finished response.
// The zero value means "no decision". The host scores the campaign and fills it in. The resolver
// only reads it.
type CampaignDecision struct {
	// Active is true when an ACTIVE-mode campaign scored this response (not shadow mode, not
	// opted out).
	Active bool
	// Tag names the campaign on the ping rows. Read only when Active.
	Tag string
	// Apply writes the campaign's ping count onto a policy. Read only when Active.
	Apply func(Policy) Policy
}

// ResolveInput is everything Resolve reads besides the account's own policy.
type ResolveInput struct {
	TenantID string
	// OpenAI is true for an OpenAI-family request. Strategies and campaigns are Anthropic only.
	OpenAI bool
	Now    time.Time
	// Strategies is the manager strategy list (a snapshot the host swaps wholesale under its lock).
	Strategies []tenant.Strategy
	// Campaign is the response's campaign decision, or the zero value when there is none (a
	// record-time gate such as canPing has no response yet).
	Campaign CampaignDecision
	// IgnoreWindows ignores a strategy's time windows (#858). Windows describe when a person
	// answers, so they gate only a wait for a person.
	IgnoreWindows bool
	// Override is the session's armed override that is still in force, or nil.
	Override *Override
}

// Resolve layers the manager strategy, an active campaign and the session override over the
// account's own policy, and returns the policy plus the tag of the strategy or campaign that
// applied ("" when neither did).
//
// Order, each layer reading the one below it:
//  1. Account config (pol).
//  2. The matching manager strategy. Anthropic only. With IgnoreWindows and no match today, a
//     strategy that matches only because its window was ignored applies, unless an active campaign
//     would own the count (with a campaign and a closed window, the campaign wins).
//  3. An ACTIVE campaign decides the ping count unless a strategy matched. The campaign owns the
//     count, so the per-wait plan is off for it (#858).
//  4. The session override. An override may switch the mechanism ON for a session whose account
//     leaves it off, since arming is the consent act. It may not widen MaxUSDPerPing, and it cannot
//     reach around the kill switch or the no-audit-sink refusal, which the host checks first.
//
// For OpenAI the strategy and campaign layers do not apply: the 60-290 s strategy band would buy
// 6-28 useless pings against a 30-minute cache. The idle is the OpenAI idle (or its default), and
// an override applies through ApplyOpenAI.
func Resolve(pol Policy, in ResolveInput) (Policy, string) {
	applied := ""
	if !in.OpenAI {
		account := pol
		best := BestStrategy(in.Strategies, in.TenantID, in.Now, false)
		pol, applied = ApplyStrategy(account, best)
		if applied == "" && in.IgnoreWindows && !in.Campaign.Active {
			pol, applied = ApplyStrategy(account, BestStrategy(in.Strategies, in.TenantID, in.Now, true))
		}
		if applied == "" && in.Campaign.Active && in.Campaign.Apply != nil {
			pol, applied = in.Campaign.Apply(pol), in.Campaign.Tag
			pol.WaitPlan = false
		}
		if in.Override != nil {
			pol = in.Override.Apply(pol)
		}
		return pol, applied
	}
	pol.Idle = pol.OpenAIIdle
	if pol.Idle <= 0 {
		pol.Idle = modelinfo.OpenAIDefaultKeepAliveIdle
	}
	if in.Override != nil {
		pol = in.Override.ApplyOpenAI(pol)
	}
	return pol, applied
}

// WaitPlanOnly is the part of an account's policy that rides on EVERY policy, a parked one too:
// the per-wait ping plan (#858) and its guards. A manager strategy can switch the keep-alive on
// for an account whose own block has it off, and the plan then still has to apply. These fields
// alone never turn the keep-alive on (Policy.On reads KeepAlive).
func (p Policy) WaitPlanOnly() Policy {
	return Policy{WaitPlan: p.WaitPlan, ToolWaitMaxPings: p.ToolWaitMaxPings, ToolWaitMinPrefix: p.ToolWaitMinPrefix,
		MinPrefixTokens: p.MinPrefixTokens, BackgroundExtraPings: p.BackgroundExtraPings,
		HumanExtraPings: p.HumanExtraPings, KeepAlive: p.KeepAlive}
}

// EffectiveWaitPlan is an account's EFFECTIVE per-wait ping plan (#868), the policy a calculator
// replays. It follows Resolve's order and reports the zero policy (the fixed-K model) when Resolve
// would turn the plan off:
//
//  1. a covering manager strategy wins first, found as the request path finds it for a wait that is
//     not for a person (windows ignored, unless an active campaign is on). Its Idle decides: 300 s
//     or more (the 1-hour band) turns the plan off, less keeps the account's own setting. The
//     campaign is then skipped;
//  2. only when no strategy covers the account, an active campaign owns the count and turns the
//     plan off.
//
// Limits, stated because one read cannot see a single request: a campaign owns the count only on a
// request that passes its price gate; a per-session override turns the plan off for that session
// only; and the plan flag is one value for the account.
func EffectiveWaitPlan(account Policy, strategies []tenant.Strategy, tenantID string, now time.Time,
	campaignOn bool) Policy {
	pol := account.WaitPlanOnly()
	if best := BestStrategy(strategies, tenantID, now, !campaignOn); best != nil {
		if time.Duration(best.IdleSeconds)*time.Second >= OneHourIdleFloor {
			return Policy{}
		}
		return pol
	}
	if campaignOn {
		return Policy{}
	}
	return pol
}

// CanKeepAlive is the record-time gate on a resolved policy: the policy is on, and a thread with no
// tool call yet is not pinged (#858) when the per-wait plan is on, so it keeps no copy either.
// toolSeen is read only when the plan is on. The host checks the kill switch, the audit sink, the
// provider and the session first.
func CanKeepAlive(pol Policy, toolSeen func() bool) bool {
	if !pol.On() {
		return false
	}
	return !(pol.WaitPlan && !toolSeen())
}
