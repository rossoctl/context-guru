package campaign

import (
	"time"

	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/context-guru/kvcache"
	"github.com/rossoctl/context-guru/predictive"
)

// Choose is the ping count and the spacing a campaign gives one finished response. The segment rule of spec for the response's facts decides:
// a model rule asks the model (its own spacing), a plan rule reads the menu, and any other rule means no pings at the spacing the decision is
// scored at (kvcache.DefaultPingIdle). row is the model's feature row, nil for a plan-only campaign.
func Choose(spec *predictive.Spec, model predictive.Model, row *predictive.Row, facts *predictive.Facts, econ predictive.Econ) (k int, idleS float64) {
	rule := spec.RuleFor(facts)
	k, idleS = 0, kvcache.DefaultPingIdle.Seconds()
	switch rule.Kind {
	case predictive.RuleModel:
		d := predictive.Decide(model, row, econ)
		k, idleS = d.K, model.Rule().XS
	case predictive.RulePlan:
		k, idleS = spec.PlanOf(rule)
	}
	return k, idleS
}

// Apply writes an ACTIVE campaign's decision (k pings) into the entry's policy. Account-level bounds (the per-ping dollar ceiling) are left alone.
// A campaign that is not active (shadow, off, or the tenant opted out) leaves the policy as it is.
func Apply(pol keepalive.Policy, k int, active bool) keepalive.Policy {
	if !active {
		return pol
	}
	if k <= 0 {
		pol.KeepAlive = false
		return pol
	}
	// The campaign decides HOW MANY pings, never their spacing: that stays the tenant's configured keep-alive interval (the shipped 240 s when none
	// is configured), so a campaign never pings later than it (live, pings 275-280 s after a response miss the cache 19% of the time) nor more
	// often than the tenant's own policy would.
	idle := pol.Idle
	if idle <= 0 {
		idle = config.DefaultKeepAliveIdle * time.Second
	}
	pol.KeepAlive, pol.Idle, pol.MaxPings = true, idle, k
	// A campaign is scored on five-minute economics (Cliff, PWrite5m), so its k is a count of
	// five-minute windows. On a thread billed at the one-hour tier the keeper swaps in the one-hour
	// schedule (Policy.ForOneHour); k then only bounds it from above, as an override's does.
	k1h := pol.MaxPings1h
	if k1h <= 0 {
		k1h = modelinfo.AnthropicDefaultKeepAliveMaxPings1h
	}
	pol.MaxPings1h = min(k1h, k)
	pol.MinPrefixTokens, pol.PredictorID, pol.PredictorThreshold = 0, "", 0
	return pol
}
