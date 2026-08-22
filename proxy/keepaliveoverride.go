package proxy

import (
	"fmt"
	"strings"
	"time"
)

// Per-session keep-alive overrides: a human pointing at one session and saying "keep this
// one warm for the next hour".
//
// # Why this is an override and not a strategy
//
// The obvious feature here is a learned session allowlist, and it does not work. Fitted on the
// first half of the production window and scored on the second it earned $5.27 where the plain
// account-wide gate earned $53.32, and only 3 of the top-20 first-half sessions have any
// request in the second half at all. Sessions are ephemeral — lifetime p50 is about zero hours
// and p99 is 12.9 — so a list of "sessions worth pinging" is a description of the past. The UI
// says so above the table, in those numbers.
//
// So this is deliberately a MANUAL, SCOPED, EXPIRING act: a mandatory `until`, capped at 12 h
// because that is where session lifetime runs out, and an audit row per arm and per disarm.
//
// # Why it lives in memory and not in a table
//
// Three reasons, and the third is the deciding one. The dash database may not grow unboundedly
// and a schema bump renames five days of production history aside; the tenant registry would
// need its own migration; and an override is an AUTHORIZATION TO SPEND, so one that silently
// survives a restart is worse than one that does not. The cost is that overrides are lost on
// restart, which the arm dialog and the armed list both say. The durable half is the audit row:
// who authorized what spend, with which parameters, until when, is permanent even though the
// live policy is not.
//
// # Why it holds no material
//
// An override is POLICY, not state. Arming creates no kaEntry, holds no request body and no
// credential, and retains nothing. Material is held only when the session actually sends a
// request, which is why an override left on a dead session costs exactly zero. It disappears at
// `until`, at a restart, or on disarm.

// Override bounds. Every one of them is a refusal with a reason rather than a silent clamp: a
// clamp on a spend authorization tells the person they got what they asked for when they did not.
const (
	// maxOverrideUntil caps how far ahead an override may reach. Session lifetime p99 is
	// 12.9 h, so past this the authorization is being spent on a session that statistically no
	// longer exists.
	maxOverrideUntil = 12 * time.Hour
	// defaultOverrideUntil is what the dialog offers.
	defaultOverrideUntil = time.Hour
	// The idle interval's band. Below 60 s a ping lands inside the previous one's own refresh
	// for no reason; above 290 s the first ping arrives after a 300 s lifetime has already
	// lapsed and pays a 1.25x WRITE — the exact charge the mechanism exists to avoid.
	minOverrideIdle = 60 * time.Second
	maxOverrideIdle = 290 * time.Second
	// The ping-count band. The upper end is bounded again, and more tightly, by the credential
	// hold below.
	minOverridePings = 1
	maxOverridePings = 11
	// maxOverrideHold is the operator's ceiling on the CREDENTIAL HOLD an override may buy.
	// The hard deadline is (K+1)x X (see kaEntry.timer), and the Settings copy promises "up to
	// about 14 minutes" at the shipped defaults — an override changes that number, so the arm
	// dialog states the computed hold and this refuses anything past an hour.
	maxOverrideHold = time.Hour
	// Bounds on the map itself. Same order as maxKeepAliveSessions, and both are needed for
	// the same reason that bound has two: many accounts with a few, or one account with many.
	maxOverridesPerTenant = 64
	maxOverridesTotal     = 512
	// maxSessionIDBytes refuses an implausible session id before it is used as a map key.
	maxSessionIDBytes = 200
	// overrideArmsPerHour is the arm rate per account. Arming is a spend authorization, so it
	// is rate-limited like one.
	overrideArmsPerHour = 30
)

// sessionOverride is one armed session's policy and its expiry.
type sessionOverride struct {
	// pol carries Idle, MaxPings and MinPrefixTokens ONLY. MaxUSDPerPing is deliberately not
	// here: the per-ping cost guard is the one control that exists because ping cost is bimodal
	// (p50 $0.0004, p99 $0.2275, max $0.3780), and an override may not widen it. KeepAlive is
	// forced on — enabling the mechanism for one session while the account default is off is
	// the point of the feature, and the arming request is itself the consent act.
	pol   CachePolicy
	until time.Time
	armed time.Time
	// by is the tenant id of the principal that armed it, for the audit row. Always the
	// authenticated principal, never a value out of a request body.
	by string
}

// hold is the credential-hold ceiling this override buys: (K+1) x Idle, the same arithmetic
// kaEntry.timer uses. Named because it is a number a person is shown before they authorize it.
func (o sessionOverride) hold() time.Duration {
	return time.Duration(o.pol.MaxPings+1) * o.pol.Idle
}

// validOverride checks a proposed override against every bound, naming the one it fails.
//
// Returns the resolved override, so the caller cannot apply a different one from the one that
// was checked. `now` is passed in rather than read, for the same reason the keeper's clock is.
func validOverride(session string, idle time.Duration, pings, minPrefix int,
	until, now time.Time, by string) (sessionOverride, error) {
	if session == "" {
		return sessionOverride{}, fmt.Errorf("name the session to arm")
	}
	if len(session) > maxSessionIDBytes {
		return sessionOverride{}, fmt.Errorf("that is not a session id")
	}
	if strings.ContainsRune(session, 0) {
		return sessionOverride{}, fmt.Errorf("that is not a session id")
	}
	if until.IsZero() {
		return sessionOverride{}, fmt.Errorf("an override must state when it expires")
	}
	if !until.After(now) {
		return sessionOverride{}, fmt.Errorf("that expiry has already passed")
	}
	if until.Sub(now) > maxOverrideUntil {
		return sessionOverride{}, fmt.Errorf(
			"an override may last at most %d hours; sessions do not live longer than that",
			int(maxOverrideUntil/time.Hour))
	}
	if idle < minOverrideIdle || idle > maxOverrideIdle {
		return sessionOverride{}, fmt.Errorf(
			"the idle interval must be between %d and %d seconds; past %d the first ping "+
				"arrives after the provider's 5-minute lifetime has already lapsed and pays "+
				"a cache WRITE instead of a read",
			int(minOverrideIdle.Seconds()), int(maxOverrideIdle.Seconds()),
			int(maxOverrideIdle.Seconds()))
	}
	if pings < minOverridePings || pings > maxOverridePings {
		return sessionOverride{}, fmt.Errorf("the ping count must be between %d and %d",
			minOverridePings, maxOverridePings)
	}
	if minPrefix < 0 {
		return sessionOverride{}, fmt.Errorf("the prefix floor cannot be negative")
	}
	o := sessionOverride{
		pol:   CachePolicy{KeepAlive: true, Idle: idle, MaxPings: pings, MinPrefixTokens: minPrefix},
		until: until, armed: now, by: by,
	}
	if h := o.hold(); h > maxOverrideHold {
		return sessionOverride{}, fmt.Errorf(
			"%d pings %ds apart would hold your credential for up to %d minutes; the ceiling "+
				"is %d minutes, so lower one of them",
			pings, int(idle.Seconds()), int(h.Minutes()), int(maxOverrideHold.Minutes()))
	}
	return o, nil
}

// overrideFor resolves one session's effective policy: the account's own, unless an unexpired
// override is armed for it.
//
// The ONE hook this feature has in the request path, called from record. Everything downstream
// — pol.on(), pingable(), the hard deadline, the per-ping cost guard, the kill switch, the
// no-audit-sink refusal — reads the returned policy and is otherwise untouched.
func (k *keeper) overrideFor(tenantID, session string, pol CachePolicy) CachePolicy {
	if k == nil || session == "" {
		return pol
	}
	k.mu.Lock()
	o, ok := k.overrides[kaKey(tenantID, session)]
	k.mu.Unlock()
	if !ok || !k.now().Before(o.until) {
		return pol
	}
	// The three fields an override may move. MaxUSDPerPing stays the ACCOUNT's — see
	// sessionOverride.pol — and so do the head-TTL fields, which are a different mechanism.
	pol.KeepAlive = true
	pol.Idle, pol.MaxPings = o.pol.Idle, o.pol.MaxPings
	pol.MinPrefixTokens = o.pol.MinPrefixTokens
	return pol
}

// arm installs an override, refusing when a bound would be crossed.
//
// Refuses outright under the two global refusals, which an override does not get to reach
// around: the operator kill switch and a deployment with no audit sink. Both are the reasons
// retention is defensible at all, and a per-session opt-in that could bypass them would be a
// hole in the consent story rather than a feature.
func (k *keeper) arm(tenantID, session string, o sessionOverride) error {
	if k == nil {
		return fmt.Errorf("the keep-alive is not running on this deployment")
	}
	if keepAliveDisabled() {
		return fmt.Errorf("the keep-alive is switched off service-wide by the operator")
	}
	if k.h == nil || k.h.rec == nil {
		return fmt.Errorf("this deployment records no audit trail, so nothing may be armed")
	}
	key := kaKey(tenantID, session)
	prefix := tenantID + "\x00"
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.overrides == nil {
		k.overrides = map[string]sessionOverride{}
	}
	k.expireOverridesLocked(k.now())
	if _, replacing := k.overrides[key]; !replacing {
		mine := 0
		for other := range k.overrides {
			if strings.HasPrefix(other, prefix) {
				mine++
			}
		}
		if mine >= maxOverridesPerTenant {
			return fmt.Errorf("you already have %d sessions armed, which is the limit; "+
				"disarm one first", maxOverridesPerTenant)
		}
		if len(k.overrides) >= maxOverridesTotal {
			return fmt.Errorf("this service has %d sessions armed, which is the limit; "+
				"try again shortly", maxOverridesTotal)
		}
	}
	k.overrides[key] = o
	return nil
}

// disarm removes an override AND releases anything already held for that session, rather than
// waiting for the hard deadline. Reports whether there was one.
//
// The release is the point: withdrawing an authorization to hold a credential has to stop the
// hold now. `retire` zeroizes the body and the credential and cancels the deadline.
func (k *keeper) disarm(tenantID, session string) bool {
	if k == nil {
		return false
	}
	key := kaKey(tenantID, session)
	k.mu.Lock()
	_, had := k.overrides[key]
	delete(k.overrides, key)
	k.mu.Unlock()
	// Outside the lock: retire takes it itself.
	k.retire(key)
	return had
}

// armedOverride is one row of "what is armed right now" — the live map, not a stored intention.
type armedOverride struct {
	Session     string  `json:"session"`
	IdleSeconds int     `json:"idle_seconds"`
	MaxPings    int     `json:"max_pings"`
	MinPrefix   int     `json:"min_prefix_tokens"`
	Until       int64   `json:"until"`
	Armed       int64   `json:"armed"`
	HoldMinutes float64 `json:"hold_minutes"`
}

// armedFor lists one tenant's live overrides, expired ones excluded.
func (k *keeper) armedFor(tenantID string) []armedOverride {
	out := []armedOverride{}
	if k == nil {
		return out
	}
	prefix := tenantID + "\x00"
	now := k.now()
	k.mu.Lock()
	defer k.mu.Unlock()
	k.expireOverridesLocked(now)
	for key, o := range k.overrides {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		out = append(out, armedOverride{
			Session: strings.TrimPrefix(key, prefix), IdleSeconds: int(o.pol.Idle.Seconds()),
			MaxPings: o.pol.MaxPings, MinPrefix: o.pol.MinPrefixTokens,
			Until: o.until.UnixMilli(), Armed: o.armed.UnixMilli(),
			HoldMinutes: o.hold().Minutes(),
		})
	}
	return out
}

// forgetOverrides drops every override for one tenant. Called from forget, so unticking the
// account-wide box kills armed sessions too — a consent withdrawal that left per-session
// authorizations running would be the setting not working.
func (k *keeper) forgetOverrides(tenantID string) {
	prefix := tenantID + "\x00"
	for key := range k.overrides {
		if strings.HasPrefix(key, prefix) {
			delete(k.overrides, key)
		}
	}
}

// expireOverridesLocked deletes what has expired, so the map cannot grow on the strength of
// authorizations nobody withdrew. Called from the 2 s sweep and from every read, so an expired
// override is invisible even between sweeps.
func (k *keeper) expireOverridesLocked(now time.Time) {
	for key, o := range k.overrides {
		if !now.Before(o.until) {
			delete(k.overrides, key)
		}
	}
}
