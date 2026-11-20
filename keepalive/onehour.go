package keepalive

import (
	"log/slog"
	"strings"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
)

// The 1-hour-on-wait ping (#1243).
//
// WHY. A ping that asks 1 hour on a prompt cached at 5 minutes can move the entry to 1 hour. On the
// gateway measured on 2026-11-19 (litellm 1.108.4, Bedrock) that switch is a READ, not a rewrite: 15 of
// 15 switches read the whole prefix and wrote only the new tokens at 1 hour. So one 1-hour ping at the
// start of a long wait keeps the prompt for 1 hour at the price of one read, and no request pays the 2x
// write premium. The last request that touches an entry sets its lifetime, so the client's own 5-minute
// request on return reads the entry and moves it back to 5 minutes.
//
// WHEN. Only on a LONG wait (LongWait), at the usual first-ping time, and it is the only ping of the
// wait. A short wait (a tool, a subagent) is plain auto.
//
// THE BODY DIFFERS FROM EVERY EARLIER BODY. Relabelling the markers changes the bytes, and the ping
// already varies its output budget (pingBodyN). litellm replays a byte-identical request from its own
// response cache (same message id, an `x-litellm-cache-key` header, about 170 ms, the OLD usage): such a
// "ping" would not reach Bedrock and would refresh nothing. A 1-hour ping body is never byte-identical
// to the real request it copies, because every marker changed.
//
// THE RUN-TIME CHECK. The result may differ on another provider route (Anthropic first-party may rewrite
// the whole prefix on the switch, per a public report that was not reproduced here). So the keeper reads
// the first 1-hour ping's usage (judgeOneHour). A read near 0 and a large write means it cost a rewrite:
// the route is switched off for the life of the process, with one WARN, and its threads fall back to
// auto. A read of about the whole prefix is free, but a read does not show the tier, so see below.

// THE GRANT IS UNKNOWN UNTIL PROVEN. A 1-hour ping that READS the prefix and writes nothing looks the
// same on a route that grants the tier and on one that silently downgrades it to 5 minutes (a read bills
// no write, so zero at 1h says nothing). So a route is in one of four states, per provider, endpoint,
// route and MODEL (the grant varies by model):
//
//	unknown      the default. The first long wait sends the 1-hour ping as a PROBE. The probe is claimed
//	             (oneHourClaim) so only one wait per route runs on an unproven grant. While the route is
//	             unknown the entry is NOT marked 1-hour: nothing that reads cacheTTL (the substitute
//	             windows, the retry window, Entry.TTL1h) believes it.
//	proven       a main-thread return read comes more than 5 minutes after the probe and still reads the
//	             prefix: the entry lived, so the tier was granted. From then on the 1-hour ping is sent
//	             freely and the entry is marked 1-hour.
//	downgrading  that return is a cold rebuild: the entry died at 5 minutes. The route is switched off.
//	off          the probe itself rewrote the prefix (a costly switch), as above.
//
// A probe that never resolves (the person never returns, the return is inside 5 minutes or over an hour
// later, a partial read) is released after oneHourProbeTTL, or at once when its ping decides nothing.

// oneHourProbeTTL is how long a probe holds its route before another wait may probe. A person away for
// more than this has not told us anything.
const oneHourProbeTTL = 2 * time.Hour

// oneHourRoute names the route and model a 1-hour ping goes to.
func oneHourRoute(provider bschemas.ModelProvider, endpoint, route, model string) string {
	return string(provider) + "|" + endpoint + "|" + route + "|" + model
}

type oneHourRouteState struct {
	proven, off bool
	probeUntil  time.Time // a probe holds the route until then; zero = none
}

// oneHourProbe is what a free probe leaves for the thread's return.
type oneHourProbe struct {
	route  string
	at     time.Time // when the ping started
	prefix int64     // the billed prefix of the request it copied
}

// oneHourBlocked reports whether the route is switched off. Record uses it: a route that is off gets the
// plain plan from the start.
func (k *Keeper) oneHourBlocked(provider bschemas.ModelProvider, endpoint, route, model string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	st := k.oneHourRoutes[oneHourRoute(provider, endpoint, route, model)]
	return st != nil && st.off
}

// oneHourClaim decides, at fire time, whether this ping may be the 1-hour ping. A proven route always
// may. An unknown route may for ONE wait at a time (the probe). An off route may not.
func (k *Keeper) oneHourClaim(rk string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	st := k.oneHourRoutes[rk]
	if st == nil {
		if len(k.oneHourRoutes) >= oneHourRoutesMax {
			k.oneHourRoutes = map[string]*oneHourRouteState{}
		}
		st = &oneHourRouteState{}
		k.oneHourRoutes[rk] = st
	}
	switch {
	case st.off:
		return false
	case st.proven:
		return true
	case k.now().Before(st.probeUntil):
		return false // another wait is probing this route
	}
	st.probeUntil = k.now().Add(oneHourProbeTTL)
	return true
}

// oneHourRoutesMax bounds the route map. Routes are few; past it the map starts over.
const oneHourRoutesMax = 256

// Thresholds of the run-time check, as fractions of the expected prefix (the billed prefix of the real
// request): the read must cover at least 90% for the switch to count as free, with a write of at most
// 10%. A read under 10% with a write of at least half is a rewrite.
const (
	oneHourFreeRead   = 0.9
	oneHourFreeWrite  = 0.1
	oneHourCostlyRead = 0.1
	oneHourCostlyWrit = 0.5
)

// judgeOneHour reads the usage of a ping that really went out relabelled, dispatched at start.
//   - A ping that landed on an entry that had ALREADY expired (start past the entry's last refresh plus
//     its lifetime) reads ~0 and writes the prefix, the signature of a costly switch. It says nothing
//     about the route: decide nothing, and release the probe.
//   - A costly switch turns the route off.
//   - A free switch (a read of the prefix, no write) proves nothing about the tier. On a proven route
//     the entry is marked 1-hour. On an unknown route it is left alone and the probe waits for the
//     thread's return (resolveOneHourProbe).
//   - Anything else decides nothing, and releases the probe.
func (k *Keeper) judgeOneHour(j pingJob, status int, u Usage, start time.Time) {
	k.mu.Lock()
	e := j.e
	rk := oneHourRoute(j.provider, e.endpoint, j.route, e.model)
	prefix := float64(e.prefix)
	expired := start.After(e.refreshedAt.Add(e.cacheTTL()))
	k.mu.Unlock()
	if status < 200 || status >= 300 || prefix <= 0 || expired {
		k.oneHourRelease(rk)
		return
	}
	read, write := float64(u.CacheRead), float64(max(u.CacheWrite, u.CacheWrite1h))
	switch {
	case read >= oneHourFreeRead*prefix && write <= oneHourFreeWrite*prefix:
		k.mu.Lock()
		st := k.oneHourRoutes[rk]
		if st != nil && st.proven {
			e.ttl1h = true // a route proven to grant: from now until the next real request it is a 1-hour session
		} else {
			if len(k.oneHourProbes) >= MaxTurnKeys {
				k.oneHourProbes = map[string]oneHourProbe{}
			}
			k.oneHourProbes[j.key.id()] = oneHourProbe{route: rk, at: start, prefix: e.prefix}
		}
		k.mu.Unlock()
	case read < oneHourCostlyRead*prefix && write >= oneHourCostlyWrit*prefix:
		k.oneHourSwitchOff(rk, j.provider, j.route, e.endpoint, e.model, "the probe rewrote the prefix",
			"cache_read", u.CacheRead, "cache_write", u.CacheWrite, "cache_write_1h", u.CacheWrite1h,
			"expected_prefix", int64(prefix))
	default:
		k.oneHourRelease(rk)
	}
}

// oneHourRelease lets another wait probe the route.
func (k *Keeper) oneHourRelease(rk string) {
	k.mu.Lock()
	if st := k.oneHourRoutes[rk]; st != nil && !st.proven {
		st.probeUntil = time.Time{}
	}
	k.mu.Unlock()
}

// oneHourSwitchOff turns the route off for the life of the process, with one WARN.
func (k *Keeper) oneHourSwitchOff(rk string, provider bschemas.ModelProvider, route, endpoint, model, why string, kv ...any) {
	k.mu.Lock()
	st := k.oneHourRoutes[rk]
	if st == nil {
		st = &oneHourRouteState{}
		k.oneHourRoutes[rk] = st
	}
	first := !st.off
	st.off, st.proven, st.probeUntil = true, false, time.Time{}
	k.mu.Unlock()
	if first {
		slog.Warn("context-guru: no more 1-hour keep-alive pings on this route; the threads fall back to the plain schedule: "+why,
			append([]any{"provider", string(provider), "route", route, "endpoint", endpoint, "model", model}, kv...)...)
	}
}

// resolveOneHourProbe runs on every real request of a thread (Record). If the thread left a free probe,
// this request is its return: a read of the prefix more than 5 minutes after the probe proves the grant
// (the 5-minute entry would be gone); a cold rebuild proves a downgrade. A return inside 5 minutes, over
// an hour later, or a partial read proves nothing.
func (k *Keeper) resolveOneHourProbe(key string, startedAt time.Time, u Usage, usageOK bool) {
	k.mu.Lock()
	p, ok := k.oneHourProbes[key]
	delete(k.oneHourProbes, key)
	k.mu.Unlock()
	if !ok {
		return
	}
	gap := startedAt.Sub(p.at)
	prefix := float64(p.prefix)
	switch {
	case !usageOK || gap <= 5*time.Minute || gap > time.Hour:
		k.oneHourRelease(p.route)
	case float64(u.CacheRead) >= oneHourFreeRead*prefix:
		k.mu.Lock()
		if st := k.oneHourRoutes[p.route]; st != nil && !st.off {
			st.proven, st.probeUntil = true, time.Time{}
		}
		k.mu.Unlock()
	case float64(u.CacheRead) < oneHourCostlyRead*prefix:
		parts := strings.SplitN(p.route, "|", 4)
		k.oneHourSwitchOff(p.route, bschemas.ModelProvider(parts[0]), parts[2], parts[1], parts[3],
			"the entry did not outlive 5 minutes after the 1-hour ping (a silent downgrade)",
			"cache_read", u.CacheRead, "expected_prefix", p.prefix, "gap", gap.String())
	default:
		k.oneHourRelease(p.route)
	}
}
