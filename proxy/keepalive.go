package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// The idle keep-alive: the one cache mechanism that cannot be a component.
//
// # What it is for
//
// A provider prompt-cache entry has a five-minute default lifetime, and a session idle
// longer than that loses its entire cached prefix. The next turn then re-bills every token
// of it at the cache-CREATION rate. Measured on this service over 19,805 requests and
// $3,139.97 of spend: 742 requests (3.7% of traffic) missed for that reason and cost
// $741.07 — 23.6% of everything — at $0.9987 each against $0.1178 for a request that hit.
// Against each row's own counterfactual the penalty is 11.35x, of which 91.2% is pure
// re-write. $584.83 of it, 18.6% of ALL spend, sits in misses whose idle gap was under an
// hour, and 47% of the misses missed the window by less than five minutes.
//
// The provider documents the two facts that make this fixable:
//
//	"By default, the cache has a 5-minute lifetime. The cache is refreshed for no
//	 additional cost each time the cached content is used."
//
//	"The lifetime is measured from the start of the request that writes or reads the cache
//	 entry, not from the end of its response."
//
// So a cache READ refreshes the lifetime, and a read costs 0.1x base input where re-creating
// the prefix costs 1.25x. One ping buys back one re-creation at 11.5:1. Simulated over the
// production window at the shipped defaults: 9,234 pings costing $125.29 convert 182 misses
// worth $295.37, net **+$170.08**, which is 5.42% of spend and 17.5x everything the whole
// transformation pipeline delivered over the same window ($9.72).
//
// # Why it is host-level and not a component
//
// A component runs while a request is in flight. The event this addresses happens when NO
// request is in flight: the agent is idle, and the entry lapses with nothing on the wire to
// hook. Nothing inside the pipeline can act at that moment, which is why this lives beside
// the request path rather than in it.
//
// # What it costs, and whose money it is
//
// A ping is a real upstream request billed to the caller's own credential. That is the trade
// — a small certain spend against a much larger probable one — and it is not ours to make on
// someone's behalf, so it is opt-in per account, capped per session, capped in count, and
// recorded as its own dashboard row with its own cost. See CachePolicy.
//
// # The two ways this can be WRONG, and the guards
//
//  1. **A ping that writes instead of reads costs 12.5x rather than saving.** The prefix hash
//     is cumulative up to and including the breakpoint, so the ping's body must be
//     byte-identical over that prefix. Hence the ping resends the exact bytes that were sent
//     upstream, and changes only `max_tokens` and `stream` — fields outside the hashed
//     prefix. If a ping's usage nevertheless reports more written than read, the mechanism is
//     wrong for that session: it is logged at ERROR, counted, and the session is dropped
//     rather than pinged again.
//  2. **Pinging a session that has ENDED is pure waste.** It is not detectable: only 290 of
//     3,891 session-final requests (7.5%) end with `end_turn`, the other 3,476 end
//     mid-tool-loop and look exactly like an active session. K is therefore the control, and
//     it is why the default is 2 — session-final waste scales linearly in K (measured
//     3,891 sessions x K pings, $18.30 at K=2) so the net peaks early and collapses: +$170.08
//     at K=2, +$162.56 at K=3, +$27.99 at K=12, −$99.72 at K=20.
//
// A note on what this deliberately does NOT do: it does not stop pinging a session whose last
// response ended with `end_turn`. That reads like a session-end signal and is the opposite.
// `end_turn` means the model finished and control returned to a HUMAN, which is precisely the
// state that produces a long gap: P(gap > 300s) is 37.15% after `end_turn` against 0.74%
// after `tool_use`, a 6.7x lift, and 83.7% of the recoverable dollars sit behind it. Stopping
// there would discard most of the value the mechanism exists to capture.

// CachePolicy is one tenant's resolved cache policy — the host-level half of the cache
// story, which no component can reach.
//
// A plain struct in this package rather than config.CacheConfig, for the reason
// ConfigBuilder exists: proxy does not depend on the configuration loader, so a library
// host or the bifrost adapter can set this directly.
type CachePolicy struct {
	// KeepAlive turns the mechanism on for this tenant. Off by default.
	KeepAlive bool
	// Idle is X: how long a session must be idle before the first ping, measured from the
	// previous request's START per the provider's documented lifetime rule. Zero disables.
	Idle time.Duration
	// MaxPings is K: the most pings one idle span may send. Zero disables.
	MaxPings int
	// MaxUSDPerPing refuses a ping whose projected cost exceeds this. PER PING, because ping
	// cost is bimodal (p50 $0.0004, p99 $0.2275, max $0.3780) while a per-SESSION budget
	// truncates exactly the long large-prefix sessions holding the value — capping the
	// window's pings per session at 20 drops the net from +$164 to $92.34.
	MaxUSDPerPing float64
	// MinPrefixTokens is the billed-prefix floor (the previous request's cache_read +
	// cache_write) a session must reach before it is pinged. With the first-request skip this
	// sends 8.7x fewer pings for 3.3% less money.
	MinPrefixTokens int
	// HeadTTL1h asks for the one-hour tier on the head breakpoints; HeadTTLMinTokens gates
	// it on request size. Passed through to apply.Opts — see apply/headttl.go.
	HeadTTL1h        bool
	HeadTTLMinTokens int
}

// on reports whether this policy can ping at all.
func (p CachePolicy) on() bool {
	return p.KeepAlive && p.Idle > 0 && p.MaxPings > 0
}

// Keeper bounds. Both are memory bounds and both are needed: a few enormous sessions and a
// great many small ones are different ways to exhaust the same 8 GiB.
const (
	// maxKeepAliveSessions bounds the tracked sessions. Same order as modes.Tracker's own
	// bound, and reached only by a deployment with that many opted-in sessions idle at once.
	maxKeepAliveSessions = 512
	// maxKeepAliveBytes bounds the total request bodies held for replay. A 200k-token
	// Claude Code body is under 1 MiB, so this is roughly 130 concurrent large sessions
	// before the oldest are dropped.
	maxKeepAliveBytes = 128 << 20
	// maxKeepAliveTurnKeys bounds the per-session turn counter. Larger than the session bound
	// because it holds one int rather than a body, and it has to outlive the entry.
	maxKeepAliveTurnKeys = 20000
	// maxKeepAliveBodyBytes refuses to hold a single body larger than this. A body this big
	// is a multi-million-token request whose ping would itself cost real money, and holding
	// one would spend a fifth of the whole budget on one session.
	maxKeepAliveBodyBytes = 8 << 20
	// keepAliveTick is how often the sweep runs. The window it is protecting is 300s wide
	// and the default fires at 280s, so a 2s granularity spends at most 2s of the 20s of
	// slack that leaves.
	keepAliveTick = 2 * time.Second
)

// kaEntry is one live session's replay material. It exists between the end of a request and
// the start of the next one, which is exactly the interval the mechanism acts in.
type kaEntry struct {
	tenant, session string
	// startedAt is when the last upstream request on this session STARTED — a real request
	// or a ping, whichever was later.
	//
	// DO NOT "SIMPLIFY" THIS TO THE RESPONSE'S COMPLETION TIME. It is the single decision in
	// this file that flips the sign of the whole feature. The provider's lifetime runs from
	// request start, so an anchor at completion silently spends the response's own duration
	// out of the 20 s of margin X leaves: `upstream_ms` is p50 8.9 s, p75 17.5 s, p90 33.4 s,
	// so roughly 21% of first pings would land after the entry had already expired — and a
	// ping onto a dead entry pays a 1.25x WRITE, the exact cost it exists to avoid. Simulated
	// over the production window, that one change takes X=280 from **+$164.46 to −$241.99**,
	// with ping cost tripling to $408.85.
	startedAt time.Time
	// body is the exact bytes last sent upstream. Retained, not copied: after serve returns
	// nothing else references the slice, so holding it costs the bytes and no allocation.
	// The prefix hash covers this content, which is why the ping must resend it verbatim.
	body []byte
	up   upstream
	// hdr is the minimal header set the ping needs, and — when the upstream is caller-pays
	// — the caller's provider credential.
	//
	// THIS IS THE ONE PLACE THIS SERVICE HOLDS A CALLER'S CREDENTIAL BEYOND THE LIFE OF A
	// REQUEST, and it is a deliberate, bounded, opt-in decision rather than an oversight.
	// The default deployment is caller-pays: no upstream in the operator's allow-list names
	// a key, so every request is authenticated with the caller's own key and there is no
	// server-held credential a between-requests ping could use. Without retention the
	// mechanism cannot exist here at all. It is held in memory only, never logged, never
	// persisted, never in a command line, dropped the moment the session is evicted or the
	// span ends, and held for at most MaxPings x Idle plus one sweep. When the upstream DOES
	// name a key (up.setKey != nil) nothing is retained: the credential is read from the
	// environment at call time, exactly as on the request path.
	hdr                         http.Header
	provider                    bschemas.ModelProvider
	model, route, preset, agent string
	pol                         CachePolicy
	pings                       int
	// turn is how many requests this session sent BEFORE the one that established this entry.
	// 0 means the session's first request, which the gate skips.
	turn int
	// prefix is what the previous request billed (cache_read + cache_write) — the size of the
	// entry a ping would refresh, and therefore the input to both the gate and the cost guard.
	prefix int64
	// pingUSD is the projected cost of one ping on that prefix, from the model's own read
	// rate. Checked against pol.MaxUSDPerPing before anything is sent.
	pingUSD float64
	// spent is what this session's pings have cost so far. Reported, never a cap — see
	// CachePolicy.MaxUSDPerPing for why the guard is per ping.
	spent float64
	// refreshed is what the last ping read from cache. It is the ceiling on what the next
	// real request may credit to this mechanism — see dash.Event.keepaliveSavedUSD.
	refreshed int64
	// stopped means this session will not be pinged again: a ping wrote instead of reading,
	// or the upstream refused in a way that repeating cannot fix.
	stopped bool
}

// due reports whether this TRACKED entry should be pinged now. Timing, K and the stop flag
// only: every policy gate is applied at record time instead, so an entry that exists is by
// construction one we intend to ping.
//
// That split is not cosmetic. A gate evaluated here would leave un-pingable sessions sitting in
// the map holding a request body and a caller credential until memory pressure evicted them —
// measured on the production replay, the map saturated at its 512-session bound and the
// eviction then threw away the entries that were about to pay, cutting conversions from 153 to
// 69. Gating at record keeps nothing we will not use.
func (e *kaEntry) due(now time.Time) bool {
	return !e.stopped && e.pol.on() && e.pings < e.pol.MaxPings &&
		!now.Before(e.startedAt.Add(e.pol.Idle))
}

// pingable reports whether this session is worth holding state for at all. Every condition is
// known when the request finishes, which is why it is checked there.
//
//   - **Not the session's FIRST request** (turn >= 1). Single-request sessions are 79% of the
//     pings and 0.9% of the value: nothing has accumulated for a second turn to hit, and most
//     never send one. A cheap filter, NOT a claim that later turns miss more often — P(the next
//     request is an addressable TTL miss) is flat at 2.24% from turn 0, 2.33% at turn 5, 2.47%
//     at turn 20, 2.29% at turn 100.
//   - **A billed prefix worth protecting**, in the provider's own units.
//   - **A projected ping cost inside budget.** Ping cost is bimodal (p50 $0.0004, p99 $0.2275,
//     max $0.3780), so the outlier to refuse is one ping and not a session's total.
//
// Deliberately NOT gated on the previous turn's `stop_reason`. `end_turn` looks like a
// session-end signal and is the opposite — P(gap > 300s) is 37.15% after it against 0.74%
// after `tool_use`, and 83.7% of the recoverable dollars sit behind it.
func (e *kaEntry) pingable() bool {
	return e.turn >= 1 && e.prefix >= int64(e.pol.MinPrefixTokens) &&
		(e.pol.MaxUSDPerPing <= 0 || e.pingUSD <= e.pol.MaxUSDPerPing)
}

// keeper runs the keep-alive. One goroutine per handler, one map, one lock.
type keeper struct {
	h    *Handler
	stop chan struct{}
	done chan struct{}
	// stopOnce and started make Stop idempotent and safe on a keeper whose sweep was never
	// launched. Handler.Close documents that it is safe to call twice, and it is: a second
	// close of the same channel would panic.
	stopOnce sync.Once
	started  atomic.Bool

	mu    sync.Mutex
	live  map[string]*kaEntry
	bytes int64
	// turns counts requests seen per session, so the first-request gate survives the entry's
	// own lifecycle: an entry is dropped the moment the next request arrives, and retired by
	// policy a few minutes later, while "has this session sent a request before?" has to
	// outlive both.
	//
	// ponytail: a plain map with a size bound and no per-key expiry. It holds one int per
	// session id and is cleared wholesale at the bound; give it an LRU only if session churn
	// is ever shown to cost real pings.
	turns map[string]int

	// send performs one ping. A field so tests can drive the whole policy — timing, caps,
	// limits, the write-instead-of-read guard — without a network.
	send func(pingJob, []byte) (Usage, int, error)
	// dispatch starts one ping. Production runs it on its own goroutine, because a round trip
	// must not be made while the map lock is held; the snapshot replay overrides this to run
	// inline so a five-day simulation is deterministic and needs no synchronisation.
	dispatch func(pingJob)
	// now is the clock, injected for the same reason.
	now func() time.Time

	// Counters, for /stats and for the honest question "is this thing spending money and
	// getting nothing?". Every one of them is a way the mechanism can be useless.
	pings    atomic.Int64
	skipped  atomic.Int64 // due, but a limit or a cap or a gate refused
	failed   atomic.Int64
	wrote    atomic.Int64  // pings that CREATED instead of refreshing — a bug, not a cost
	spentUSD atomic.Uint64 // float64 bits; only ever read as a whole
}

// keepAliveDisabled is the operator's kill switch, read once at construction. A single
// environment variable rather than a control-plane route: the thing an operator needs at
// 3am is one that works when the control plane is what is broken.
func keepAliveDisabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("CONTEXT_GURU_KEEPALIVE")))
	return v == "off" || v == "0" || v == "false"
}

func newKeeper(h *Handler) *keeper {
	k := &keeper{h: h, stop: make(chan struct{}), done: make(chan struct{}),
		live: map[string]*kaEntry{}, turns: map[string]int{}, now: time.Now}
	k.send = k.sendPing
	k.dispatch = func(j pingJob) { go k.fire(j) }
	return k
}

// start launches the sweep goroutine. Nil-safe, and a no-op when the kill switch is set.
func (k *keeper) start() {
	if k == nil {
		return
	}
	if keepAliveDisabled() {
		slog.Info("context-guru: cache keep-alive disabled by CONTEXT_GURU_KEEPALIVE")
		close(k.done)
		return
	}
	k.started.Store(true)
	go func() {
		defer close(k.done)
		t := time.NewTicker(keepAliveTick)
		defer t.Stop()
		for {
			select {
			case <-k.stop:
				return
			case <-t.C:
				k.sweep(k.now())
			}
		}
	}()
}

// Stop ends the sweep and drops every held body and credential.
func (k *keeper) Stop() {
	if k == nil {
		return
	}
	k.stopOnce.Do(func() {
		if k.started.Load() {
			close(k.stop)
			<-k.done
		}
	})
	k.mu.Lock()
	defer k.mu.Unlock()
	for key, e := range k.live {
		e.clear()
		delete(k.live, key)
	}
	k.bytes = 0
	k.turns = map[string]int{}
}

// clear drops an entry's body and credential. Called on every removal path, because the two
// things this holds are the two things it must not hold longer than it needs to.
func (e *kaEntry) clear() {
	e.body = nil
	e.hdr = nil
}

func kaKey(tenant, session string) string { return tenant + "\x00" + session }

// arrive tells the keeper a real request just started on this session, and reports what the
// keep-alive did during the span that request just ended.
//
// Called at the START of the request, before it goes upstream: an entry left in the map
// while a real request is in flight could be pinged concurrently with it, which is both
// pointless (the real request refreshes the entry itself) and a second request against the
// tenant's concurrency budget.
func (k *keeper) arrive(tenant, session string) (pings int, refreshed int64) {
	if k == nil || session == "" {
		return 0, 0
	}
	key := kaKey(tenant, session)
	k.mu.Lock()
	defer k.mu.Unlock()
	e, ok := k.live[key]
	if !ok {
		return 0, 0
	}
	pings, refreshed = e.pings, e.refreshed
	k.bytes -= int64(len(e.body))
	e.clear()
	delete(k.live, key)
	return pings, refreshed
}

// record hands the keeper what it needs to ping this session, once the request that
// established the cache entry has finished.
//
// startedAt is the instant the upstream request STARTED, per the provider's lifetime rule.
// The body is the exact bytes that went upstream.
func (k *keeper) record(tn *Tenancy, session string, startedAt time.Time, body []byte,
	up upstream, r *http.Request, provider bschemas.ModelProvider, route string, status int,
	u Usage, usageOK bool) {
	if k == nil || tn == nil || session == "" || len(body) == 0 || startedAt.IsZero() {
		return
	}
	// Only a request the provider ACCEPTED left a cache entry behind. Tracking a 4xx or a
	// failed forward would hold a body and a credential in order to ping a prefix that was
	// never cached — cost with no possible benefit.
	if status < 200 || status >= 300 {
		return
	}
	pol := tn.Cache
	if !pol.on() {
		return
	}
	if len(body) > maxKeepAliveBodyBytes {
		k.skipped.Add(1)
		return
	}
	// Nothing to keep alive unless the provider honours explicit breakpoints and this
	// request actually established or read an entry. A request that wrote and read nothing
	// has no prefix worth a ping.
	if !cacheAwareProvider(provider) {
		return
	}
	if usageOK && u.CacheRead == 0 && u.CacheWrite == 0 {
		return
	}
	// Thinking, and the one shape that cannot be pinged. The ping's whole correctness rests
	// on the hashed prefix being byte-identical, and the provider's own invalidation table
	// puts a change to the thinking parameters at the messages level — so the thinking block
	// must be resent exactly. With `thinking.type: enabled` the API also requires
	// max_tokens > thinking.budget_tokens, so the ping cannot be made cheap: budgets on this
	// traffic are 32,000 tokens, and a ping allowed to generate that much output costs far
	// more than the miss it prevents. `adaptive` carries no budget and is unaffected.
	//
	// Measured exposure of refusing these: of the 357 addressable misses in the production
	// window, 17 had a predecessor with thinking enabled, worth $14.29 of $732 — 2.0%.
	if gjson.GetBytes(body, "thinking.type").String() == "enabled" {
		k.skipped.Add(1)
		return
	}
	model := gjson.GetBytes(body, "model").String()
	// What the provider billed for this request's prefix, in its own units — the size of the
	// entry a ping would refresh. Both the gate and the projected cost read it, so a request
	// that reported no usage is not pingable: without it there is no honest estimate of either.
	prefix := u.CacheRead + u.CacheWrite
	e := &kaEntry{
		tenant: tn.ID, session: session, startedAt: startedAt, body: body, up: up,
		provider: provider, model: model, route: route, preset: tn.Preset,
		agent: r.UserAgent(), pol: pol, prefix: prefix,
		pingUSD: k.projectedPingUSD(model, prefix),
		hdr:     pingHeaders(r, up),
	}
	key := kaKey(tn.ID, session)
	k.mu.Lock()
	defer k.mu.Unlock()
	// The turn count outlives the entry, which is dropped on the next request's arrival. It is
	// incremented for EVERY request, including the ones the gate then refuses to track — that is
	// what lets a session's second request be recognised as such.
	k.turns[key]++
	e.turn = k.turns[key] - 1
	if len(k.turns) > maxKeepAliveTurnKeys {
		k.turns = map[string]int{key: k.turns[key]}
	}
	// Drop any previous entry either way: it is stale now, and if the gate refuses this request
	// its body and credential must not be left behind.
	if prev, ok := k.live[key]; ok {
		k.bytes -= int64(len(prev.body))
		prev.clear()
		delete(k.live, key)
	}
	if !e.pingable() {
		k.skipped.Add(1)
		e.clear() // hold no body and no credential for a session we will not ping
		return
	}
	k.live[key] = e
	k.bytes += int64(len(body))
	k.evictLocked()
}

// projectedPingUSD is what one ping on this prefix will cost: the prefix at the model's own
// cache-read rate, plus the single output token. Computed at record time so the guard can
// refuse an expensive ping BEFORE sending it rather than reporting it afterwards.
//
// Zero when the model is not priced, which lets the ping through — refusing to protect a cache
// because a price list is incomplete would be the wrong failure, and the spend still lands on
// its own dashboard row either way.
func (k *keeper) projectedPingUSD(model string, prefix int64) float64 {
	p := k.h.opts.Prices
	if p == nil || model == "" || prefix <= 0 {
		return 0
	}
	price, ok := p.Price(context.Background(), model)
	if !ok || price.Zero() {
		return 0
	}
	return float64(prefix)*price.CacheRead + price.Output
}

// evictLocked enforces both bounds by dropping the entries whose deadline is furthest away
// — the ones with the longest still to wait, and therefore the least imminent value.
func (k *keeper) evictLocked() {
	for len(k.live) > maxKeepAliveSessions || k.bytes > maxKeepAliveBytes {
		var worstKey string
		var worst time.Time
		for key, e := range k.live {
			if worstKey == "" || e.startedAt.After(worst) {
				worstKey, worst = key, e.startedAt
			}
		}
		if worstKey == "" {
			return
		}
		e := k.live[worstKey]
		k.bytes -= int64(len(e.body))
		e.clear()
		delete(k.live, worstKey)
		k.skipped.Add(1)
	}
}

// sweep fires every due ping. Returns how many it started, for tests.
//
// The map lock is held only to select and to stamp: a ping is an HTTP round trip and holding
// the lock across it would block every arriving request on this map.
func (k *keeper) sweep(now time.Time) int {
	if k == nil {
		return 0
	}
	k.mu.Lock()
	var due []pingJob
	for key, e := range k.live {
		// Retire a span that can do nothing more. Without this an entry sits in the map until
		// eviction pressure removes it, which on a quiet deployment is never — and what it
		// sits there holding is a request body and, on a caller-pays upstream, the caller's
		// provider credential. So the hold is bounded by the POLICY (at most (K+1) x X, about
		// 14 minutes at the defaults) rather than by how busy the box happens to be.
		if e.stopped || e.pings >= e.pol.MaxPings || !e.pingable() {
			if !now.Before(e.startedAt.Add(e.pol.Idle)) {
				k.bytes -= int64(len(e.body))
				e.clear()
				delete(k.live, key)
			}
			continue
		}
		if !e.due(now) {
			continue
		}
		// Everything the ping needs, taken HERE under the lock. Nothing outside it may read the
		// FIELDS e.body or e.hdr: the retirement branch above clears both, and a ping goroutine
		// still in flight would then be racing this sweep. The slice VALUE is safe to carry out
		// — the bytes are written once at record time and never mutated, and clear() drops the
		// field rather than the array — which is what keeps the rewrite below out of the
		// critical section.
		//
		// Stamp the clock BEFORE the call, and from the ping's own start: the provider's
		// lifetime runs from request start, so the next deadline is this instant plus X. It
		// also makes the entry not-due for the rest of this sweep, so a slow ping cannot be
		// started twice.
		e.startedAt = now
		e.pings++
		due = append(due, pingJob{e: e, raw: e.body, hdr: e.hdr.Clone(), up: e.up,
			tenant: e.tenant, session: e.session, ping: e.pings})
	}
	k.mu.Unlock()

	for _, j := range due {
		k.dispatch(j)
	}
	return len(due)
}

// pingJob is one ping's material, copied out of the entry under the keeper's lock. It exists
// because the ping runs on another goroutine and the entry it came from may be retired — body
// and credential dropped — before that goroutine finishes.
type pingJob struct {
	e *kaEntry
	// raw is the recorded request's bytes. The ping's own body is derived from it in fire,
	// outside the keeper's lock: the rewrite copies the whole body, and a large one under the
	// map lock would stall every arriving request on this proxy.
	raw             []byte
	hdr             http.Header
	up              upstream
	tenant, session string
	ping            int
}

// fire sends one ping and accounts for it. Always fails open and quietly: the agent is not
// waiting on this, so there is nobody to return an error to and nothing that a retry could
// help. A failed ping is logged, counted, and forgotten.
func (k *keeper) fire(j pingJob) {
	// The tenant's own limits apply, and a ping only ever uses SLACK. Refusing rather than
	// queueing is the requirement: a queued ping would sit in front of a real request the
	// agent IS waiting on. AcquireSpare reserves headroom so a ping cannot consume the last
	// slot or the last request of the minute.
	release, err := k.h.limiter.AcquireSpare(j.tenant, keepAliveReserveFrac)
	if err != nil {
		k.skipped.Add(1)
		return
	}
	defer release()

	body, ok := pingBody(j.raw)
	if !ok {
		k.markStopped(j.e)
		k.skipped.Add(1)
		return
	}
	start := k.now()
	u, status, err := k.send(j, body)
	ms := float64(k.now().Sub(start).Microseconds()) / 1000.0
	k.pings.Add(1)
	if err != nil {
		k.failed.Add(1)
		slog.Debug("context-guru: cache keep-alive ping failed",
			"tenant", tenantLabel(j.tenant), "session", j.session, "err", err)
		return
	}
	// A 4xx will repeat identically, so stop rather than spend the rest of K learning the
	// same thing. A 5xx is transient and the next ping may work.
	if status >= 400 && status < 500 {
		k.markStopped(j.e)
		slog.Debug("context-guru: cache keep-alive ping refused; not pinging this session again",
			"tenant", tenantLabel(j.tenant), "session", j.session, "status", status)
		return
	}
	// THE guard. A ping is supposed to be a pure cache READ; if the provider says it wrote
	// more than it read, the prefix did not match and this ping cost 12.5x what a read
	// costs instead of saving 11.5x. That is the mechanism being wrong, not expensive, so
	// it is an ERROR and the session is dropped.
	if u.CacheWrite > u.CacheRead {
		k.wrote.Add(1)
		k.markStopped(j.e)
		slog.Error("context-guru: cache keep-alive ping CREATED a cache entry instead of "+
			"refreshing one; not pinging this session again",
			"tenant", tenantLabel(j.tenant), "session", j.session,
			"cache_write", u.CacheWrite, "cache_read", u.CacheRead)
	}
	cost := k.record1(j, u, status, ms)
	slog.Debug("context-guru: cache keep-alive ping",
		"tenant", tenantLabel(j.tenant), "session", j.session, "ping", j.ping,
		"cache_read", u.CacheRead, "cache_write", u.CacheWrite, "output", u.Output,
		"cost_usd", cost, "ms", ms)
}

// markStopped stops pinging one session, if it is still tracked.
func (k *keeper) markStopped(e *kaEntry) {
	k.mu.Lock()
	defer k.mu.Unlock()
	e.stopped = true
}

// record1 books one ping: the session's running spend, the process counter, and the
// dashboard row. Returns the ping's cost.
//
// The row is the whole reason this is shippable. It carries the ping's own tokens and cost,
// marked keepalive, attributed to the tenant and the session — so the money this mechanism
// spends is visible in the same ledger as the money it saves, and an account can see it
// without being told.
func (k *keeper) record1(j pingJob, u Usage, status int, ms float64) float64 {
	e := j.e
	// The identifying fields are read under the lock with everything else: they are set once
	// at record time and never mutated, but a retired entry is concurrent with this goroutine
	// and the race detector is right to insist.
	k.mu.Lock()
	model, provider, route, preset, agent := e.model, e.provider, e.route, e.preset, e.agent
	k.mu.Unlock()
	var price modelinfo.Price
	priced := false
	if p := k.h.opts.Prices; p != nil && model != "" {
		price, priced = p.Price(context.Background(), model)
	}
	ev := &dash.Event{
		TS: k.now().UnixMilli(), TenantID: j.tenant, Model: model,
		Provider: string(provider), Route: route, Preset: preset,
		Status: status, KeepAlive: true,
	}
	ev.SessionID = j.session
	ev.Agent = dash.AgentFor(agent)
	ev.FreshInput, ev.CacheRead = u.FreshInput, u.CacheRead
	ev.CacheWrite, ev.OutputTokens = u.CacheWrite, u.Output
	ev.CacheWrite1h = u.CacheWrite1h
	ev.StopReason = u.StopReason
	ev.UpstreamMs = ms
	// What the ping actually asked for, so the row says it rather than reading as a request
	// with no output budget. The audit trail is the reason these rows exist; a column that is
	// blank because nobody filled it in is the same defect at a smaller scale.
	ev.MaxTokens = 1
	// A ping is not agent traffic, so it gets no cache-miss attribution and never touches
	// the session-recency map: doing so would re-date the session and make the NEXT real
	// request's gap read as four minutes instead of the twenty it actually was, hiding the
	// very thing this mechanism is here to demonstrate.
	ev.Price(price, priced && (u.CacheRead > 0 || u.CacheWrite > 0 || u.Output > 0))
	cost := ev.CostUSD
	k.mu.Lock()
	e.spent += cost
	e.refreshed = u.CacheRead
	k.mu.Unlock()
	for {
		old := k.spentUSD.Load()
		if k.spentUSD.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+cost)) {
			break
		}
	}
	if k.h.rec != nil {
		k.h.rec.Record(ev)
	}
	return cost
}

// pingBody turns the last real request into the cheapest possible cache READ of the same
// prefix.
//
// Only two fields change, and NEITHER is inside the hashed prefix. The provider hashes
// `tools` → `system` → `messages` cumulatively up to the breakpoint; `max_tokens` and
// `stream` are request-level knobs outside that sequence, so the prefix stays byte-identical
// and the ping matches the entry the real request wrote. Everything a change to WOULD
// invalidate the prefix — the model, the tools, `tool_choice`, the thinking parameters, any
// system or message content — is left exactly as it was sent.
//
// max_tokens is 1, not 0: 0 is the documented pre-warm form but is rejected alongside
// streaming and thinking, and a rejected ping is a wasted round trip. 1 is accepted
// everywhere and generates one token.
//
// stream is false so the response is one small JSON body with its usage block in it, rather
// than an SSE stream this would have to read to the end to price.
func pingBody(body []byte) ([]byte, bool) {
	out, err := sjson.SetBytes(body, "max_tokens", 1)
	if err != nil {
		return nil, false
	}
	out, err = sjson.SetBytes(out, "stream", false)
	if err != nil {
		return nil, false
	}
	return out, true
}

// pingHeaders is the minimal header set a ping needs, plus the caller's credential when the
// upstream is caller-pays.
//
// Built explicitly rather than by copying the client's headers: a replayed `content-length`
// or `accept-encoding` from a request whose body has changed is a bug waiting to happen, and
// the fewer of a caller's headers this holds for ten minutes the better. `anthropic-beta` is
// carried because a beta header can change how the body is interpreted, and a ping that is
// interpreted differently is not the same request.
func pingHeaders(r *http.Request, up upstream) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	for _, name := range []string{"Anthropic-Version", "Anthropic-Beta"} {
		if v := r.Header.Get(name); v != "" {
			h.Set(name, v)
		}
	}
	if up.setKey != nil {
		return h // a server-held key is injected at call time; nothing to retain
	}
	// Caller-pays: the credential is the caller's own, and without it there is no ping. Only
	// the auth slots, and only after our own token has been scrubbed out of them.
	for _, name := range authHeaders {
		for _, v := range r.Header.Values(name) {
			h.Add(name, v)
		}
	}
	scrubToken(h)
	return h
}

// sendPing performs the upstream round trip. Bounded by its own timeout: a ping has no
// client waiting and must never hold a concurrency slot for the ten minutes the request
// path's header timeout allows.
func (k *keeper) sendPing(j pingJob, body []byte) (Usage, int, error) {
	if j.up.base == "" {
		return Usage{}, 0, errNoUpstream
	}
	ctx, cancel := context.WithTimeout(context.Background(), keepAlivePingTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.up.base+j.up.path,
		strings.NewReader(string(body)))
	if err != nil {
		return Usage{}, 0, err
	}
	for name, vs := range j.hdr {
		req.Header[name] = append([]string(nil), vs...)
	}
	setUpstreamAuth(req.Header, j.up)
	resp, err := k.h.client.Do(req)
	if err != nil {
		return Usage{}, 0, err
	}
	defer resp.Body.Close()
	// Bounded read: a ping's response is a few hundred bytes of JSON, and an upstream that
	// answers a max_tokens:1 request with megabytes is not one to buffer.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil && !errors.Is(err, io.EOF) {
		return Usage{}, resp.StatusCode, err
	}
	u, _ := responseUsage(resp.Header.Get("Content-Type"), raw)
	return u, resp.StatusCode, nil
}

// keepAlivePingTimeout bounds one ping. Generous relative to what a max_tokens:1 answer
// takes and far short of the request path's own header timeout.
const keepAlivePingTimeout = 60 * time.Second

// keepAliveReserveFrac is the share of a tenant's rate and concurrency budget a ping may
// never touch. A quarter, so a tenant at three quarters of its limit stops being pinged
// while its agent keeps working — the requirement is that a ping never crowds out a real
// request, and the only way to honour that with a fixed-window counter is to stay away from
// the edge of it.
const keepAliveReserveFrac = 0.25

// cacheAwareProvider reports whether this provider honours explicit cache_control
// breakpoints, which is the precondition for there being an entry a ping could refresh.
func cacheAwareProvider(p bschemas.ModelProvider) bool {
	switch p {
	case bschemas.Anthropic, bschemas.Bedrock, bschemas.BedrockMantle, bschemas.Vertex:
		return true
	}
	return false
}

// KeepAliveStats is the keeper's own ledger, for /stats. Every field is a way the mechanism
// can be worthless, which is the point of publishing them together: pings with no
// conversions is a policy that spends and gains nothing, and `wrote` above zero is a bug.
type KeepAliveStats struct {
	Live     int     `json:"live_sessions"`
	Pings    int64   `json:"pings"`
	Skipped  int64   `json:"skipped"`
	Failed   int64   `json:"failed"`
	Wrote    int64   `json:"wrote_instead_of_read"`
	SpentUSD float64 `json:"spend_usd"`
}

// Stats snapshots the keeper's counters.
func (k *keeper) Stats() KeepAliveStats {
	if k == nil {
		return KeepAliveStats{}
	}
	k.mu.Lock()
	live := len(k.live)
	k.mu.Unlock()
	return KeepAliveStats{Live: live, Pings: k.pings.Load(), Skipped: k.skipped.Load(),
		Failed: k.failed.Load(), Wrote: k.wrote.Load(),
		SpentUSD: math.Float64frombits(k.spentUSD.Load())}
}
