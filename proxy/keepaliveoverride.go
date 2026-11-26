package proxy

import (
	"fmt"
	"strings"
	"time"

	"github.com/rossoctl/context-guru/keepalive"
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

// The override rules (bounds, validation, how an armed override changes a policy) live in package
// keepalive (keepalive/override.go), so every host resolves the same policy. This file keeps the
// host half: the map of armed overrides, its lock, the arm limits and the credential release. The
// names below keep the old unexported spelling for this package and its tests.
const (
	maxOverrideUntil      = keepalive.MaxOverrideUntil
	defaultOverrideUntil  = keepalive.DefaultOverrideUntil
	minOverrideIdle       = keepalive.MinOverrideIdle
	maxOverrideIdle       = keepalive.MaxOverrideIdle
	minOverridePings      = keepalive.MinOverridePings
	maxOverridePings      = keepalive.MaxOverridePings
	maxOverrideHold       = keepalive.MaxOverrideHold
	maxOverridesPerTenant = keepalive.MaxOverridesPerTenant
	maxOverridesTotal     = keepalive.MaxOverridesTotal
	maxSessionIDBytes     = keepalive.MaxSessionIDBytes
	overrideArmsPerHour   = keepalive.OverrideArmsPerHour
)

// sessionOverride is keepalive.Override under this package's old field names. The fields are
// copies, and the rules read them through lib().
type sessionOverride struct {
	pol   CachePolicy
	until time.Time
	armed time.Time
	by    string
}

func fromLib(o keepalive.Override) sessionOverride {
	return sessionOverride{pol: o.Pol, until: o.Until, armed: o.Armed, by: o.By}
}

func (o sessionOverride) lib() keepalive.Override {
	return keepalive.Override{Pol: o.pol, Until: o.until, Armed: o.armed, By: o.by}
}

// hold is the credential-hold ceiling this override buys (keepalive.Override.Hold).
func (o sessionOverride) hold() time.Duration { return o.lib().Hold() }

// validOverride is keepalive.ValidOverride.
func validOverride(session string, idle time.Duration, pings, minPrefix int,
	until, now time.Time, by string) (sessionOverride, error) {
	o, err := keepalive.ValidOverride(session, idle, pings, minPrefix, until, now, by)
	return fromLib(o), err
}

// overrideFor resolves one session's effective policy: the account's own, unless an unexpired
// override is armed for it (keepalive.Override.Apply).
func (k *keeper) overrideFor(tenantID, session string, pol CachePolicy) CachePolicy {
	o, ok := k.activeOverride(tenantID, session)
	if !ok {
		return pol
	}
	return o.lib().Apply(pol)
}

// overrideForOpenAI is overrideFor for an OpenAI request (keepalive.Override.ApplyOpenAI).
func (k *keeper) overrideForOpenAI(tenantID, session string, pol CachePolicy) CachePolicy {
	o, ok := k.activeOverride(tenantID, session)
	if !ok {
		return pol
	}
	return o.lib().ApplyOpenAI(pol)
}

func (k *keeper) activeOverride(tenantID, session string) (sessionOverride, bool) {
	if k == nil || session == "" {
		return sessionOverride{}, false
	}
	k.mu.Lock()
	o, ok := k.overrides[kaKey(tenantID, session)]
	k.mu.Unlock()
	if !ok || !o.lib().Active(k.now()) {
		return sessionOverride{}, false
	}
	return o, true
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
	if !k.mayHold() {
		return fmt.Errorf("this hosted deployment records no audit trail, so nothing may be armed")
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
	// Outside the lock: retireSession takes it itself. Every thread of the session, because the
	// override authorized the hold for all of them.
	k.retireSession(tenantID, session)
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
