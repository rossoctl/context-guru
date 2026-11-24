package keepalive

import (
	"fmt"
	"strings"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// Per-session keep-alive overrides: a human pointing at one session and saying "keep this one
// warm for the next hour". This file holds the policy half: the bounds, the validation and the way
// an armed override changes a Policy. The host keeps the map of armed overrides, its lock, the
// arm rate limit and the control routes.
//
// An override is a MANUAL, SCOPED, EXPIRING act: a mandatory `until`, capped at 12 h because that
// is where session lifetime runs out (p99 is 12.9 h). It lives in memory only, because an
// authorization to spend that survives a restart silently is worse than one that does not. It is
// POLICY, not state: arming holds no request body and no credential. The durable half is the audit
// row.

// Override bounds. Every one is a refusal with a reason, not a silent clamp: a clamp on a spend
// authorization tells the person they got what they asked for when they did not.
const (
	// MaxOverrideUntil caps how far ahead an override may reach.
	MaxOverrideUntil = 12 * time.Hour
	// DefaultOverrideUntil is what the dialog offers.
	DefaultOverrideUntil = time.Hour
	// The idle interval's band. Below 60 s a ping lands inside the previous one's own refresh.
	// Above MaxOverrideIdle the first ping arrives after the 5-minute lifetime has lapsed and pays
	// a 1.25x WRITE, the charge the mechanism exists to avoid.
	MinOverrideIdle = 60 * time.Second
	MaxOverrideIdle = modelinfo.AnthropicCacheLifetime5m - 10*time.Second
	// OneHourIdleFloor is the Idle at or above which a policy is an EXPLICIT 1-hour schedule (a
	// strategy with HeadTTL1h): no 5-minute Idle can reach it, because MaxOverrideIdle refuses it.
	// Such a policy does not take the per-wait plan.
	OneHourIdleFloor = 300 * time.Second
	// The ping-count band. The upper end is bounded again, and more tightly, by MaxOverrideHold.
	MinOverridePings = 1
	MaxOverridePings = 11
	// MaxOverrideHold is the operator's ceiling on the CREDENTIAL HOLD an override may buy. The hard
	// deadline is (K+1) x X. It is currently unreachable: the two caps above bound (K+1) x X at
	// 12 x 290 s = 58 minutes. It stays as the bound that keeps holding if either is widened.
	MaxOverrideHold = time.Hour
	// MaxOverridesPerTenant and MaxOverridesTotal bound the host's map: many accounts with a few,
	// or one account with many.
	MaxOverridesPerTenant = 64
	MaxOverridesTotal     = 512
	// MaxSessionIDBytes refuses an implausible session id before it is used as a map key.
	MaxSessionIDBytes = 200
	// OverrideArmsPerHour is the arm rate per account. Arming is a spend authorization, so it is
	// rate-limited like one.
	OverrideArmsPerHour = 30
)

// Override is one armed session's policy and its expiry.
type Override struct {
	// Pol carries Idle, MaxPings and MinPrefixTokens ONLY. MaxUSDPerPing is deliberately not here:
	// the per-ping cost guard exists because ping cost is bimodal (p50 $0.0004, p99 $0.2275, max
	// $0.3780), and an override may not widen it. KeepAlive is forced on: enabling the mechanism for
	// one session is the point, and the arming request is the consent act.
	Pol   Policy
	Until time.Time
	Armed time.Time
	// By is the tenant id of the principal that armed it, for the audit row. Always the
	// authenticated principal, never a value out of a request body.
	By string
}

// Hold is the credential-hold ceiling this override buys: (K+1) x Idle, the arithmetic the entry's
// deadline uses. A person is shown it before they authorize.
func (o Override) Hold() time.Duration {
	return time.Duration(o.Pol.MaxPings+1) * o.Pol.Idle
}

// Active reports whether the override is still in force at now.
func (o Override) Active(now time.Time) bool { return now.Before(o.Until) }

// ValidOverride checks a proposed override against every bound, naming the one it fails. It
// returns the resolved override, so the caller cannot apply a different one from the one checked.
// now is passed in, like the keeper's clock.
func ValidOverride(session string, idle time.Duration, pings, minPrefix int,
	until, now time.Time, by string) (Override, error) {
	if session == "" {
		return Override{}, fmt.Errorf("name the session to arm")
	}
	if len(session) > MaxSessionIDBytes {
		return Override{}, fmt.Errorf("that is not a session id")
	}
	if strings.ContainsRune(session, 0) {
		return Override{}, fmt.Errorf("that is not a session id")
	}
	// A session id whose `tenant:base` prefix names SOMEBODY ELSE is refused. Overrides are keyed
	// under the authenticated principal, so a foreign id would become a key no request matches:
	// arming it would do nothing and return 200. A manager's service-wide session list is full of
	// these rows, so this is reachable by clicking.
	if t, _, ok := strings.Cut(session, ":"); ok && t != by {
		return Override{}, fmt.Errorf(
			"that session belongs to another account. An override is keyed to the principal that " +
				"arms it, so this one would match no request and keep nothing warm")
	}
	if until.IsZero() {
		return Override{}, fmt.Errorf("an override must state when it expires")
	}
	if !until.After(now) {
		return Override{}, fmt.Errorf("that expiry has already passed")
	}
	if until.Sub(now) > MaxOverrideUntil {
		return Override{}, fmt.Errorf(
			"an override may last at most %d hours; sessions do not live longer than that",
			int(MaxOverrideUntil/time.Hour))
	}
	if idle < MinOverrideIdle || idle > MaxOverrideIdle {
		return Override{}, fmt.Errorf(
			"the idle interval must be between %d and %d seconds; past %d the first ping "+
				"arrives after the provider's 5-minute lifetime has already lapsed and pays "+
				"a cache WRITE instead of a read",
			int(MinOverrideIdle.Seconds()), int(MaxOverrideIdle.Seconds()),
			int(MaxOverrideIdle.Seconds()))
	}
	if pings < MinOverridePings || pings > MaxOverridePings {
		return Override{}, fmt.Errorf("the ping count must be between %d and %d",
			MinOverridePings, MaxOverridePings)
	}
	if minPrefix < 0 {
		return Override{}, fmt.Errorf("the prefix floor cannot be negative")
	}
	o := Override{
		Pol:   Policy{KeepAlive: true, Idle: idle, MaxPings: pings, MinPrefixTokens: minPrefix},
		Until: until, Armed: now, By: by,
	}
	if h := o.Hold(); h > MaxOverrideHold {
		return Override{}, fmt.Errorf(
			"%d pings %ds apart would hold your credential for up to %d minutes; the ceiling "+
				"is %d minutes, so lower one of them",
			pings, int(idle.Seconds()), int(h.Minutes()), int(MaxOverrideHold.Minutes()))
	}
	return o, nil
}

// Apply writes an armed override onto an Anthropic policy. Everything downstream (On, the
// pingable check, the hard deadline, the per-ping cost guard) reads the returned policy.
func (o Override) Apply(pol Policy) Policy {
	pol.KeepAlive = true
	pol.Idle, pol.MaxPings = o.Pol.Idle, o.Pol.MaxPings
	pol.MinPrefixTokens = o.Pol.MinPrefixTokens
	// The person armed this exact count and was shown the hold it buys: the per-wait plan (#858)
	// does not change it.
	pol.WaitPlan = false
	// The override's ping count bounds a 1-hour thread too, but only downward: its 60-290 s band
	// cannot express an hour, so the 1-hour timing stays the account's, and the hold the person was
	// shown is then (K+1) x 59 min at most. 0 means the proxy default, resolved here so that the
	// minimum is taken against a real number.
	k1h := pol.MaxPings1h
	if k1h <= 0 {
		k1h = modelinfo.AnthropicDefaultKeepAliveMaxPings1h
	}
	pol.MaxPings1h = min(k1h, o.Pol.MaxPings)
	return pol
}

// ApplyOpenAI writes an armed override onto an OpenAI policy. It enables the session and sets its
// floor, but never imports the Anthropic-only 60-290 s cadence. The actual credential hold is
// bounded to one hour even when an account configured a longer OpenAI interval or many pings.
func (o Override) ApplyOpenAI(pol Policy) Policy {
	pol.KeepAlive = true
	pol.MinPrefixTokens = o.Pol.MinPrefixTokens
	if pol.Idle <= 0 || pol.Idle > modelinfo.OpenAIDefaultKeepAliveIdle {
		pol.Idle = modelinfo.OpenAIDefaultKeepAliveIdle
	}
	maxPings := int(MaxOverrideHold/pol.Idle) - 1
	pol.MaxPings = min(o.Pol.MaxPings, maxPings)
	return pol
}

// WorstCasePings is the most pings one armed override can send before it expires: time until expiry
// over the idle interval, at least 1. It is a CEILING and not an estimate.
//
// The count is `time until expiry / X`, one ping per idle interval, and NOT `K pings per (K+1)X
// span`. A span ends the instant a real request arrives, so the worst case is a session whose
// requests land just after each ping, restarting the clock every time and sending one ping per X
// for the whole hold. The span form under-states by (K+1)/K: 8 against a true 12 at the shipped
// defaults, and 2x at K=1. Under-stating a ceiling on a spend authorization is the one direction
// that cannot be defended. A zero idle gives 0 (nothing can be computed).
func (o Override) WorstCasePings(now time.Time) int64 {
	if o.Pol.Idle <= 0 {
		return 0
	}
	pings := int64(o.Until.Sub(now).Seconds() / o.Pol.Idle.Seconds())
	if pings < 1 {
		pings = 1
	}
	return pings
}

// PingUSD is the worst-case cost of one ping on a prefix of prefixTokens tokens: a cache read of the
// prefix plus the output of the ping. Per-ping cost is bimodal, so it is priced from the session's
// own last billed prefix and its own model's rates, never a service-wide average.
func PingUSD(prefixTokens int64, cacheReadUSDPerToken, outputUSD float64) float64 {
	return float64(prefixTokens)*cacheReadUSDPerToken + outputUSD
}
