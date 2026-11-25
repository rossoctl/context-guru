package proxy

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/dispatch"
	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/context-guru/predictive"
	"github.com/rossoctl/context-guru/tenant"
)

// The idle keep-alive itself — when to ping, the gates, the hard deadline, the write-instead-of-
// read guard and the per-ping ledger — is package keepalive, whose package comment explains the
// mechanism and the figures behind it. This file is cmd/context-guru-proxy's HOST for it: it
// resolves the policy (account config, manager strategy, session override — keepalivestrategy.go,
// keepaliveoverride.go), enforces the audit-sink rule, and owns the caller credentials through
// Dispatcher (dispatcher.go).

// CachePolicy is keepalive.Policy, kept under its old name for the config builder and tests.
type CachePolicy = keepalive.Policy

// Keeper bounds, now owned by package keepalive — kept here under their old unexported names so
// the rest of this package compiles unchanged.
const (
	maxKeepAliveSessions  = keepalive.MaxSessions
	maxKeepAliveBytes     = keepalive.MaxBytes
	maxKeepAliveTurnKeys  = keepalive.MaxTurnKeys
	maxKeepAliveBodyBytes = keepalive.MaxBodyBytes
	// keepAliveTick is how often the sweep runs. The window it is protecting is 300s wide
	// and the default fires at 280s, so a 2s granularity spends at most 2s of the 20s of
	// slack that leaves.
	keepAliveTick = 2 * time.Second
)

// DefaultMaxUSDPerPing mirrors keepalive's; pinned to config's by
// TestTheProxysPingCeilingDefaultMatchesTheConfigLoaders.
const DefaultMaxUSDPerPing = keepalive.DefaultMaxUSDPerPing

// KeepAliveStats is keepalive.Stats, kept under its old name so /stats JSON and callers are
// unchanged.
type KeepAliveStats = keepalive.Stats

func keepAliveDisabled() bool { return keepalive.Disabled() }

// kaKey is the overrides map's key for one session: the same tenant-NUL-session shape the
// keeper's own map uses for a session's primary thread.
func kaKey(tenant, session string) string { return tenant + "\x00" + session }

// keeper is the proxy's keep-alive host: it resolves policy (account config, manager strategy,
// session override — keepalivestrategy.go, keepaliveoverride.go), enforces the audit-sink rule,
// and holds the credentials its pings use, through the Handler's Dispatcher. Every decision about WHEN to ping lives in keepalive.Keeper.
type keeper struct {
	*keepalive.Keeper
	h *Handler
	d *Dispatcher
	// mu guards overrides and strategies only — the core has its own lock.
	mu sync.Mutex
	// overrides is the per-session manual keep-alive, keyed by kaKey. In memory only and
	// deliberately so — an authorization to spend that silently survives a restart is worse
	// than one that does not. See keepaliveoverride.go.
	overrides map[string]sessionOverride
	// strategies is the manager-controlled keep-alive strategy list, loaded from the
	// tenant registry at process start and replaced wholesale on every write through the
	// control routes — never mutated in place, the same pattern k.overrides itself
	// documents for a swap under lock. UNLIKE overrides, this one IS meant to survive a
	// restart: a strategy is a standing rule a manager expects to still be there after a
	// deploy, so it is reloaded from SQLite at boot. See keepalivestrategy.go.
	strategies []tenant.Strategy
	// pred holds the learned keep-alive campaigns (predictive.go). Never nil.
	pred *predRuntime
	// signalApplied counts client lifecycle signals that changed anything (clientsignal.go).
	signalApplied atomic.Int64
}

func newKeeper(h *Handler) *keeper {
	o := keepalive.Options{}
	if h != nil {
		o.TerminalTools = h.opts.TerminalTools
	}
	return newKeeperWith(h, o)
}

// newKeeperWith lets tests inject the clock and an inline runner. Dispatcher and Spare are always
// the proxy's own.
//
// The Dispatcher's Price and Spare read h.opts.Prices and h.limiter at CALL time, not here, exactly as the keeper
// did when it held h itself: a handler's limiter and price table may be replaced after the keeper
// is built (tests do both), and a ping must use the ones in force when it fires.
func newKeeperWith(h *Handler, o keepalive.Options) *keeper {
	k := &keeper{h: h, overrides: map[string]sessionOverride{}}
	if h != nil {
		k.d = h.dispatcher()
		k.pred = newPredRuntime(h.campaignDir())
	} else {
		k.pred = newPredRuntime("")
		k.d = NewDispatcher(nil)
	}
	o.Dispatcher = k.d
	if h != nil && h.waits != nil {
		// The thread's open background tasks and teammate waits, for the wait cause the sweep's
		// question stores with its held answer (#404, #453).
		o.OpenTasks = func(tenantID, session, threadID string) (int, int) {
			return h.waits.OpenCounts(tenantID, session, threadID)
		}
	}
	if h != nil {
		o.Spare = func(tenantID string, reserveFrac float64) (func(), error) {
			return h.limiter.AcquireSpare(tenantID, reserveFrac)
		}
	}
	o.OnTick = func(now time.Time) {
		// Expired overrides go on the same 2 s tick, so the map cannot grow on the strength of
		// authorizations nobody withdrew. overrideFor treats an expired one as absent regardless,
		// so this is hygiene rather than correctness.
		k.mu.Lock()
		k.expireOverridesLocked(now)
		k.mu.Unlock()
		// Likewise for an incoming cheap-model ref nothing has asked about since IncomingRefTTL:
		// incoming() and Do() only expire the one ref they were just asked about, so this is what
		// frees the common case of a ref nobody asks about again at all.
		k.d.sweepIncoming(now)
		// A learned campaign's periodic rebuild rides the same tick (campaignauto.go); it starts
		// at most one refresh per period and never alongside a build.
		if k.h != nil {
			k.h.maybeRefreshAutoCampaigns(now)
		}
	}
	k.Keeper = keepalive.New(o)
	// Loaded once at construction. Unlike overrides, a strategy is a standing rule a
	// manager expects to survive a restart, so it comes from SQLite rather than starting
	// empty — see loadStrategies.
	k.loadStrategies()
	return k
}

// dispatcher returns the Handler's Dispatcher, building it on first use: New builds it, and a test
// Handler built as a literal gets one when its keeper does. Construction is single-threaded.
//
// Its Price adapts h.opts.Prices, which prices by model alone, and reads it per call; with none
// configured every call is unpriced, as before.
func (h *Handler) dispatcher() *Dispatcher {
	if h.dispatch == nil {
		h.dispatch = NewDispatcher(h.client)
		h.dispatch.price = func(ctx context.Context, _, model string, promptTokens int64) (keepalive.Price, bool) {
			if h.opts.Prices == nil {
				return keepalive.Price{}, false
			}
			p, ok := h.opts.Prices.Price(ctx, model)
			if !ok {
				return p, false
			}
			// The rates of the prompt size the caller names (0 or unknown: the base rates).
			return p.At(promptTokens), true
		}
	}
	return h.dispatch
}

func (k *keeper) now() time.Time { return k.Keeper.Now() }

func (k *keeper) start() {
	if k != nil {
		k.Keeper.Start()
	}
}

// Stop ends the sweep, releases everything held, and drops the overrides with it: they are
// authorizations to spend, and the process they authorized is ending.
func (k *keeper) Stop() {
	if k == nil {
		return
	}
	k.Keeper.Shutdown()
	k.mu.Lock()
	k.overrides = map[string]sessionOverride{}
	k.mu.Unlock()
}

// arrive tells the keeper a real request just started on this session, and reports what the
// keep-alive did during the span that request just ended.
//
// Called at the START of the request, before it goes upstream: an entry left in the map
// while a real request is in flight could be pinged concurrently with it, which is both
// pointless (the real request refreshes the entry itself) and a second request against the
// tenant's concurrency budget.
//
// strategyID is e.appliedStrategy — which manager-controlled strategy resolved this idle
// span's policy, "" when none did. It is in scope right here and nowhere else once the entry
// is cleared, so this is the one place a real request's credit can be attributed to the
// strategy that earned it, rather than only the tenant's whole lifetime credit.
//
// Only THIS thread's entry is cleared: a subagent's request says nothing about whether the main
// thread is still idle.
func (k *keeper) arrive(tenant, session, threadID string) (pings int, refreshed int64, strategyID string) {
	a := k.arriveAt(tenant, session, threadID)
	return a.pings, a.refreshed, a.strategyID
}

// kaArrival is what the keeper knew about an idle span at the moment a real request ended it.
type kaArrival struct {
	pings      int
	refreshed  int64
	strategyID string
	// refreshedAt is when the provider's entry was last known refreshed: the previous real
	// request's start, or the start of the last ping that READ or WROTE the cache. Zero when no entry.
	refreshedAt time.Time
	// refreshedOneHour: the last touch was a free 1-hour ping that still counts, so the entry is at the
	// 1-hour tier whatever tier its last write was billed at (#1350). A fact the proxy passes on.
	refreshedOneHour bool
}

// arriveAt is arrive plus the instant the provider's entry was last refreshed. The cache-miss
// verdict needs it: the dashboard's idle gap runs from the last real request, but a ping that
// read the cache moved the provider's clock, so the entry's true age is shorter.
func (k *keeper) arriveAt(tenant, session, threadID string) kaArrival {
	if k == nil || session == "" {
		return kaArrival{}
	}
	key := keepalive.Key{Tenant: tenant, Session: session, Thread: threadID}
	pings, refreshed, tag, refreshedAt := k.Keeper.ArriveAt(key)
	return kaArrival{pings: pings, refreshed: refreshed, strategyID: tag, refreshedAt: refreshedAt,
		refreshedOneHour: k.Keeper.OneHourTouchLive(key)}
}

func (k *keeper) retireSession(tenant, session string) {
	if k == nil {
		return
	}
	k.Keeper.RetireSession(tenant, session)
}

// depart ends one open request on key's thread, for a path that returns after arrive without
// reaching record (record departs for itself, through keepalive.Keeper.Record).
func (k *keeper) depart(tenant, session, threadID string) {
	if k == nil {
		return
	}
	k.Keeper.Depart(keepalive.Key{Tenant: tenant, Session: session, Thread: threadID})
}

// markEnded, endedAfter and retireSessionStartedBefore are the client-signal half of the keeper
// (clientsignal.go), nil-safe like the wrappers above. session is the SCOPED client id
// (scopedClientID), the same string record passes as Key.Session or Record.Client.
func (k *keeper) markEnded(tenant, session string, at time.Time) {
	if k == nil {
		return
	}
	k.Keeper.MarkEnded(tenant, session, at)
}

func (k *keeper) endedAfter(tenant, session string, startedAt time.Time) bool {
	if k == nil {
		return false
	}
	return k.Keeper.EndedAfter(tenant, session, startedAt)
}

func (k *keeper) retireSessionStartedBefore(tenant, session string, cutoff time.Time) int {
	if k == nil {
		return 0
	}
	return k.Keeper.RetireSessionStartedBefore(tenant, session, cutoff)
}

// forget releases everything held for one tenant (see keepalive.Keeper.Forget) and drops its
// per-session overrides too. This is the path a Settings save takes, so unticking the
// account-wide box must not leave armed sessions pinging on the strength of an authorization the
// account has just withdrawn.
func (k *keeper) forget(tenant string) {
	if k == nil {
		return
	}
	// Overrides first, then the core's entries. That shrinks the window in which a concurrent
	// record resolves its policy from an override that is being dropped and re-arms an entry
	// after Forget has cleared them. It does not close it: the wrapper's mu is released before
	// the core call, because it is never held across one.
	k.mu.Lock()
	k.forgetOverrides(tenant)
	k.mu.Unlock()
	k.Keeper.Forget(tenant)
}

func (k *keeper) pendingPings() int {
	if k == nil {
		return 0
	}
	return k.Keeper.PendingPings()
}

// Stats and LiveSessionKeys shadow the promoted methods only to stay nil-safe on a nil *keeper,
// as they were before the core moved out: a promoted method dereferences the embedded pointer.
func (k *keeper) Stats() KeepAliveStats {
	if k == nil {
		return KeepAliveStats{}
	}
	return k.Keeper.Stats()
}

func (k *keeper) LiveSessionKeys() []string {
	if k == nil {
		return nil
	}
	return k.Keeper.LiveSessionKeys()
}

// record hands the keeper what it needs to ping this thread, once the request that
// established the cache entry has finished. It resolves the policy and hands off to
// keepalive.Keeper.Record.
//
// startedAt is the instant the upstream request STARTED, per the provider's lifetime rule.
// The body is the exact bytes that went upstream. The entry, the deadline and the turn count
// are per thread; a session override still applies to every thread of its session.
//
// record is a shim for tests: it passes no touch alias. Production calls recordTouch, with the alias
// apply derived from the arriving request.
func (k *keeper) record(tn *Tenancy, session, threadID string, startedAt time.Time, body []byte,
	up upstream, r *http.Request, provider bschemas.ModelProvider, route string, status int,
	u Usage, usageOK bool) {
	_ = k.recordTouch("", 0, 0, tn, session, threadID, startedAt, body, up, r, provider, route, status, u, usageOK, nil)
}

// recordTouch is record with the liveness-clock alias apply derived from the ARRIVING request
// (apply.Trace.TouchAlias). The body here went through tool deferral, which changes the head the
// alias hashes, so a ping that re-derived the alias from it would touch a key the request never
// reads (#902). "" falls back to deriving it from the body.
//
// It returns the ping plan the keeper built for the wait this response starts (#1198), or nil when
// none was built (the keep-alive is off for the request, or a gate refused it first).
//
// open, when non-nil, are the open-task counts of this turn's own body (the early scan, earlyscan.go). The
// same counts decide the windows here and the plan in Record, so a turn that starts background work gets the
// background count, the background floor and no windows together (#1225, #1227).
func (k *keeper) recordTouch(touchAlias string, touchAtMs, touchSeq int64, tn *Tenancy, session, threadID string, startedAt time.Time, body []byte,
	up upstream, r *http.Request, provider bschemas.ModelProvider, route string, status int,
	u Usage, usageOK bool, open *keepalive.TaskCounts) *keepalive.PlanInForce {
	if k == nil || tn == nil {
		return nil
	}
	// keepalive.Keeper.Record departs the thread's open request (the one arrive opened) on every
	// return path of its own. A panic in this function BEFORE it hands off would skip that and
	// leave the thread paused until restart, so depart here unless the hand-off happened. Not an
	// unconditional deferred depart: Record has already departed on the normal path, and a second
	// depart would release a CONCURRENT request's count on the same thread. The key is the one
	// arrive used (proxy.go: tn.ID, tr.Session, threadID — serve's session and threadID).
	recorded := false
	defer func() {
		if !recorded {
			k.depart(tn.ID, session, threadID)
		}
	}()
	// NO AUDIT SINK, NO RETENTION. An audit control that --dashboard silently removes is not a
	// control, and this is the one that makes the whole credential arrangement reviewable. Given
	// the choice between an unconditional second sink and refusing to hold anything we cannot
	// account for, this takes the second: it is three lines instead of a parallel writer, and it
	// fails in the safe direction — a deployment with no recorder keeps none of a caller's
	// credential and none of their conversation.
	//
	// A zero policy makes the core retire the key after its own status and kill-switch checks,
	// which is exactly the old order's effect.
	//
	// Consent is re-read from the tenancy on EVERY request rather than trusted from when the
	// hold began, and withdrawing it retires what is already held. The tenancy is re-resolved
	// per request and its cache is keyed on the configuration document, so an account that turns
	// the setting off stops being retained on its very next request; anything held for a session
	// that goes quiet instead is dropped by the keeper's hard deadline, within (K+1)x X.
	//
	// A client that reported this session ended (clientsignal.go) holds nothing further. The
	// check is made here as well as in the core so that such a request is not scored by a
	// campaign either; a zero policy then lets the core retire the thread and depart, as main's
	// record did (retire, then return). The switch is the host's: the core has none.
	client := clientSessionAlias(tn.ID, session, r)
	tracked := session != "" && len(body) > 0 && !startedAt.IsZero() && status >= 200 && status < 300
	ended := tracked && clientSignalEnabled() && (k.endedAfter(tn.ID, session, startedAt) ||
		(client != "" && k.endedAfter(tn.ID, client, startedAt)))
	pol := tn.Cache
	applied := ""
	if ended || k.h == nil || k.h.rec == nil {
		pol = keepalive.Policy{}
	} else {
		// A learned campaign (predictive.go) scores every finished response of its tenant, in
		// shadow or active mode. It runs only on a request the core would consider (2xx, a body,
		// a start time), after the kill switch and the no-audit-sink refusal, so a switched-off
		// or unaudited keep-alive retains and tracks nothing for it, and a failure inside it is
		// contained (recover).
		var pdec predDecision
		var pdecOK bool
		if tracked && !keepAliveDisabled() && provider != bschemas.OpenAI && usageOK {
			var arr *predictive.Arrival
			if u.Live != nil {
				arr = u.Live.Arrival
			}
			pdec, pdecOK = k.predictiveDecide(arr, !tn.Cache.KeepAlive, tn.ID, session, threadID,
				string(provider), startedAt, body, r.UserAgent(), u)
		}
		// The wait-cause windows, today's reading against the wide one, are keepalive.ResolveWithWindows.
		// This host supplies the tracker, the request's usage and its own strategy, campaign and override.
		wkey := keepalive.Key{Tenant: tn.ID, Session: session, Thread: threadID}
		pol, applied, open = keepalive.ResolveWithWindows(keepalive.WindowsInput{
			Account: pol, Tracked: tracked, OpenAI: provider == bschemas.OpenAI,
			Subagent: keepalive.IsSubagentThread(threadID), Open: open, Prefix: u.CacheRead + u.CacheWrite,
			CauseOf: func(o *keepalive.TaskCounts) string {
				return k.Keeper.WaitingFor(wkey, u.ToolUseNames, u.ToolUseIncomplete, u.StopReason, usageOK, o)
			},
			OneHourTier: func() bool {
				return k.Keeper.OneHourTier(wkey, body, keepalive.Usage{FreshInput: u.FreshInput, CacheRead: u.CacheRead,
					CacheWrite: u.CacheWrite, Output: u.Output, CacheWrite1h: u.CacheWrite1h, StopReason: u.StopReason}, usageOK, provider)
			},
			Resolve: func(p keepalive.Policy, ignore bool) (keepalive.Policy, string) {
				return k.resolvePolicyWindows(tn, session, provider, p, pdec, pdecOK, ignore)
			},
		})
		// The Ping button's opt-in (manualping.go): this REQUEST asked to be held for it. Set only here,
		// on the path that already passed the no-audit-sink rule, the client-ended check and the kill
		// switch, so the header can never widen what those refuse. It does not turn the schedule on.
		pol.ManualHold = tracked && !keepAliveDisabled() && provider != bschemas.OpenAI && wantsManualHold(r)
	}
	// The credential id only matters for a hold the core may keep, so it is not computed (an
	// HMAC over the masked auth headers) for any other request.
	cred := ""
	if tracked && (pol.On() || pol.ManualHold) {
		cred = credKey(up, r.Header)
	}
	key := keepalive.Key{Tenant: tn.ID, Session: session, Thread: threadID}
	// Built before `recorded` is set, so a panic evaluating any of its fields still departs.
	rec := keepalive.Record{Key: key, StartedAt: startedAt, Body: body, TouchAlias: touchAlias, TouchAtMs: touchAtMs, TouchSeq: touchSeq,
		Cred: cred, Client: client,
		Provider: provider, Endpoint: up.base, Route: route, Status: status,
		Usage: keepalive.Usage{FreshInput: u.FreshInput, CacheRead: u.CacheRead,
			CacheWrite: u.CacheWrite, Output: u.Output, CacheWrite1h: u.CacheWrite1h,
			StopReason: u.StopReason},
		UsageOK: usageOK, PrefixRewritten: u.PrefixRewritten, ToolUseNames: u.ToolUseNames, ToolUseIncomplete: u.ToolUseIncomplete, OpenTasks: open, Policy: pol, Tag: applied, Agent: r.UserAgent(), Preset: tn.Preset,
		Header: r.Header, Store: tn.Store, Emitter: tn.Pipe.Emitter(),
		Hold: func() dispatch.Ref { return k.d.hold(key, up, r.Header) }}
	plan := &keepalive.PlanInForce{}
	rec.Plan = plan
	recorded = true
	k.Keeper.Record(rec)
	if !plan.Built {
		return nil
	}
	return plan
}

// oneHourFirstFor is the apply hook of the 1-hour modes D and C (#1261), or nil when there is no keep-alive. At
// the end of apply, for a main-thread request that declares tools, it resolves the account's policy WITH its
// stored strategy (so a manager's strategy that switches 1-hour-on-wait on counts, like for the pings) and asks
// the keeper. The model comes from apply, which reads it from the final body.
func (h *Handler) oneHourFirstFor(r *reqInfo) func(session, threadID, model string, toolsDeclared bool) bool {
	if h == nil || h.keeper == nil || h.keeper.Keeper == nil || r.tn == nil {
		return nil
	}
	return func(session, threadID, model string, toolsDeclared bool) bool {
		return h.keeper.Keeper.OneHourFirstFor(func() keepalive.Policy {
			pol, _ := h.keeper.resolvePolicy(r.tn, session, r.provider, r.tn.Cache, predDecision{}, false)
			return pol
		}, r.tn.ID, session, threadID, r.provider, r.endpoint, r.route, model, toolsDeclared)
	}
}

// readSetsTTL is Keeper.ReadSetsTTL, nil-safe for a deployment with no keep-alive.
func (k *keeper) readSetsTTL(provider bschemas.ModelProvider, endpoint, route, model string, pinged bool) bool {
	return k != nil && k.Keeper != nil && k.Keeper.ReadSetsTTL(provider, endpoint, route, model, pinged)
}

// canPing reports whether the keep-alive can ping a thread of this session on this tenancy: the kill
// switch is off, an audit sink exists, the provider is not OpenAI (the sweep's question is Anthropic
// only), and the resolved policy (account, manager strategy, session override) is on. It is the
// record-time gate, evaluated before the request goes out and without a campaign decision (that one
// needs the response), so a thread a learned campaign would turn on later gets a plain ping. That is
// the safe direction: nothing extra is kept for it.
func (k *keeper) canPing(tn *Tenancy, session, threadID string, provider bschemas.ModelProvider) bool {
	if k == nil || tn == nil || k.h == nil || k.h.rec == nil || keepAliveDisabled() ||
		provider == bschemas.OpenAI || session == "" {
		return false
	}
	// The wait cause is not known yet, so the windows count, as before #858: for an unknown wait the
	// pessimistic answer is the safe one. A tool wait outside a window then registers no conversation copy
	// and still gets its plain ping; it loses only the sweep substitute. A copy is content kept in memory.
	pol, _ := k.resolvePolicy(tn, session, provider, tn.Cache, predDecision{}, false)
	return keepalive.CanKeepAlive(pol, func() bool {
		return k.Keeper.ToolSeen(keepalive.Key{Tenant: tn.ID, Session: session, Thread: threadID})
	})
}

// resolvePolicy layers the manager strategy and the session override over the account's own
// policy — the old record's policy block, unchanged.
func (k *keeper) resolvePolicy(tn *Tenancy, session string, provider bschemas.ModelProvider,
	pol CachePolicy, pdec predDecision, pdecOK bool) (CachePolicy, string) {
	return k.resolvePolicyWindows(tn, session, provider, pol, pdec, pdecOK, false)
}

// resolvePolicyWindows is resolvePolicy with the strategy's time windows optionally ignored (#858).
// The order (account, manager strategy, ignore-windows retry, campaign, session override) is
// keepalive.Resolve. This host supplies the strategy snapshot, the campaign decision and the armed
// override, which are its own state.
func (k *keeper) resolvePolicyWindows(tn *Tenancy, session string, provider bschemas.ModelProvider,
	pol CachePolicy, pdec predDecision, pdecOK bool, ignoreWindows bool) (CachePolicy, string) {
	in := keepalive.ResolveInput{TenantID: tn.ID, OpenAI: provider == bschemas.OpenAI, Now: k.now(),
		IgnoreWindows: ignoreWindows}
	if !in.OpenAI {
		in.Strategies = k.strategyList()
		if pdecOK && pdec.active {
			in.Campaign = keepalive.CampaignDecision{Active: true, Apply: pdec.apply,
				Tag: campaignStrategyID(pdec.campaign.meta.ID)}
		}
	}
	if o, ok := k.activeOverride(tn.ID, session); ok {
		lo := o.lib()
		in.Override = &lo
	}
	return keepalive.Resolve(pol, in)
}

// PendingPings reports how many tracked sessions still have a ping scheduled ahead of them.
//
// This exists for the idle-exit watchdog, and it exists because the keep-alive INVERTS the
// ordinary meaning of "idle": pinging is what the proxy does precisely while no client
// traffic is arriving. A watchdog counting only requests would therefore kill the process in
// exactly the window the feature was built for — the quiet gap after `end_turn`, where 83.7%
// of the recoverable dollars sit. So "no requests recently" is not sufficient to exit; "and
// nothing is waiting to be pinged" is the other half.
//
// The conditions are `due`'s minus the timing term: an entry that is stopped, or has spent its
// MaxPings, or whose policy is off will never be pinged again and must not hold the process open.
//
// One caveat, because "a live entry is by construction one we intend to ping" is very nearly true
// and not exactly: `sweep` also drops entries whose `pingable()` has since gone false, and it only
// looks once `now >= startedAt + Idle`. Inside that window this counts an entry that will never
// actually be pinged, which resets the activity clock and delays the exit by up to one `Idle`.
// Bounded by the entry's own retire timer at `(MaxPings+1) * Idle`, and immaterial against a 24h
// threshold — but the honest statement is "outside that window", not "by construction".
//
// Erring toward counting is the right direction anyway: over-counting delays an exit by minutes,
// while under-counting kills the process during the quiet gap the keep-alive exists to work in.
func (h *Handler) PendingPings() int {
	if h == nil {
		return 0
	}
	return h.keeper.pendingPings()
}

// KeepAlivePingStates is the keeper's per-thread ping state for one session (dash.API.SetPingStates):
// whether a manual ping can work, and when not, why. nil when there is no keeper.
func (h *Handler) KeepAlivePingStates(tenantID string, allTenants bool, session string) map[string]dash.PingState {
	if h == nil || h.keeper == nil {
		return nil
	}
	in := h.keeper.Keeper.PingStates(tenantID, allTenants, session)
	if in == nil {
		return nil
	}
	out := make(map[string]dash.PingState, len(in))
	for th, s := range in {
		out[th] = dash.PingState{Pingable: s.Pingable, Reason: s.Reason}
	}
	return out
}

// KeepAliveOneHourTouches is the keeper's per-thread "the last touch was a free 1-hour ping" facts for one
// session (dash.API.SetOneHourTouches), so /api/cachestate can count the clock an hour from the ping (#1243).
func (h *Handler) KeepAliveOneHourTouches(tenantID string, allTenants bool, session string) map[string]dash.OneHourTouch {
	if h == nil || h.keeper == nil || session == "" {
		return nil
	}
	in := h.keeper.Keeper.OneHourTouches(tenantID, allTenants, session)
	out := make(map[string]dash.OneHourTouch, len(in))
	for th, t := range in {
		out[th] = dash.OneHourTouch{AtMS: t.At.UnixMilli(), Proven: t.Proven}
	}
	return out
}

// KeepAlivePlans is the keeper's ping plan for one session's threads, keyed by thread id, for the
// status line's "held warm until" (dash.API.SetKeepAlivePlans). Only an entry that will still be
// pinged has a plan (keepalive.Keeper.Plans). allTenants matches the session in any tenant, for a
// single-tenant or service-wide scope.
func (h *Handler) KeepAlivePlans(tenantID string, allTenants bool, session string) map[string]dash.KeepAlivePlan {
	if h == nil || h.keeper == nil || session == "" {
		return nil
	}
	plans := h.keeper.Keeper.Plans(tenantID, allTenants, session)
	out := make(map[string]dash.KeepAlivePlan, len(plans))
	for th, p := range plans {
		out[th] = dash.KeepAlivePlan{RefreshedMS: p.RefreshedAt.UnixMilli(),
			NextPingMS: p.NextPing.UnixMilli(), PingsLeft: p.PingsLeft,
			IdleMS: p.Idle.Milliseconds()}
	}
	return out
}

// DashWaitPolicy is the dashboard's READ hook for an account's EFFECTIVE per-wait ping plan (#868). The
// calculator replays the plan with it. It follows the request path (resolvePolicyWindows) in the same
// order, and reports WaitPlan false, the fixed-K model, when that path would turn the plan off:
//
//  1. keepalive_wait_plan is false, or the account cannot be read (fail to the old model);
//  2. a covering manager strategy wins first, found as the request path finds it for a wait that is
//     not for a person (bestStrategy, windows ignored; respected while a campaign is active). Its Idle decides: 300 s or more (the 1-hour
//     band) turns the plan off, less keeps the account's own setting. The campaign is then skipped;
//  3. only when no strategy covers the account, an active campaign (keep-alive on) owns the count and
//     turns the plan off.
//
// While a campaign is active the answer depends on the hour: a strategy whose window is closed falls
// through to the campaign, so the flag is right at the moment the page is read.
//
// Limits, stated because one read hook cannot see a single request:
//   - A campaign owns the count only on a request that passes its price gate (pdecOK in
//     predictive.go). An account with no prices never has its count owned, but the hook reports plan off.
//   - A per-session override turns the plan off for that session only. No account-wide figure shows it.
//   - The plan flag is one value for the account. A 1-hour strategy whose window is closed leaves a
//     person's wait on the account plan (the windows apply to that wait), but the hook reports plan off.
//
// Wired in cmd/context-guru-proxy beside the keep-alive plans.
func (h *Handler) DashWaitPolicy() func(tenantID string) keepalive.Policy {
	return func(id string) keepalive.Policy {
		off := keepalive.Policy{}
		tn := h.static.Load()
		if reg := h.registry(); reg != nil && h.opts.Tenants != nil {
			t, err := reg.Get(id)
			if err != nil {
				return off
			}
			if tn, err = h.opts.Tenants.ForTenant(t); err != nil {
				return off
			}
		}
		if tn == nil {
			return off
		}
		c := tn.Cache
		// With an active campaign the request path does not retry a closed window (resolvePolicyWindows),
		// so a strategy counts only while its window is open and a closed one falls through to the
		// campaign. Without one, the window-ignoring lookup stands.
		campaignOn := false
		if pc := h.predRT().forTenant(tn.ID); pc != nil && c.KeepAlive {
			pc.mu.Lock()
			campaignOn = pc.meta.Mode == campaignModeActive
			pc.mu.Unlock()
		}
		var strategies []tenant.Strategy
		now := time.Now()
		if h.keeper != nil {
			strategies, now = h.keeper.strategyList(), h.keeper.now()
		}
		return keepalive.EffectiveWaitPlan(c, strategies, tn.ID, now, campaignOn)
	}
}
