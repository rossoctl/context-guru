package keepalive

import (
	"time"

	"github.com/rossoctl/context-guru/config"
)

// FromConfig maps the configuration document's `cache:` block onto a Policy. Every host that reads
// the document builds its account policy here, so they all resolve the same defaults.
func FromConfig(c config.CacheConfig) Policy {
	r := c.Resolved()
	// The per-wait ping plan (#858) rides on EVERY policy, a parked one too, because a manager
	// strategy can switch the keep-alive on for an account whose own block has it off, and the plan
	// then still has to apply. These fields alone never turn the keep-alive on (Policy.On reads
	// KeepAlive).
	plan := Policy{
		WaitPlan:             c.WaitPlanOn(),
		ToolWaitMaxPings:     r.KeepAliveToolWaitMaxPings,
		ToolWaitMinPrefix:    r.KeepAliveToolWaitMinPrefixTokens,
		BackgroundExtraPings: r.KeepAliveBackgroundExtraPings,
		HumanExtraPings:      r.KeepAliveHumanExtraPings,
		// A memory guard, like the plan fields: a manual hold or a manager strategy can arm a hold
		// on a parked account.
		MaxBodyBytes: r.KeepAliveMaxBodyBytes,
		// D's threshold rides the parked plan too. It is a threshold, not a switch: alone it turns
		// nothing on. A manager strategy that switches 1-hour-on-wait on for this account must still
		// find the account's own choice here, a negative value that keeps D off (a 0 would read as
		// the 150,000 default).
		OneHourRebuildTokens: r.KeepAliveOneHourRebuildTokens,
	}
	// Resolve the defaults only when something is actually switched on. A parked block
	// (`keepalive: false` with a tuned interval) must not come out looking enabled, and a zero
	// HeadTTLMinBytes must stay zero so a host that never configured this asks for nothing rather
	// than asking on every request.
	if !c.KeepAlive && !c.HeadTTL1h && !c.KeepAliveOneHourAlways {
		return plan
	}
	return Policy{
		WaitPlan:             plan.WaitPlan,
		OneHourOnWait:        c.KeepAliveOneHourOnWait,
		OneHourRebuildTokens: r.KeepAliveOneHourRebuildTokens,
		OneHourAlways:        c.KeepAliveOneHourAlways,
		ToolWaitMaxPings:     plan.ToolWaitMaxPings,
		ToolWaitMinPrefix:    plan.ToolWaitMinPrefix,
		BackgroundExtraPings: plan.BackgroundExtraPings,
		HumanExtraPings:      plan.HumanExtraPings,
		KeepAlive:            c.KeepAlive,
		Idle:                 time.Duration(r.KeepAliveIdleSeconds) * time.Second,
		OpenAIIdle:           time.Duration(r.KeepAliveOpenAIIdleSeconds) * time.Second,
		MaxPings:             r.KeepAliveMaxPings,
		Idle1h:               time.Duration(r.KeepAliveIdleSeconds1h) * time.Second,
		MaxPings1h:           r.KeepAliveMaxPings1h,
		MaxUSDPerPing:        r.KeepAliveMaxUSDPerPing,
		MaxBodyBytes:         r.KeepAliveMaxBodyBytes,
		MinPrefixTokens:      r.KeepAliveMinPrefixTokens,
		HeadTTL1h:            c.HeadTTL1h,
		HeadTTLMinBytes:      headTTLMin(c.HeadTTL1h, r.HeadTTLMinBytes),
	}
}

// headTTLMin returns the size gate only when the policy is on, so `HeadTTLMinBytes > 0` is the
// single condition apply has to test.
func headTTLMin(on bool, n int) int {
	if !on {
		return 0
	}
	return n
}
