package keepalive

import (
	"log/slog"
	"strings"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/store"
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
	route    string
	provider bschemas.ModelProvider // for the two lifetimes the return is judged against
	model    string                 // the model the probe's request named, to see a model switch on the return
	at       time.Time              // when the ping started
	prefix   int64                  // the billed prefix of the request it copied
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
		// The last touch of this thread was a FREE 1-hour ping. The tier and the time are in the store, where
		// the ping's touch wrote them (pingJob.touch). The keeper keeps only WHERE to look (the thread's
		// alias) and which route made the ping. Kept apart from the entry, which the keeper forgets after
		// about 8 minutes.
		k.makeRoomForThreadLocked(start)
		k.oneHourThreads[j.key.id()] = oneHourThread{key: j.key, route: rk,
			provider: j.provider, model: e.model, st: j.st, alias: touchAliasOf(j)}
		if st != nil && st.proven {
			e.ttl1h = true // a route proven to grant: from now until the next real request it is a 1-hour session
		} else {
			if len(k.oneHourProbes) >= MaxTurnKeys {
				k.oneHourProbes = map[string]oneHourProbe{}
			}
			k.oneHourProbes[j.key.id()] = oneHourProbe{route: rk, provider: j.provider, model: e.model, at: start, prefix: e.prefix}
		}
		k.mu.Unlock()
	case read < oneHourCostlyRead*prefix && write >= oneHourCostlyWrit*prefix:
		k.oneHourMarkCostly(rk, j, "cache_read", u.CacheRead, "cache_write", u.CacheWrite, "cache_write_1h", u.CacheWrite1h,
			"expected_prefix", int64(prefix))
	default:
		k.oneHourRelease(rk, "the ping's usage was neither a free nor a costly switch")
	}
}

// makeRoomForThreadLocked keeps the thread index bounded. It drops the threads whose stored touch is no longer a
// live free 1-hour ping first (the store says so, not the index). If every one is live, it starts over. A bound is
// a capacity event, not a fact about any entry: the stored records are NOT ended here, they expire by
// apply.EntryLifetime, so a live entry still reads warm in the cold check. Only the index (the dashboard, the
// press refusal) loses a thread then. A route switch-off is different: there the tier was never granted, and
// oneHourSwitchOff ends the records.
func (k *Keeper) makeRoomForThreadLocked(now time.Time) {
	if len(k.oneHourThreads) < MaxTurnKeys {
		return
	}
	for id, t := range k.oneHourThreads {
		if _, live := t.standing(now); !live {
			delete(k.oneHourThreads, id)
		}
	}
	if len(k.oneHourThreads) >= MaxTurnKeys {
		k.oneHourThreads = map[string]oneHourThread{}
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
	// The pings this route gave no longer count. The one record is the store's, so ending it there ends it for
	// the cold check, the dashboard and the manual ping at once.
	var ended []oneHourThread
	for _, t := range k.oneHourThreads {
		if t.route == rk {
			ended = append(ended, t)
		}
	}
	k.mu.Unlock()
	for _, t := range ended {
		apply.EndTouchTier(t.st, t.alias, apply.RouteTag(t.route))
	}
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
	delete(k.oneHourThreads, key) // a real request is the next touch: this index entry has nothing left to point at
	k.mu.Unlock()
	if !ok {
		return
	}
	gap := startedAt.Sub(p.at)
	// The two lifetimes the return is judged against: the base tier (the entry would be gone without the
	// grant) and the 1-hour tier (past it nothing proves anything). From the one table, for the probe's own
	// provider and model, so no number is written here.
	base, hour := cacheLifetime(p.provider, p.model, false), cacheLifetime(p.provider, p.model, true)
	prefix := float64(p.prefix)
	write := float64(max(u.CacheWrite, u.CacheWrite1h))
	switch {
	// A return on another model, or after the client rewrote the prefix (a compaction, a fork), reads cold
	// whatever the cache tier is: it proves nothing about the grant. The probe is not resolved.
	case !usageOK || gap <= base || gap > hour || prefixRewritten || (model != "" && model != p.model):
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

// oneHourThread is the index entry of a thread whose ping was a free 1-hour ping: WHERE the one record of the fact
// lives (the store and the alias) and which route made the ping. It holds no tier and no time, so it cannot
// disagree with the store: a reader asks the store (apply.LiveOneHourPing) and uses the entry only to find it.
// Metadata only.
type oneHourThread struct {
	key   Key
	route string
	// provider and model give the lifetime (apply.EntryLifetime), so no duration is stored.
	provider bschemas.ModelProvider
	model    string
	// st and alias are the record: cg:seen:<alias>, "<ms>:1h" or "<ms>^<pingMs>".
	st    store.Store
	alias string
}

// standing is the time of the live free 1-hour ping the store holds for this thread, asked of the one function.
func (t oneHourThread) standing(now time.Time) (atMs int64, live bool) {
	return apply.LiveOneHourPing(t.st, t.alias, t.provider, t.model, now.UnixMilli())
}

// touchAliasOf is the cold clock's key for a job: the one the host handed in, else the one its body derives.
func touchAliasOf(j pingJob) string {
	if j.touchAlias != "" {
		return j.touchAlias
	}
	return apply.TouchAliasFor(j.tenant, j.raw, j.provider)
}

// oneHourTouchLiveLocked reports whether the thread's last touch was a free 1-hour ping that still counts: the store
// says a ping stands and is under an hour old (apply.LiveOneHourPing), on a route not switched off. The same test
// as OneHourTouches. The caller holds k.mu.
func (k *Keeper) oneHourTouchLiveLocked(id string, now time.Time) bool {
	t, ok := k.oneHourThreads[id]
	if !ok {
		return false
	}
	if _, live := t.standing(now); !live {
		return false
	}
	st := k.oneHourRoutes[t.route]
	return st == nil || !st.off
}

// OneHourTouchLive reports whether key's thread is at 1 hour from a live free 1-hour ping. A host reads it to give
// a refused press the right reason before its own gates. Nil-safe.
func (k *Keeper) OneHourTouchLive(key Key) bool {
	if k == nil || key.Session == "" {
		return false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.oneHourTouchLiveLocked(key.id(), k.now())
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
	for _, t := range k.oneHourThreads {
		if t.key.Session != session || (!all && t.key.Tenant != tenant) {
			continue
		}
		at, live := t.standing(now)
		if !live {
			continue
		}
		st := k.oneHourRoutes[t.route]
		if st != nil && (st.off || st.costly) {
			continue
		}
		out[t.key.Thread] = OneHourTouch{At: time.UnixMilli(at), Proven: st != nil && st.proven}
	}
	return out
}

// WRITING AT 1 HOUR ON THE MAIN THREAD: D AND C (#1261).
//
// 1-hour-on-wait (A) pings an entry from 5 minutes to 1 hour. That needs the switch to be free (a read). On
// a route where the probe found it COSTLY (a rewrite, expected on Anthropic first-party, not tested) the
// proxy has two other ways to put a main thread on the 1-hour tier, both relabelling every existing marker of
// every real request to 1 hour (apply.RelabelAllTTL1h; no marker is added):
//
//   - D, the fallback of 1-hour-on-wait on a costly route. A main thread runs as auto until its first COLD
//     REBUILD of a prefix of at least OneHourRebuildTokens (150,000). That rebuild is sent with 1-hour markers,
//     so it is written at 1 hour, and from then on the thread writes at 1 hour with no pings. The rebuild is
//     decided BEFORE the request is sent, from what the keeper knows: the thread's last touch (its last
//     response, or a successful ping) is older than the entry's lifetime (5 minutes, or 1 hour if that request
//     carried 1-hour markers), and the prefix of that last request (cache read plus write) was at least the
//     threshold. The rebuild is sized by the previous prefix: after a compaction it can be smaller. Simulated
//     (150,000): hosted -12% against auto, owner's laptop -25%, but 15% to 18% above A.
//   - C, the explicit opt-in (OneHourAlways, strategy 1-hour-always): every new main thread writes at 1 hour
//     from its first request, on any route. ON MOST USERS IT DOES NOT PAY OFF: it pays the 0.75x premium on
//     every write, and most sessions have few long waits. Simulated on Bedrock-shaped data, main threads only:
//     hosted, 97 main sessions, +22% cost against auto, 77 worse, the premium 39% of the cost; the owner's
//     laptop -25% (33 sessions). So it is never a default and never turns on by itself.
//
// Rules both share:
//
//   - the main thread is the primary thread that declares tools (a title or quota request declares none);
//     a subagent is never relabelled;
//   - the thread that paid the probe's rewrite is in the mode too: its entry is a 1-hour entry;
//   - a thread in the mode gets NO ping on any wait cause, and no 59-minute ping;
//   - a request with 1-hour markers that WROTE tokens but was billed at 5 minutes means the gateway
//     downgraded the tier: the mode stops for that route and model (checkOneHourFirstDowngrade). A request
//     that wrote nothing proves nothing.
//
// C and A together: C wins on a main thread (its 1-hour markers leave nothing to ping), and A still serves a
// thread that is not in the mode. C on a route where the switch is free is allowed; it only costs more.

// OneHourMode is what the host passes with each question: the account's resolved choices.
type OneHourMode struct {
	// Always is the C opt-in.
	Always bool
	// RebuildTokens is D's threshold. 0 = 150,000. NEGATIVE turns D off: a costly route then falls back to
	// plain auto. D applies only on a costly route.
	RebuildTokens int64
}

// DefaultOneHourRebuildTokens is D's default threshold.
const DefaultOneHourRebuildTokens = 150000

// touchRec is what the keeper remembers of a thread's last touch, to see before sending that its entry is
// gone (D). Metadata only.
type touchRec struct {
	at     time.Time     // the end of the last response, or of a successful ping
	ttl    time.Duration // the entry's lifetime then: 5 minutes, or 1 hour when its markers were all 1 hour
	prefix int64         // billed prefix tokens of the last real request: cache read plus write
}

// noteTouch remembers a thread's last touch. A request sets the prefix; a ping keeps it. Bounded.
func (k *Keeper) noteTouchLocked(key string, at time.Time, ttl time.Duration, prefix int64) {
	if len(k.lastTouch) >= MaxTurnKeys {
		k.lastTouch = map[string]touchRec{}
	}
	if prefix <= 0 {
		prefix = k.lastTouch[key].prefix
	}
	k.lastTouch[key] = touchRec{at: at, ttl: ttl, prefix: prefix}
}

// OneHourFirst reports whether this request must carry 1-hour markers on every breakpoint. The host asks at
// the end of apply, once the thread is known. Nil-safe.
func (k *Keeper) OneHourFirst(tenant, session, threadID string, provider bschemas.ModelProvider, endpoint, route, model string, toolsDeclared bool, mode OneHourMode) bool {
	if k == nil || session == "" || threadID != "" || IsSubagentThread(threadID) || !toolsDeclared {
		return false
	}
	key := Key{Tenant: tenant, Session: session, Thread: threadID}.id()
	rk := oneHourRoute(provider, endpoint, route, model)
	k.mu.Lock()
	defer k.mu.Unlock()
	st := k.oneHourRoutes[rk]
	if st != nil && st.off {
		return false // the gateway downgraded 1-hour markers here: the mode is over for the route and model
	}
	if k.oneHourFirst[key] {
		return true // decided: the thread stays in the mode
	}
	enter := false
	switch {
	case mode.Always:
		enter = true // C: every new main thread, from its first request
	case st != nil && st.costly:
		// D: the first cold rebuild of a big prefix.
		need := mode.RebuildTokens
		if need == 0 {
			need = DefaultOneHourRebuildTokens
		}
		lt, seen := k.lastTouch[key]
		enter = need > 0 && seen && k.now().Sub(lt.at) > lt.ttl && lt.prefix >= need
	}
	if !enter {
		return false
	}
	if len(k.oneHourFirst) >= MaxTurnKeys {
		k.oneHourFirst = map[string]bool{}
	}
	k.oneHourFirst[key] = true
	return true
}

// oneHourFirstThread reports whether the thread is in the mode on a route that is not off.
func (k *Keeper) oneHourFirstThread(key string, provider bschemas.ModelProvider, endpoint, route, model string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	st := k.oneHourRoutes[oneHourRoute(provider, endpoint, route, model)]
	return k.oneHourFirst[key] && (st == nil || !st.off)
}

// oneHourMarkCostly records that the probe's switch rewrote the prefix. The route goes to the costly mode: no
// more 1-hour pings, and D is the fallback of its main threads. The probing thread, whose entry is now a
// 1-hour entry, keeps 1 hour on every later request.
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
			"A main thread now switches to 1 hour at its first cold rebuild of a big prefix (no pings after)",
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

// OneHourFirstFor is the apply hook of the 1-hour modes D and C (#1261), with the policy decision in
// the library. resolve returns the policy in force for the request: the account's with its stored
// strategy, so a manager's strategy that switches 1-hour-on-wait on counts, like for the pings. It
// is called only for a main-thread request that declares tools, so a host skips the policy chain for
// the rest. model is the model of the FINAL body apply hands over, the one Record keys the route
// state by too.
func (k *Keeper) OneHourFirstFor(resolve func() Policy, tenant, session, threadID string,
	provider bschemas.ModelProvider, endpoint, route, model string, toolsDeclared bool) bool {
	if threadID != "" || !toolsDeclared || session == "" {
		return false // only the main thread is ever relabelled
	}
	pol := resolve()
	if !pol.OneHourOnWait && !pol.OneHourAlways {
		return false
	}
	return k.OneHourFirst(tenant, session, threadID, provider, endpoint, route, model, toolsDeclared,
		OneHourMode{Always: pol.OneHourAlways, RebuildTokens: int64(pol.OneHourRebuildTokens)})
}
