package proxy

import (
	"context"
	"net/http"
	"sync"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/context-guru/tenant"
)

// The idle keep-alive itself — when to ping, the gates, the hard deadline, the write-instead-of-
// read guard and the per-ping ledger — is package keepalive, whose package comment explains the
// mechanism and the figures behind it. This file is cmd/context-guru-proxy's HOST for it: it
// resolves the policy (account config, manager strategy, session override — keepalivestrategy.go,
// keepaliveoverride.go), enforces the audit-sink rule, and owns the caller credentials through
// httpDispatcher (keepalivedispatch.go).

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
// and owns the httpDispatcher. Every decision about WHEN to ping lives in keepalive.Keeper.
type keeper struct {
	*keepalive.Keeper
	h *Handler
	d *httpDispatcher
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
}

func newKeeper(h *Handler) *keeper { return newKeeperWith(h, keepalive.Options{}) }

// newKeeperWith lets tests inject the clock and an inline runner. Dispatcher, Prices and Spare
// are always the proxy's own.
//
// Prices and Spare read h.opts.Prices and h.limiter at CALL time, not here, exactly as the keeper
// did when it held h itself: a handler's limiter and price table may be replaced after the keeper
// is built (tests do both), and a ping must use the ones in force when it fires.
func newKeeperWith(h *Handler, o keepalive.Options) *keeper {
	k := &keeper{h: h, overrides: map[string]sessionOverride{}}
	var client *http.Client
	if h != nil {
		client = h.client
	}
	k.d = newHTTPDispatcher(client)
	o.Dispatcher = k.d
	if h != nil {
		o.Prices = modelPricer{h}
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
	}
	k.Keeper = keepalive.New(o)
	// Loaded once at construction. Unlike overrides, a strategy is a standing rule a
	// manager expects to survive a restart, so it comes from SQLite rather than starting
	// empty — see loadStrategies.
	k.loadStrategies()
	return k
}

// modelPricer adapts the handler's modelinfo.Pricer, which prices by model alone. It reads
// h.opts.Prices per call; with none configured every ping is unpriced, as before.
type modelPricer struct{ h *Handler }

func (m modelPricer) Price(ctx context.Context, _, model string, _ int64) (keepalive.Price, bool) {
	if m.h.opts.Prices == nil {
		return keepalive.Price{}, false
	}
	return m.h.opts.Prices.Price(ctx, model)
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

func (k *keeper) arrive(tenant, session, threadID string) (int, int64, string) {
	if k == nil {
		return 0, 0, ""
	}
	return k.Keeper.Arrive(keepalive.Key{Tenant: tenant, Session: session, Thread: threadID})
}

func (k *keeper) retireSession(tenant, session string) {
	if k == nil {
		return
	}
	k.Keeper.RetireSession(tenant, session)
}

// forget releases everything held for one tenant (see keepalive.Keeper.Forget) and drops its
// per-session overrides too. This is the path a Settings save takes, so unticking the
// account-wide box must not leave armed sessions pinging on the strength of an authorization the
// account has just withdrawn.
func (k *keeper) forget(tenant string) {
	if k == nil {
		return
	}
	k.Keeper.Forget(tenant)
	k.mu.Lock()
	k.forgetOverrides(tenant)
	k.mu.Unlock()
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
func (k *keeper) record(tn *Tenancy, session, threadID string, startedAt time.Time, body []byte,
	up upstream, r *http.Request, provider bschemas.ModelProvider, route string, status int,
	u Usage, usageOK bool) {
	if k == nil || tn == nil {
		return
	}
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
	pol := tn.Cache
	applied := ""
	if k.h == nil || k.h.rec == nil {
		pol = keepalive.Policy{}
	} else {
		pol, applied = k.resolvePolicy(tn, session, provider, pol)
	}
	key := keepalive.Key{Tenant: tn.ID, Session: session, Thread: threadID}
	k.Keeper.Record(keepalive.Record{Key: key, StartedAt: startedAt, Body: body,
		Provider: provider, Endpoint: up.base, Route: route, Status: status,
		Usage: keepalive.Usage{FreshInput: u.FreshInput, CacheRead: u.CacheRead,
			CacheWrite: u.CacheWrite, Output: u.Output, CacheWrite1h: u.CacheWrite1h,
			StopReason: u.StopReason},
		UsageOK: usageOK, Policy: pol, Tag: applied, Agent: r.UserAgent(), Preset: tn.Preset,
		Header: r.Header, Store: tn.Store, Emitter: tn.Pipe.Emitter(),
		Hold: func() { k.d.hold(key, up, r) }})
}

// resolvePolicy layers the manager strategy and the session override over the account's own
// policy — the old record's policy block, unchanged.
func (k *keeper) resolvePolicy(tn *Tenancy, session string, provider bschemas.ModelProvider,
	pol CachePolicy) (CachePolicy, string) {
	// The manager-controlled strategy layer, resolved once here and generalizing the one
	// hook the session override already had — see keepalivestrategy.go's applyStrategy.
	// A matching ACTIVE strategy forces KeepAlive on and replaces Idle/MaxPings/
	// MinPrefixTokens/MaxUSDPerPing; it sits ABOVE account config and BELOW a session
	// override, which still wins on top and still cannot widen MaxUSDPerPing.
	applied := ""
	if provider != bschemas.OpenAI {
		pol, applied = k.applyStrategy(tn.ID, pol, k.now())
	}
	// A per-session manual override, if one is armed and unexpired. Everything below reads
	// `pol` and neither knows nor cares where it came from. An override may switch the
	// mechanism ON for a session whose account default (or no matching strategy) leaves it
	// off — that is the point of it, and the arming request is the consent act — but it may
	// not widen the per-ping cost guard, and it cannot reach around the kill switch or the
	// no-audit-sink refusal above.
	// The manager strategy and session override APIs bound Idle to Anthropic's
	// five-minute lifetime (60–290s). Applying that cadence to a 30-minute
	// OpenAI cache would buy 6–28 unnecessary pings per useful one. Strategies
	// remain Anthropic-only; a manual session override uses an OpenAI-specific
	// cadence and a one-hour credential-hold ceiling instead.
	if provider != bschemas.OpenAI {
		pol = k.overrideFor(tn.ID, session, pol)
	}
	if provider == bschemas.OpenAI {
		pol.Idle = pol.OpenAIIdle
		if pol.Idle <= 0 {
			pol.Idle = modelinfo.OpenAIDefaultKeepAliveIdle
		}
		pol = k.overrideForOpenAI(tn.ID, session, pol)
	}
	return pol, applied
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
