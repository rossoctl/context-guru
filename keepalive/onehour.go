package keepalive

import (
	"log/slog"
	"strings"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
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
	// costly: the probe showed that the 5m to 1h switch REWRITES the prefix on this route and model. The
	// 1-hour ping stops here, and instead the main threads that start while this is true write at 1 hour from
	// their first request (OneHourFirst). off wins over costly.
	costly     bool
	probeUntil time.Time // a probe holds the route until then; zero = none
}

// oneHourProbe is what a free probe leaves for the thread's return.
type oneHourProbe struct {
	route  string
	model  string    // the model the probe's request named, to see a model switch on the return
	at     time.Time // when the ping started
	prefix int64     // the billed prefix of the request it copied
}

// oneHourBlocked reports whether the route is switched off. Record uses it: a route that is off gets the
// plain plan from the start.
func (k *Keeper) oneHourBlocked(provider bschemas.ModelProvider, endpoint, route, model string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	st := k.oneHourRoutes[oneHourRoute(provider, endpoint, route, model)]
	return st != nil && (st.off || st.costly)
}

// ReadSetsTTL says whether the accountant may be told that a read sets the entry's TTL on this route and
// model (Provider.ReadSetsTTL, rossoctl/accountant#105). The proxy does not guess it from a model name or a
// provider: the route is Anthropic-shaped whatever it forwards to, and only evidence says what it did.
//
//   - A route the probe PROVED: always.
//   - A route not yet proven, for a request that follows a 1-hour ping (pinged): yes, unless the route is
//     off. This is the return that decides the probe. It is priced in the same pass, before the keeper
//     records the proof (the accounting runs first), so it must be priced as a return after a 1-hour ping
//     to be credited at all. The library's rescue is itself evidence: it credits only tokens the return
//     READ after the 5-minute lifetime. A cold return reads nothing and books none, and the keeper then
//     switches the route off (resolveOneHourProbe).
//   - Otherwise no. Nil-safe.
func (k *Keeper) ReadSetsTTL(provider bschemas.ModelProvider, endpoint, route, model string, pinged bool) bool {
	if k == nil {
		return false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	st := k.oneHourRoutes[oneHourRoute(provider, endpoint, route, model)]
	switch {
	case st != nil && (st.off || st.costly):
		return false
	case st != nil && st.proven:
		return true
	}
	return pinged
}

// oneHourClaim decides, at fire time, whether this ping may be the 1-hour ping. A proven route always
// may. An unknown route may for ONE wait at a time (the probe: probe is true then). An off route may not.
func (k *Keeper) oneHourClaim(rk string) (ok, probe bool) {
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
	case st.off || st.costly:
		return false, false
	case st.proven:
		return true, false
	case k.now().Before(st.probeUntil):
		return false, false // another wait is probing this route
	}
	st.probeUntil = k.now().Add(oneHourProbeTTL)
	return true, true
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
		k.oneHourRelease(rk, "the ping failed or landed on an entry that had already expired")
		return
	}
	read, write := float64(u.CacheRead), float64(max(u.CacheWrite, u.CacheWrite1h))
	switch {
	case read >= oneHourFreeRead*prefix && write <= oneHourFreeWrite*prefix:
		k.mu.Lock()
		st := k.oneHourRoutes[rk]
		// The last touch of this thread was a FREE 1-hour ping: until its next real request, or an hour
		// from the ping, the clock counts an hour from it (OneHourTouches). Kept apart from the entry,
		// which the keeper forgets after about 8 minutes.
		if len(k.oneHourTouches) >= MaxTurnKeys {
			k.oneHourTouches = map[string]oneHourTouch{}
		}
		k.oneHourTouches[j.key.id()] = oneHourTouch{key: j.key, at: start, route: rk}
		if st != nil && st.proven {
			e.ttl1h = true // a route proven to grant: from now until the next real request it is a 1-hour session
		} else {
			if len(k.oneHourProbes) >= MaxTurnKeys {
				k.oneHourProbes = map[string]oneHourProbe{}
			}
			k.oneHourProbes[j.key.id()] = oneHourProbe{route: rk, model: e.model, at: start, prefix: e.prefix}
		}
		k.mu.Unlock()
	case read < oneHourCostlyRead*prefix && write >= oneHourCostlyWrit*prefix:
		k.oneHourMarkCostly(rk, j, "cache_read", u.CacheRead, "cache_write", u.CacheWrite, "cache_write_1h", u.CacheWrite1h,
			"expected_prefix", int64(prefix))
	default:
		k.oneHourRelease(rk, "the ping's usage was neither a free nor a costly switch")
	}
}

// oneHourRelease lets another wait probe the route. why names the reason in the INFO line. It logs only
// when a probe was held (a route that was never probing has nothing to release).
func (k *Keeper) oneHourRelease(rk, why string) {
	k.mu.Lock()
	held := false
	if st := k.oneHourRoutes[rk]; st != nil && !st.proven {
		held = !st.probeUntil.IsZero()
		st.probeUntil = time.Time{}
	}
	k.mu.Unlock()
	if held {
		slog.Info("context-guru: 1-hour probe released; it proved nothing", "route", rk, "why", why)
	}
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
	st.off, st.costly, st.proven, st.probeUntil = true, false, false, time.Time{}
	k.mu.Unlock()
	if first {
		slog.Warn("context-guru: no more 1-hour keep-alive pings on this route; the threads fall back to the plain schedule: "+why,
			append([]any{"provider", string(provider), "route", route, "endpoint", endpoint, "model", model}, kv...)...)
	}
}

// resolveOneHourProbe runs on every real request of a thread (Record). If the thread left a free probe,
// this request is its return: a read of the prefix more than 5 minutes after the probe proves the grant
// (the 5-minute entry would be gone); a cold rebuild (a near-zero read and a large write) proves a
// downgrade. A return inside 5 minutes, over an hour later, a partial read, a return on another model, or
// a return after the client rewrote the prefix (prefixRewritten) proves nothing: the probe is released.
func (k *Keeper) resolveOneHourProbe(key string, startedAt time.Time, u Usage, usageOK bool, model string, prefixRewritten bool) {
	k.mu.Lock()
	p, ok := k.oneHourProbes[key]
	delete(k.oneHourProbes, key)
	delete(k.oneHourTouches, key) // a real request is the next touch: the 1-hour clock ends
	k.mu.Unlock()
	if !ok {
		return
	}
	gap := startedAt.Sub(p.at)
	prefix := float64(p.prefix)
	write := float64(max(u.CacheWrite, u.CacheWrite1h))
	switch {
	// A return on another model, or after the client rewrote the prefix (a compaction, a fork), reads cold
	// whatever the cache tier is: it proves nothing about the grant. The probe is not resolved.
	case !usageOK || gap <= 5*time.Minute || gap > time.Hour || prefixRewritten || (model != "" && model != p.model):
		k.oneHourRelease(p.route, "the return came inside 5 minutes, after an hour, on another model, or after the client rewrote the prefix")
	case float64(u.CacheRead) >= oneHourFreeRead*prefix:
		k.mu.Lock()
		newly := false
		if st := k.oneHourRoutes[p.route]; st != nil && !st.off {
			newly = !st.proven
			st.proven, st.probeUntil = true, time.Time{}
		}
		k.mu.Unlock()
		if newly {
			slog.Info("context-guru: 1-hour keep-alive grant proven; the entry outlived 5 minutes after the ping",
				"route", p.route, "model", p.model, "cache_read", u.CacheRead, "expected_prefix", p.prefix, "gap", gap.String())
		}
	case float64(u.CacheRead) < oneHourCostlyRead*prefix && write >= oneHourCostlyWrit*prefix:
		// A cold REBUILD: it read almost nothing and wrote the prefix back. A return that read and wrote
		// nothing (an uncacheable request) is not one.
		parts := strings.SplitN(p.route, "|", 4)
		k.oneHourSwitchOff(p.route, bschemas.ModelProvider(parts[0]), parts[2], parts[1], parts[3],
			"the entry did not outlive 5 minutes after the 1-hour ping (a silent downgrade)",
			"cache_read", u.CacheRead, "expected_prefix", p.prefix, "gap", gap.String())
	default:
		k.oneHourRelease(p.route, "the return read part of the prefix")
	}
}

// oneHourTouch is the fact "the last touch of this thread was a free 1-hour ping". Metadata only.
type oneHourTouch struct {
	key   Key
	at    time.Time // when the ping started
	route string
}

// OneHourTouch is what the dashboard needs of it.
type OneHourTouch struct {
	At time.Time
	// Proven is true when the route is proven to grant the 1-hour tier. While it is false the clock is a
	// belief the first return will test: the band shows "1h?".
	Proven bool
}

// OneHourTouches lists, by thread id, the threads of one session whose last touch was a free 1-hour ping
// that is under an hour old. A thread drops out at its next real request (Record), at the ping plus an
// hour, or when its route is switched off: a route that does not grant never shows an hour. Nil-safe.
func (k *Keeper) OneHourTouches(tenant string, all bool, session string) map[string]OneHourTouch {
	if k == nil || session == "" {
		return nil
	}
	now := k.now()
	k.mu.Lock()
	defer k.mu.Unlock()
	out := map[string]OneHourTouch{}
	for _, t := range k.oneHourTouches {
		if t.key.Session != session || (!all && t.key.Tenant != tenant) || !now.Before(t.at.Add(time.Hour)) {
			continue
		}
		st := k.oneHourRoutes[t.route]
		if st != nil && (st.off || st.costly) {
			continue
		}
		out[t.key.Thread] = OneHourTouch{At: t.at, Proven: st != nil && st.proven}
	}
	return out
}

// THE COSTLY ROUTE: WRITE AT 1 HOUR FROM THE FIRST REQUEST (#1261).
//
// When the probe shows that the switch rewrites the prefix, the strategy no longer falls back to auto.
// A main thread that STARTS while the route is costly asks 1 hour on every existing marker of every real
// request, from its first one: nothing is ever switched, so nothing is rewritten. Its pings stop (any wait
// cause, no 59-minute ping): the hour is the cover. Rules:
//
//   - the thread that paid the rewrite keeps 1 hour on every later request (its entry is a 1-hour entry);
//   - a main thread already running at 5 minutes stays on auto until it ends (a switch would cost it a
//     rewrite): the decision is made ONCE, at the thread's first request that qualifies, and kept;
//   - a subagent, and any request that does not declare tools, is unchanged;
//   - a request with 1-hour markers that WROTE tokens but was billed at 5 minutes means the gateway
//     downgraded the tier: the mode stops for that route and model (checkOneHourFirstDowngrade). A request
//     that wrote nothing proves nothing.
//
// The tools-declared test stands for "the main agent": the thread tracker gives the primary thread to the
// first thread that declares tools, and an auxiliary request (a title, a quota check) declares none.

// OneHourFirst reports whether this request must carry 1-hour markers on every breakpoint. The host asks
// at the end of apply, once the thread is known; the answer is made once per thread and kept. Nil-safe.
func (k *Keeper) OneHourFirst(tenant, session, threadID string, provider bschemas.ModelProvider, endpoint, route, model string, toolsDeclared bool) bool {
	if k == nil || session == "" || threadID != "" || IsSubagentThread(threadID) || !toolsDeclared {
		return false
	}
	key := Key{Tenant: tenant, Session: session, Thread: threadID}.id()
	rk := oneHourRoute(provider, endpoint, route, model)
	k.mu.Lock()
	defer k.mu.Unlock()
	st := k.oneHourRoutes[rk]
	costly := st != nil && st.costly && !st.off
	if v, decided := k.oneHourFirst[key]; decided {
		return v && costly
	}
	if len(k.oneHourFirst) >= MaxTurnKeys {
		k.oneHourFirst = map[string]bool{}
	}
	k.oneHourFirst[key] = costly // a thread that starts on a costly route is in the mode for good; any other is not
	return costly
}

// oneHourFirstThread reports whether the thread is in the mode on a route that is still costly.
func (k *Keeper) oneHourFirstThread(key string, provider bschemas.ModelProvider, endpoint, route, model string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	st := k.oneHourRoutes[oneHourRoute(provider, endpoint, route, model)]
	return k.oneHourFirst[key] && st != nil && st.costly && !st.off
}

// oneHourMarkCostly records that the probe's switch rewrote the prefix. The route goes to the costly mode (no
// more 1-hour pings; main threads that start now write at 1 hour), and the probing thread, whose entry is now
// a 1-hour entry, keeps 1 hour on every later request.
func (k *Keeper) oneHourMarkCostly(rk string, j pingJob, kv ...any) {
	k.mu.Lock()
	st := k.oneHourRoutes[rk]
	if st == nil {
		st = &oneHourRouteState{}
		k.oneHourRoutes[rk] = st
	}
	first := !st.costly && !st.off
	st.costly, st.proven, st.probeUntil = !st.off, false, time.Time{}
	if first {
		if len(k.oneHourFirst) >= MaxTurnKeys {
			k.oneHourFirst = map[string]bool{}
		}
		if j.thread == "" {
			k.oneHourFirst[j.key.id()] = true
		}
	}
	endpoint, model := j.e.endpoint, j.e.model
	k.mu.Unlock()
	if first {
		slog.Warn("context-guru: a 1-hour keep-alive ping rewrote the prefix on this route; no more 1-hour pings. "+
			"Main threads that start now write at 1 hour from their first request, with no pings",
			append([]any{"provider", string(j.provider), "route", j.route, "endpoint", endpoint, "model", model,
				"session", j.session, "thread", j.thread}, kv...)...)
	}
}

// checkOneHourFirstDowngrade runs on every real request. A thread in the mode that sent 1-hour markers and
// WROTE tokens, and was billed at 5 minutes, was downgraded by the gateway: the mode stops for the route and
// model, and the threads go back to auto, with one WARN. A request that wrote nothing proves nothing.
func (k *Keeper) checkOneHourFirstDowngrade(key string, provider bschemas.ModelProvider, endpoint, route, model string, body []byte, u Usage, usageOK bool) {
	if !usageOK || u.CacheWrite <= 0 || !k.oneHourFirstThread(key, provider, endpoint, route, model) ||
		!apply.AllBreakpointsAskExtendedTTL(body) || BilledOneHour(u.CacheWrite, u.CacheWrite1h, true) {
		return
	}
	k.oneHourSwitchOff(oneHourRoute(provider, endpoint, route, model), provider, route, endpoint, model,
		"a request with 1-hour markers wrote tokens and was billed at 5 minutes (a silent downgrade)",
		"cache_write", u.CacheWrite, "cache_write_1h", u.CacheWrite1h)
}
