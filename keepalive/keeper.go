package keepalive

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/dispatch"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/internal/thread"
	"github.com/rossoctl/context-guru/kvcache"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
)

// kaEntry is one live THREAD's replay material. It exists between the end of a request and
// the start of the next one on the same thread, which is exactly the interval the mechanism
// acts in.
//
// Per thread, not per session (#423). Each thread of a session — the main agent, each subagent,
// each fork — has its own provider cache entry. Keyed on the session, every request replaced the
// one entry, so while subagents ran the keeper pinged the newest subagent's prompt and let the
// main thread's cache expire.
type kaEntry struct {
	// key is the entry's own identity, held so every removal path can hand it back to the
	// Dispatcher's Release.
	key             Key
	tenant, session string
	// client is Record.Client as it stood when this entry was created — the tenant-scoped id a
	// client's hooks name this session by, when the host keys it differently (Codex's
	// session-id header). "" when there is none. MarkEnded / EndedAfter and
	// RetireSessionStartedBefore match a session on either Key.Session or this.
	client string
	// cred is Record.Cred as it stood when this entry was created — the opaque credential id
	// the host computed for the request, "" meaning a server-held key. Read by the route
	// breaker's gate (see breaker.go) at Record and at Sweep time.
	cred string
	// thread is the session thread this entry keeps warm; "" is the primary thread.
	thread string
	// refreshedAt is the last moment this entry's provider-side cache entry is KNOWN refreshed:
	// the real request that created it, or a later ping or summary substitute call that actually
	// READ the cache (CacheRead > 0). A ping or substitute that wrote instead of reading refreshed
	// nothing that was there before, so it must not move this clock — see record1 and
	// recordSummarySubstitute, both gated on CacheRead > 0 the same way. Surfaced to hosts via
	// Plan.RefreshedAt, for a status line.
	refreshedAt time.Time
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
	// st is the tenant's state store, held so a successful ping can record that it refreshed the
	// provider's entry — see apply.RecordCacheTouch and issue #243. Held for the same interval as
	// the masked body below, and for the same reason: the entry exists exactly between one request
	// and the next, which is when the ping fires.
	st store.Store
	// emitter is this tenant's components.Emitter, captured at record time off Record.Emitter
	// the same way st is captured off Record.Store — so record1 can report a ping without reaching into a
	// concrete sink (dash) itself. See components.KeepAliveEmitter and issue #382. NopEmitter
	// (never nil) when the tenant has no pipeline, which type-asserts to nothing and makes
	// reporting a no-op rather than a panic.
	emitter components.Emitter
	// body is the bytes last sent upstream, held MASKED under a per-process key (see mask). A keeper-owned copy, so it can be overwritten on release
	// without corrupting a slice something else still reads.
	//
	// Masked for the reason the credential is, and the asymmetry decides it: a leaked key is
	// rotatable, a leaked transcript is not. This is up to 8 MiB of the user's conversation, and
	// XOR over it costs a memcpy-shaped pass on a path that runs a few hundred times a day.
	// The prefix hash covers this content, which is why the ping must resend it verbatim.
	body []byte
	// endpoint is the upstream host the request went to, passed to Dispatcher.Price.
	endpoint string
	// hdr is the minimal NON-SECRET header set the ping needs (content type, API version, beta
	// flags). The credential never goes in here — see held.
	hdr http.Header
	// held is true between Record.Hold and Dispatcher.Release: the host is holding the caller's
	// provider credential for this entry — masked by the host, and only when the upstream is
	// caller-pays. The credential itself is the host's; the keeper holds only the obligation to
	// release it, exactly once, on every path that drops the entry (see releaseLocked).
	//
	// THIS IS THE ONE PLACE THIS SERVICE HOLDS A CALLER'S CREDENTIAL BEYOND THE LIFE OF A
	// REQUEST, and it is a deliberate, bounded, opt-in decision rather than an oversight.
	// The default deployment is caller-pays: no upstream in the operator's allow-list names
	// a key, so every request is authenticated with the caller's own key and there is no
	// server-held credential a between-requests ping could use. Without retention the
	// mechanism cannot exist here at all. It is held in memory only, never logged, never
	// persisted, never in a command line, MASKED while idle, ZEROIZED on every release path, and
	// bounded by a hard deadline of (MaxPings+1) x Idle that fires whether or not anything else
	// in this process is awake. A host that injects its own server-held key holds no caller
	// credential at all (in cmd/context-guru-proxy, an upstream that names a key reads it from
	// the environment at call time, exactly as on the request path).
	held bool
	// ref is the handle Record.Hold returned: what the Dispatcher sends this entry's pings with and
	// what Release drops. Meaningful only while held.
	ref                         dispatch.Ref
	provider                    bschemas.ModelProvider
	model, route, preset, agent string
	pol                         Policy
	pings                       int
	// turn is how many requests this session sent BEFORE the one that established this entry.
	// 0 means the session's first request, which the gate skips.
	turn int
	// prefix is what the previous request billed (cache_read + cache_write) — the size of the
	// entry a ping would refresh, and therefore the input to both the gate and the cost guard.
	prefix int64
	// turnRead is the cache_read of the real turn that made this entry. The sweep's question must
	// read at least this much (#721).
	turnRead int64
	// stopReason is the request that established this entry's own terminal stop reason
	// (Usage.StopReason) — present tense, not a prediction. Read only by pol.PredictorID's
	// gate in pingable(); "" on every entry until a strategy actually names a predictor.
	stopReason string
	// toolUseNames and usageOK are the establishing response's tool_use names and whether its usage
	// parsed: the inputs, with the open-task counts, of waitcause.ClassifyWaitingFor. Kept for the
	// wait cause the sweep's question stores with its held answer (#404, #453).
	toolUseNames []string
	usageOK      bool
	// pingUSD is the projected cost of one ping on that prefix, from the model's own read
	// rate. Checked against pol.MaxUSDPerPing before anything is sent.
	pingUSD float64
	// spent is what this session's pings have cost so far. Reported, never a cap — see
	// Policy.MaxUSDPerPing for why the guard is per ping.
	spent float64
	// refreshed is what the last ping read from cache. It is the ceiling on what the next
	// real request may credit to this mechanism — see dash.Event.keepaliveSavedUSD.
	refreshed int64
	// stopped means this session will not be pinged again: a ping wrote instead of reading,
	// or the upstream refused in a way that repeating cannot fix.
	stopped bool
	// missed counts this entry's current run of consecutive zero-read pings. Reset to 0 by
	// notePingRead; a second miss in a row (missed == 2) stops the entry — see noteOutcome.
	missed int
	// retried is set when a ping that failed in transport was given back for one retry (see
	// giveBackFailedPing), so a route that keeps failing is retried once, not on every tick.
	// Reset by notePingRead.
	retried bool
	// tag is the host's label for this entry (Record.Tag) — in cmd/context-guru-proxy, the id of
	// the manager-controlled keep-alive strategy that resolved this entry's policy, "" when none
	// matched (account config or a session override supplied it instead). Resolved once, at
	// record time, alongside the session override — see the proxy host's applyStrategy in
	// proxy/keepalivestrategy.go — and tagged onto the ping's own dash.Event by record1, never onto
	// the real request it rescues.
	tag string
	// ttl1h is the cache lifetime this thread was GRANTED: true when the provider billed the
	// one-hour tier, false for five minutes. Set once at record time by ResolveTier. It also sets
	// how long a manual-only hold lives (holdFor), and, for an automatic entry, pol is already the
	// one-hour schedule (Policy.ForOneHour).
	ttl1h bool
	// manualOnly marks an entry held only for the Ping button (manualhold.go): nothing pings it,
	// plans it or counts it as pending, and it lives ManualHoldMax (ManualHoldMax1h on a one-hour
	// thread) from startedAt.
	manualOnly bool
	// holdFor is how long a manual-only entry lives from startedAt.
	holdFor time.Duration
	// ceiling is the per-ping ceiling this entry was admitted under, kept because a manual-only entry's
	// pol is zeroed.
	ceiling float64
	// timer is the HARD retention deadline. A scheduled deadline rather than a check inside the
	// sweep, because the requirement is that a quiet process still drops the credential on
	// time: with no request and no other activity the sweep is the only thing that would ever
	// look, and coupling a security bound to a liveness loop is how a hold silently becomes
	// unbounded. time.AfterFunc fires regardless.
	timer *time.Timer
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
	return !e.stopped && !e.manualOnly && e.pol.On() && e.pings < e.pol.MaxPings &&
		!now.Before(e.startedAt.Add(e.pol.Idle))
}

// hold is how long this entry may keep the caller's credential and conversation: (K+1) x X, the
// hard deadline. About 12 min for a 5-minute thread at the defaults, 118 min for a 1-hour thread,
// and never more than MaxHold1h (236 min) for one.
func (e *kaEntry) hold() time.Duration {
	return time.Duration(e.pol.MaxPings+1) * e.pol.Idle
}

// ResolveTier decides whether a thread's cache is a one-hour cache, from the traffic.
//
// The BILLED tier is the source of truth, not the request: a gateway can downgrade a requested 1h
// to 5m (#330) and still answer 200. One-hour needs both halves:
//
//   - the request asks 1h on EVERY breakpoint. A mixed ask (the proxy's own 1h head over a 5m tail)
//     leaves a tail that expires in five minutes, and a 59-minute ping would refresh only the head.
//   - the response billed the one-hour tier for everything it wrote. A turn that only READS the
//     cache (cache_write 0) bills no tier, so it inherits the thread's last billed one (prev).
//
// Anything else is five minutes, which is the schedule the keeper always ran. Pure, and false on
// any doubt.
func ResolveTier(body []byte, u Usage, usageOK, prev bool) bool {
	if !usageOK || !apply.AllBreakpointsAskExtendedTTL(body) {
		return false
	}
	if u.CacheWrite > 0 {
		return u.CacheWrite1h >= u.CacheWrite
	}
	return prev
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
// The unconditional default is DELIBERATELY not gated on the previous turn's
// `stop_reason`. `end_turn` looks like a session-end signal and is the opposite — P(gap >
// 300s) is 37.15% after it against 0.74% after `tool_use`, and 83.7% of the recoverable
// dollars sit behind it. That finding is exactly why StopReasonPredictor below does not
// exclude end_turn either — it excludes the two clusters that finding never addressed.
//
// A strategy naming a PredictorID (opt-in, see the proxy host's knownPredictorIDs in
// proxy/keepalivestrategy.go, and
// docs/results/kv-ttl-predictor-arms.md) adds ONE more condition on top of everything
// above: the named predictor's probability at this entry's own stop_reason must clear the
// strategy's tuned threshold. No PredictorID (every account today, and every strategy
// that predates this field) means this whole block is skipped and behaviour is byte-for-
// byte what it always was.
//
// A FINISHED subagent (an "a:" thread whose last turn ended end_turn) is pinged like any other
// thread, on purpose (#426 review): nothing tells the proxy it will never resume. On the
// fix-summarizer session, of 76 such Sonnet threads with a prefix over 20k, 23 resumed before the
// first ping (no ping sent), 8 resumed inside the ping window — agent-team teammates do — worth
// ~$8.8 of avoided misses, and 45 never came back, ~$5.8 of pings. A blanket gate would lose more
// than it saves there.
func (e *kaEntry) pingable() bool {
	if e.turn < 1 || e.prefix < int64(e.pol.MinPrefixTokens) || e.pingUSD > e.pol.Ceiling() {
		return false
	}
	if e.pol.PredictorID == "" {
		return true
	}
	p, ok := predictorFor(e.pol.PredictorID)
	if !ok {
		// An unknown predictor id can only reach a live entry if knownPredictorIDs and this
		// switch drift apart. Fail toward NOT SPENDING — refuse the ping — rather than
		// silently pinging on a rule that does not exist; the money is the thing to protect
		// here, unlike the request path itself, which always fails open.
		return false
	}
	return p(e.stopReason) >= e.pol.PredictorThreshold
}

// Options configures a Keeper. Dispatcher is required; every other field has a default.
type Options struct {
	// Dispatcher sends pings and prices them. Its Price answering ok=false leaves a ping unpriced,
	// as with no price table.
	Dispatcher dispatch.Dispatcher // required
	// Spare reserves the tenant's spare rate/concurrency headroom for one ping; nil = no limiter.
	Spare func(tenant string, reserveFrac float64) (release func(), err error)
	Now   func() time.Time // nil: time.Now
	// Go runs one ping; nil: `go f()`. Exported for tests and replay tooling (an inline runner
	// makes a replay deterministic); hosts do not need it.
	Go     func(f func())
	OnTick func(now time.Time) // start of every sweep, outside the keeper lock; nil: none
	// OpenTasks reports a thread's open background-task and teammate-wait counts, for the wait cause
	// the sweep stores with a held answer (#404, #453). nil: 0, 0.
	OpenTasks func(tenant, session, thread string) (background, teammate int)
}

// Record is one finished upstream request, handed to Keeper.Record by the host.
type Record struct {
	Key       Key
	StartedAt time.Time // when the upstream request STARTED
	Body      []byte    // exact bytes sent upstream; the keeper copies and masks it
	Provider  bschemas.ModelProvider
	Endpoint  string // upstream host, passed to Dispatcher.Price
	Route     string // upstream path
	Status    int
	Usage     Usage
	// Cred is an opaque credential id the host computes for the request's provider key; ""
	// means a server-held key (never subject to the dead-credential mark, only ever the route
	// breaker). The package scopes it by Key.Tenant itself, so the host passes the bare id with
	// no tenant prefix: the proxy host passes its per-process HMAC of the masked auth headers
	// (proxy/credmask.go credKey).
	Cred string
	// Client is the id the client itself names the session by, when the host keys it
	// differently (Codex's session-id header). "" when there is none. MarkEnded and
	// RetireSessionStartedBefore match on either Key.Session or Client.
	Client  string
	UsageOK bool
	// ToolUseNames is every tool_use name the response carried, in order. Read only to classify what
	// the thread is about to wait on (waitcause.ClassifyWaitingFor), which the sweep's held answer
	// stores (#404, #453). A field of Record, not of Usage, so Usage stays comparable.
	ToolUseNames []string
	Policy       Policy // resolved by the host
	Tag          string // host label returned by Arrive (proxy: the applied strategy id)
	Agent        string
	Preset       string
	Header       http.Header // the request's headers; only PingHeaders' allow-list is read
	Store        store.Store
	Emitter      components.Emitter // nil: components.NopEmitter{}
	// Hold is called once, under the keeper lock, only if the entry is tracked. The host captures
	// the request's credential and returns the Ref it issued for it; the keeper releases it exactly
	// once.
	Hold func() dispatch.Ref
}

// EntryView is a read-only snapshot of one tracked entry. Exported for tests and replay tooling;
// hosts do not need it.
type EntryView struct {
	Pings, Turn    int
	Prefix         int64
	PingUSD, Spent float64
	Refreshed      int64
	Stopped        bool
	TTL1h          bool // the thread's cache was billed at the one-hour tier
	Policy         Policy
	Tag            string
	MaskedBody     []byte // a copy, still masked; for tests and replay tooling
}

// Keeper runs the keep-alive. One goroutine per host, one map, one lock.
type Keeper struct {
	o    Options
	stop chan struct{}
	done chan struct{}
	// stopOnce and started make Shutdown idempotent and safe on a keeper whose sweep was never
	// launched. The proxy's Handler.Close documents that it is safe to call twice, and it is: a second
	// close of the same channel would panic.
	stopOnce sync.Once
	started  atomic.Bool

	mu    sync.Mutex
	live  map[string]*kaEntry
	bytes int64
	// routeFails, routeTripped and credDead are the route breaker and the dead-credential
	// marks (main #527) — see breaker.go. Keyed with the tenant first, so one tenant can
	// never switch off another's pings.
	routeFails   map[string]int
	routeTripped map[string]time.Time
	credDead     map[string]time.Time
	// routeTrips counts how many times routeFailedLocked has tripped a route, for Stats.
	routeTrips atomic.Int64
	// inflight counts open requests per THREAD (keyed like live): Arrive increments, Record and
	// Depart decrement. A due entry whose own thread has one or more open requests is skipped by
	// Sweep — another thread's open request never pauses it. Port of main's `inflight` field.
	inflight map[string]int
	// ended holds client lifecycle SessionEnd marks, bucketed by tenant first (main #527's
	// clientsignal.go: keeper.ended). A tenant's own bucket is a plain session (or client alias)
	// id -> the instant it was marked ended; see markEndedLocked / endedAfterLocked. Lazily
	// created, so a keeper that never sees a lifecycle signal allocates nothing.
	ended map[string]map[string]time.Time
	// turns counts requests seen per THREAD (keyed like live), so the first-request gate
	// survives the entry's own lifecycle: an entry is dropped the moment the next request arrives, and retired by
	// policy a few minutes later, while "has this session sent a request before?" has to
	// outlive both.
	//
	// ponytail: a plain map with a size bound and no per-key expiry. It holds one int per
	// session id and is cleared wholesale at the bound; give it an LRU only if session churn
	// is ever shown to cost real pings.
	turns map[string]int
	// tier1h remembers, per THREAD (keyed like live), that its last billed write was one-hour. A
	// turn that only READS the cache bills no write, so it carries no tier of its own and
	// inherits this. Written beside turns, so it is bounded and reset exactly as turns is.
	tier1h map[string]bool
	// lastVary is, per THREAD (keyed like live), the output-budget step of the last ping actually
	// handed to the Dispatcher. It outlives the entry, as turns does, so a new entry on the same
	// thread never repeats the step its predecessor last sent (#706, #712 review). Bounded and reset
	// exactly as turns is.
	lastVary map[string]int
	// manualHoldMax overrides ManualHoldMax in tests that need a real, short timer; zero means the constant.
	manualHoldMax time.Duration
	// reasons records why a thread is NOT held (manualhold.go), for the button's explanation.
	reasons map[string]refusal
	// manualAt is when each thread was last manually pinged, for PingNow's rate limit (manual.go).
	manualAt map[string]time.Time

	// Counters, for /stats and for the honest question "is this thing spending money and
	// getting nothing?". Every one of them is a way the mechanism can be useless.
	pings    atomic.Int64
	skipped  atomic.Int64 // due, but a limit or a cap or a gate refused
	failed   atomic.Int64
	wrote    atomic.Int64  // pings that CREATED instead of refreshing — a bug, not a cost
	missed   atomic.Int64  // pings that read nothing — a real cache miss, not a failure
	spentUSD atomic.Uint64 // float64 bits; only ever read as a whole
	// summarySubstituted counts pings replaced by cache_aware_summarizer's own cache-reading
	// call (see fireSummarySubstitute) — accounted toward `pings` as well, since it IS the ping
	// for that idle span, never a second one.
	summarySubstituted atomic.Int64
	// summarySubstituteFailed counts a substitute attempt that fell back to an ordinary ping
	// because the call itself declined, timed out, errored, or came back empty.
	summarySubstituteFailed atomic.Int64
	// summarySubstituteOverBudget counts a substitute attempt refused BEFORE being dispatched
	// because its projected cost exceeded the keep-alive per-ping $ cap.
	summarySubstituteOverBudget atomic.Int64
	// summarySubstitutePhaseMismatch counts a `cache_state: pre_expiry` candidate declined
	// because THIS ping is not actually in the component's own pre-expiry window — routine
	// (every strategy-driven K>1 schedule and every narrower pre_expiry_seconds than the
	// keeper's own Idle produces some of these), not a failure.
	summarySubstitutePhaseMismatch atomic.Int64
	// summarySubstituteNoCandidate counts a due ping for which no keep-alive candidate was
	// registered at all — the ordinary case for the vast majority of sessions (the component
	// not in the pipeline, or no turn has reached registration yet). Counted only when the summary
	// lookup RUNS (#404 point 10): a ping whose claim went to the sweep alone, or found the thread busy,
	// does not look the summary up. Was previously folded into
	// one silent `return false` indistinguishable from the three counters below it.
	summarySubstituteNoCandidate atomic.Int64
	// summarySubstituteCheckpointExists counts a due ping for a session that HAD a registered
	// candidate but already has a checkpoint — the next real turn splices it for free, so
	// substituting here would summarize a span nobody needs.
	summarySubstituteCheckpointExists atomic.Int64
	// summarySubstituteInFlight/ConcurrencyFull count a Dispatch that was itself refused before
	// any model call was made — a candidate existed and passed every check above, but collided
	// with another commission already running for the session, or with the global concurrency
	// bound. Distinct from summarySubstituteFailed's other causes (a call that ran and did not
	// commit), which have their own counters already (CacheAwareSummarizerErrors/Timeouts/Empty).
	summarySubstituteInFlight        atomic.Int64
	summarySubstituteConcurrencyFull atomic.Int64
	// summarySubstituteRefreshPaid/RefreshNotWorthIt count the money gate's own decision (#415)
	// for a `cache_state: pre_expiry` reserve that already exists and has new material: Paid when
	// the expected saving on the eventual cold return (the new tail's own tokens at the model's
	// cache-write rate) covers the refresh's own projected cost, NotWorthIt when it does not and
	// the ping stays an ordinary one instead. Only incremented for a REFRESH (an existing reserve
	// growing a larger one) — creating a session's first reserve has no such test and is not
	// counted by either.
	summarySubstituteRefreshPaid       atomic.Int64
	summarySubstituteRefreshNotWorthIt atomic.Int64
	// sweepSubstituted counts pings replaced by extract_llm_sweep's question (see
	// fireSweepSubstitute), accounted toward `pings` as well, since it IS the ping for that span.
	sweepSubstituted atomic.Int64
	// sweepAskFailed counts a question that left the process but never touched the cache, that
	// never left, or that panicked: the ordinary ping went out instead.
	sweepAskFailed atomic.Int64
	// sweepOverBudget counts a question refused BEFORE sending because its projected cost exceeded
	// the per-ping ceiling.
	sweepOverBudget atomic.Int64
	// sweepBatchesLowered counts sweeps where the per-ping cost ceiling admitted fewer questions than the
	// sweep planned, so fewer were asked (#132).
	sweepBatchesLowered atomic.Int64
}

// New builds a Keeper. o.Dispatcher is required. o.Now defaults to time.Now; o.Go defaults to
// running each ping on its own goroutine, because a round trip must not be made while the map
// lock is held — the snapshot replay passes an inline runner instead, so a five-day simulation
// is deterministic and needs no synchronisation.
func New(o Options) *Keeper {
	if o.Dispatcher == nil {
		panic("keepalive.New: Options.Dispatcher is required")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Go == nil {
		o.Go = func(f func()) { go f() }
	}
	return &Keeper{o: o, stop: make(chan struct{}), done: make(chan struct{}),
		live: map[string]*kaEntry{}, turns: map[string]int{}, tier1h: map[string]bool{}, lastVary: map[string]int{}, inflight: map[string]int{},
		routeFails: map[string]int{}, routeTripped: map[string]time.Time{}, credDead: map[string]time.Time{}}
}

// now is the clock, injected for the same reason.
func (k *Keeper) now() time.Time { return k.o.Now() }

// Now is the keeper's clock, so a host resolving policy reads the same instant the keeper
// schedules by. Nil-safe: a nil keeper reads time.Now.
func (k *Keeper) Now() time.Time {
	if k == nil {
		return time.Now()
	}
	return k.now()
}

// spare reserves one ping's share of the tenant's spare headroom, or nothing when the host
// configured no limiter.
func (k *Keeper) spare(tenant string) (func(), error) {
	if k.o.Spare == nil {
		return func() {}, nil
	}
	return k.o.Spare(tenant, reserveFrac)
}

// Start launches the sweep goroutine. Nil-safe, and a no-op when the kill switch is set.
func (k *Keeper) Start() {
	if k == nil {
		return
	}
	if Disabled() {
		slog.Info("context-guru: cache keep-alive disabled by CONTEXT_GURU_KEEPALIVE")
		close(k.done)
		return
	}
	k.started.Store(true)
	go func() {
		defer close(k.done)
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-k.stop:
				return
			case <-t.C:
				k.Sweep(k.now())
			}
		}
	}()
}

// Shutdown ends the sweep and drops every held body and credential.
//
// It does not wait for pings already in flight. A ping's goroutine may still finish briefly after
// Shutdown returns, and its record1 and the entry's Emitter may run then. A host that closes its
// event sink after Shutdown must tolerate one late report.
func (k *Keeper) Shutdown() {
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
		k.releaseLocked(e)
		e.clear()
		delete(k.live, key)
		offload.ClearKeepAliveCandidate(thread.Key(e.session, e.thread))
	}
	k.bytes = 0
	k.turns = map[string]int{}
	k.tier1h = map[string]bool{}
	k.lastVary = map[string]int{}
}

// clear ZEROIZES an entry's body and credential and cancels its retention deadline. Called on
// every removal path — the next request arriving, the gate refusing, retirement, eviction,
// shutdown — because the two things this holds are the two things it must not hold a moment
// longer than it needs to.
//
// Overwriting rather than dropping the reference is the point: a dropped []byte sits in the heap
// until a collection that may never come, and a dump taken in between still yields it. The body
// is a keeper-owned COPY precisely so it can be overwritten without corrupting a slice something
// else still reads.
func (e *kaEntry) clear() {
	zero(e.body)
	e.body = nil
	e.hdr = nil
	if e.timer != nil {
		e.timer.Stop()
		e.timer = nil
	}
}

// releaseLocked tells the host to drop the credential it Held for e. Exactly once per Hold:
// e.held is cleared here, so a second removal path reaching the same entry is a no-op.
func (k *Keeper) releaseLocked(e *kaEntry) {
	if e.held {
		e.held = false
		k.o.Dispatcher.Release(e.ref)
		e.ref = ""
	}
}

// arrive tells the keeper a real request just started on this session, and reports what the
// keep-alive did during the span that request just ended.
//
// Called at the START of the request, before it goes upstream: an entry left in the map
// while a real request is in flight could be pinged concurrently with it, which is both
// pointless (the real request refreshes the entry itself) and a second request against the
// tenant's concurrency budget.
//
// tag is e.tag (Record.Tag) — in the proxy, which manager-controlled strategy resolved this idle
// span's policy, "" when none did. It is in scope right here and nowhere else once the entry
// is cleared, so this is the one place a real request's credit can be attributed to the
// strategy that earned it, rather than only the tenant's whole lifetime credit.
//
// Only THIS thread's entry is cleared: a subagent's request says nothing about whether the main
// thread is still idle.
//
// Every Arrive must be paired with a Record or a Depart for the same Key, once that request
// ends. An unpaired Arrive — a request that opens here and then neither calls Record nor
// Depart — leaks that thread's open count and stops its pings for good; nothing but Forget
// clears it.
func (k *Keeper) Arrive(key Key) (pings int, refreshed int64, tag string) {
	pings, refreshed, tag, _ = k.ArriveAt(key)
	return pings, refreshed, tag
}

// ArriveAt is Arrive plus the instant the entry's provider-side cache was last known refreshed
// (see kaEntry.refreshedAt) — the real request that started the idle span, or the start of the
// last ping that READ the cache. Zero when there was no entry.
func (k *Keeper) ArriveAt(key Key) (pings int, refreshed int64, tag string, refreshedAt time.Time) {
	if k == nil || key.Session == "" {
		return 0, 0, "", time.Time{}
	}
	id := key.id()
	k.mu.Lock()
	defer k.mu.Unlock()
	k.inflight[id]++
	e, ok := k.live[id]
	if !ok {
		return 0, 0, "", time.Time{}
	}
	pings, refreshed, tag, refreshedAt = e.pings, e.refreshed, e.tag, e.refreshedAt
	k.bytes -= int64(len(e.body))
	k.releaseLocked(e)
	e.clear()
	delete(k.live, id)
	return pings, refreshed, tag, refreshedAt
}

// Depart ends one open request on key's thread. Record calls it for its own key, so a host calls
// it only for a request that ends without a Record (e.g. an error path that never reaches it).
// On a thread with nothing open it does nothing. Nil-safe and a no-op on an empty session, like
// Arrive. Port of main's `departed`.
func (k *Keeper) Depart(key Key) {
	if k == nil || key.Session == "" {
		return
	}
	k.mu.Lock()
	k.departLocked(key.id())
	k.mu.Unlock()
}

// departLocked is Depart's body, for callers that already hold k.mu. k.mu must be held.
func (k *Keeper) departLocked(id string) {
	if k.inflight[id] <= 1 {
		delete(k.inflight, id)
		return
	}
	k.inflight[id]--
}

// Record hands the keeper what it needs to ping this thread, once the request that
// established the cache entry has finished.
//
// startedAt is the instant the upstream request STARTED, per the provider's lifetime rule.
// The body is the exact bytes that went upstream. The entry, the deadline and the turn count
// are per thread; a session override still applies to every thread of its session.
func (k *Keeper) Record(r Record) {
	session, threadID, startedAt, body := r.Key.Session, r.Key.Thread, r.StartedAt, r.Body
	provider, route, status, u, usageOK := r.Provider, r.Route, r.Status, r.Usage, r.UsageOK
	if k == nil || session == "" {
		return
	}
	// Ends the request's own open count on this thread, on EVERY return path below — including
	// an empty body, a zero StartedAt, and a refused status, which the old guard below used to
	// return out from before this ran, leaking the Arrive count. The host's Arrive opened it,
	// and Record is how it closes for the ordinary case. Must run AFTER Record has released
	// k.mu: Depart takes the lock itself, and this defer, registered first, runs LAST (LIFO),
	// after every k.mu.Unlock() registered further down. Port of main's deferred `departed`
	// (43d0fe9e): departing after the hold is stored, not before.
	defer k.Depart(r.Key)
	if len(body) == 0 || startedAt.IsZero() {
		return
	}
	// Only a request the provider ACCEPTED left a cache entry behind. Tracking a 4xx or a
	// failed forward would hold a body and a credential in order to ping a prefix that was
	// never cached — cost with no possible benefit.
	if status < 200 || status >= 300 {
		return
	}
	key := r.Key.id()
	// A client lifecycle signal (host-gated; see MarkEnded) that this session already ended
	// holds nothing further: this request was in flight when the user left, so its cache entry
	// is not worth holding. Matched on Key.Session or r.Client, the id the client's own hooks
	// may name the session by. Port of main's clientsignal.go endedAfter check (#527), kept
	// first among Record's gates as main has it.
	k.mu.Lock()
	ended := k.endedAfterLocked(r.Key.Tenant, r.Key.Session, startedAt) ||
		(r.Client != "" && k.endedAfterLocked(r.Key.Tenant, r.Client, startedAt))
	if ended {
		k.retireLocked(key)
		k.setReasonLocked(r.Key, ReasonNoEntry) // the client said the session is over: nothing is held
	}
	k.mu.Unlock()
	if ended {
		return
	}
	// The kill switch stops RETENTION, not merely pinging. A switch that left bodies and
	// credentials accumulating while refusing to use them would be the worst of both.
	if Disabled() {
		k.retire(key)
		return
	}
	// Consent is re-read on EVERY request rather than trusted from when the hold began, and
	// withdrawing it retires what is already held: the host resolves r.Policy per request (in
	// cmd/context-guru-proxy, from the tenancy, the manager strategy and the session override, and
	// a zero Policy when there is no audit sink), so an account that turns the setting off stops
	// being retained on its very next request; anything held for a session that goes quiet
	// instead is dropped by the hard deadline below, within (K+1)x X.
	pol := r.Policy
	model := gjson.GetBytes(body, "model").String()
	// A request that opted in to the Ping button (Policy.ManualHold) is held even where the schedule
	// is off, or where the schedule's own gates refuse the thread; it just never gets a schedule.
	// manualOnly is decided once the gates below have run, so every refusal can name its reason.
	if !pol.On() && !pol.ManualHold {
		k.mu.Lock()
		k.setReasonLocked(r.Key, ReasonNotOptedIn)
		k.mu.Unlock()
		k.retire(key)
		return
	}
	refuse := func(reason string) {
		if pol.ManualHold {
			k.mu.Lock()
			k.setReasonLocked(r.Key, reason)
			k.mu.Unlock()
		}
	}
	// The route breaker and the dead-credential mark (main #527, see breaker.go) refuse a
	// NEW hold outright: a route failing in a row, or a credential already marked dead on
	// this model, must not accumulate another body and another held credential while it
	// waits out its hour. Any OLDER entry for this thread is dropped too, same as every
	// other refusal path above.
	//
	// A tripped route increments no counter here (main L850-853: a retire only) — the trip
	// already counted itself at RouteTrips when it fired, and Skipped is reserved for a gate
	// that refuses something that would otherwise have been tracked. A dead credential DOES
	// count as Skipped (main L886-891): unlike a route trip, nothing else ever counts it.
	now := k.now()
	k.mu.Lock()
	routeTripped := k.routeIsTrippedLocked(routeKey(r.Key.Tenant, r.Endpoint, r.Route, model), now)
	credDead := k.credIsDeadLocked(r.Key.Tenant, r.Cred, model, now)
	if routeTripped || credDead {
		k.retireLocked(key)
		if pol.ManualHold {
			k.setReasonLocked(r.Key, ReasonUnavailable)
		}
	}
	k.mu.Unlock()
	if routeTripped || credDead {
		if credDead {
			k.skipped.Add(1)
		}
		return
	}
	if len(body) > MaxBodyBytes {
		refuse(ReasonTooLarge)
		k.retire(key)
		k.skipped.Add(1)
		return
	}
	// Nothing to keep alive unless the provider honours explicit breakpoints and this
	// request actually established or read an entry. A request that wrote and read nothing
	// has no prefix worth a ping.
	if !eligible(provider, model, route) || !relayServesClaude(provider, r.Endpoint, model) {
		refuse(ReasonUnsupported)
		k.retire(key)
		return
	}
	if usageOK && u.CacheRead == 0 && u.CacheWrite == 0 {
		refuse(ReasonNoEntry) // nothing was cached by this request, so there is nothing to keep warm
		k.retire(key)
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
		refuse(ReasonUnsupported)
		k.retire(key)
		k.skipped.Add(1)
		return
	}
	// The cache lifetime this thread was granted, from the traffic (see ResolveTier). Read here, written
	// below beside turns: a request the gates above refuse must not grow tier1h.
	k.mu.Lock()
	prev1h := k.tier1h[key]
	k.mu.Unlock()
	ttl1h := ResolveTier(body, u, usageOK, prev1h) && provider != bschemas.OpenAI
	// Only a policy that is ON swaps to the one-hour schedule. ForOneHour never sets KeepAlive, so
	// `none` stays none either way; the gate keeps a manual-only hold's zero policy untouched.
	if ttl1h && pol.On() {
		pol = pol.ForOneHour()
	}
	// A keeper-OWNED copy, not the slice serve just used. It costs one allocation per tracked
	// session — and tracked sessions are gated down to a handful — in exchange for being able to
	// overwrite the bytes on release without corrupting anything else that might still read
	// them. Zeroizing a shared slice is the kind of cleverness that produces a mystery bug in a
	// month, and the earlier zero-copy retention was a micro-optimisation in the wrong place.
	owned := append([]byte(nil), body...)
	mask(owned)
	// What the provider billed for this request's prefix, in its own units — the size of the
	// entry a ping would refresh. Both the gate and the projected cost read it, so a request
	// that reported no usage is not pingable: without it there is no honest estimate of either.
	prefix := u.CacheRead + u.CacheWrite
	hdr := PingHeaders(r.Header)
	emitter := r.Emitter
	if emitter == nil {
		emitter = components.NopEmitter{}
	}
	e := &kaEntry{
		key: r.Key, tenant: r.Key.Tenant, session: session, client: r.Client, thread: threadID, startedAt: startedAt,
		cred: r.Cred,
		body: owned, endpoint: r.Endpoint,
		st:       r.Store,
		emitter:  emitter,
		provider: provider, model: model, route: route, preset: r.Preset,
		agent: r.Agent, pol: pol, ttl1h: ttl1h, prefix: prefix, turnRead: u.CacheRead, stopReason: u.StopReason,
		toolUseNames: r.ToolUseNames, usageOK: usageOK,
		pingUSD: k.projectedPingUSD(model, prefix, route, r.Endpoint),
		hdr:     hdr, tag: r.Tag,
	}
	e.refreshedAt = startedAt // the real request refreshed the provider's entry
	k.mu.Lock()
	defer k.mu.Unlock()
	// The turn count outlives the entry, which is dropped on the next request's arrival. It is
	// incremented for EVERY request, including the ones the gate then refuses to track — that is
	// what lets a session's second request be recognised as such.
	k.turns[key]++
	e.turn = k.turns[key] - 1
	// A write bills a tier; a read-only turn bills none and leaves the remembered one alone.
	if usageOK && (u.CacheWrite > 0 || ttl1h) {
		k.tier1h[key] = ttl1h
	}
	if len(k.turns) > MaxTurnKeys {
		// Reset both, keeping the current thread: dropping its tier here would send its next
		// read-only turn back to the 5-minute schedule.
		k.turns = map[string]int{key: k.turns[key]}
		k.tier1h = map[string]bool{}
		// Keep the step of every thread that still holds a live entry, and of this one: a live
		// thread that lost its step would send its next ping at step 0, the same bytes as a ping 1
		// it may have just sent (#712 review). k.live is bounded, so this stays bounded too.
		old := k.lastVary
		k.lastVary = make(map[string]int, len(k.live)+1)
		for id := range k.live {
			if v, ok := old[id]; ok {
				k.lastVary[id] = v
			}
		}
		if v, ok := old[key]; ok {
			k.lastVary[key] = v
		}
		if usageOK && (u.CacheWrite > 0 || ttl1h) {
			k.tier1h[key] = ttl1h
		}
	}
	// Drop any previous entry either way: it is stale now, and if the gate refuses this request
	// its body and credential must not be left behind.
	if prev, ok := k.live[key]; ok {
		k.bytes -= int64(len(prev.body))
		k.releaseLocked(prev) // the OLD credential goes before the new one is held
		prev.clear()
		delete(k.live, key)
	}
	// The automatic gates decide whether this thread is pinged on a schedule. A request that opted in
	// to the button and fails them (or has no schedule at all) is still held, as a manual-only entry:
	// no schedule, a hard life of 300 s (59 min on a one-hour thread). Only the cost ceiling is kept, because a press spends money too.
	if !pol.On() || !e.pingable() {
		if !pol.ManualHold {
			reason := ReasonBelowLimits
			if e.pingUSD > pol.Ceiling() {
				reason = ReasonOverBudget
			}
			k.setReasonLocked(r.Key, reason)
			k.skipped.Add(1)
			e.clear() // hold no body and no credential for a session we will not ping
			return
		}
		if e.pingUSD > pol.Ceiling() {
			k.setReasonLocked(r.Key, ReasonOverBudget)
			k.skipped.Add(1)
			e.clear()
			return
		}
		e.manualOnly = true
		e.holdFor = k.holdFor(ttl1h)
		e.ceiling = pol.Ceiling()
		e.pol = Policy{ManualHold: true} // off: due, Plans, PendingPings and Sweep all read pol.On()
	}
	k.clearReasonLocked(r.Key)
	k.live[key] = e
	k.bytes += int64(len(owned))
	if r.Hold != nil {
		e.ref = r.Hold()
		e.held = true
	}
	if e.manualOnly {
		e.timer = time.AfterFunc(e.holdFor, func() { k.expireManual(key) })
		k.evictManualLocked(r.Key.Tenant)
		k.evictLocked()
		return
	}
	// The hard deadline. (K+1) x Idle is the longest the policy could ever still want this
	// entry: K pings each restarting the clock, plus one final Idle to notice the last one
	// bought nothing. After that there is no legitimate use, so the material goes whether or not
	// anything else in this process is awake.
	e.timer = time.AfterFunc(e.hold(), func() { k.expireAuto(key) })
	k.evictLocked()
}

// nextVaryLocked picks the output-budget step for a ping about to be dispatched on thread key,
// and records it. Called at DISPATCH time only, never when a ping merely falls due: a cycle that
// sends nothing (a substitute took it, the limiter refused it) must not use up a step, or two pings
// sent in a row could repeat one another (#712 review).
//
// The next step after the thread's last one, so two pings in a row on one thread always differ.
// A thread's first ping is step 0, the bytes every ping sent before #706.
//
// NOT covered: two threads holding a byte-identical body (a fork, agent-team teammates) step through
// the same values, so their pings can match. That needs the threads to ping in lockstep with the
// same transcript; a turn on either thread changes its body and ends it.
// Caller holds k.mu.
func (k *Keeper) nextVaryLocked(key string) int {
	v, ok := k.lastVary[key]
	if ok {
		v = (v + 1) % pingVary
	}
	k.lastVary[key] = v
	return v
}

// price asks the host for a model's rates. Never nil-checked: the Dispatcher is required.
func (k *Keeper) price(endpoint, model string, prefix int64) (Price, bool) {
	return k.o.Dispatcher.Price(context.Background(), endpoint, model, prefix)
}

// projectedPingUSD is what one ping on this prefix will cost: the prefix at the model's own
// cache-read rate, plus the route's output budget. Computed at record time so the guard can
// refuse an expensive ping BEFORE sending it rather than reporting it afterwards.
//
// Zero when the model is not priced, which lets the ping through — refusing to protect a cache
// because a price list is incomplete would be the wrong failure, and the spend still lands on
// its own dashboard row either way.
func (k *Keeper) projectedPingUSD(model string, prefix int64, route, endpoint string) float64 {
	if model == "" || prefix <= 0 {
		return 0
	}
	price, ok := k.price(endpoint, model, prefix)
	if !ok || price.Zero() {
		return 0
	}
	return float64(prefix)*price.CacheRead + float64(pingOutputBudget(route))*price.Output
}

// summaryOutputBudgetGuess is the assumed output size of a keep-alive-substituted summary call,
// used ONLY to project its cost against the keep-alive $ cap before dispatching it — never a cap
// on the real call, which runs under cache_aware_summarizer's own full timeout and token budget.
//
// Unmeasured, like pingUSD's own disclaimer two paragraphs up: a real summary's length depends on
// the transcript and the component's configured prompt, and this is new as of #400 so there is no
// telemetry on this specific call's own output size yet. 2000 tokens is a round, conservative
// guess sized to make the error fail in the cheap direction — overestimating a verbose model's
// true output merely sends an ordinary ping instead of substituting one, never the reverse.
const summaryOutputBudgetGuess = 2000

// projectedSummaryUSD mirrors projectedPingUSD for the keep-alive substitute call: the prefix at
// the model's own cache-read rate, plus an assumed output budget rather than the ping's 1-token
// one, since this call generates a full summary.
func (k *Keeper) projectedSummaryUSD(model, endpoint string, prefix int64) float64 {
	if model == "" || prefix <= 0 {
		return 0
	}
	price, ok := k.price(endpoint, model, prefix)
	if !ok || price.Zero() {
		return 0
	}
	return float64(prefix)*price.CacheRead + float64(summaryOutputBudgetGuess)*price.Output
}

// expectedRefreshSavingUSD is the dollar value a refresh folding tailTokens of new material into
// a `cache_state: pre_expiry` reserve is expected to earn back: those tokens would otherwise be
// written, at the model's cache-WRITE rate, on the cold return the reserve exists to cheapen.
// Issue #415's own "safe lower bound": it deliberately omits any saving from later turns reading
// the larger reserve instead of the smaller one, since how many turns follow a cold return is not
// knowable at ping time.
//
// Zero when the model is not priced, for the same reason projectedPingUSD/projectedSummaryUSD are
// — an incomplete price list must not block a feature it cannot afford to evaluate, and
// fireSummarySubstitute's own comparison (saving < cost) already fails toward "ping normally"
// whenever cost cannot be shown to be zero too.
func (k *Keeper) expectedRefreshSavingUSD(model, endpoint string, prefix int64, tailTokens int) float64 {
	if model == "" || tailTokens <= 0 {
		return 0
	}
	price, ok := k.price(endpoint, model, prefix)
	if !ok || price.Zero() {
		return 0
	}
	return float64(tailTokens) * price.CacheWrite
}

// retire releases one session's held material now: zeroized, deadline cancelled, entry gone.
// Idempotent and safe on a session that was never tracked, which is what lets every refusal path
// call it unconditionally.
func (k *Keeper) retire(key string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.retireLocked(key)
}

// retireLocked is retire's body, for callers that already hold k.mu (the route breaker's gate
// in Record). k.mu must be held.
func (k *Keeper) retireLocked(key string) {
	e, ok := k.live[key]
	if !ok {
		return
	}
	k.bytes -= int64(len(e.body))
	k.releaseLocked(e)
	e.clear()
	delete(k.live, key)
	// The keep-alive substitute registry (cache_aware_summarizer.go's own, session-keyed map)
	// is not part of this struct and nothing else ever cleared it on this exit — a session that
	// retired here could still hold a candidate forever, which is how that registry used to fill
	// permanently after 2,048 sessions ever passed through. Every exit from k.live must drop it
	// too; see offload.ClearKeepAliveCandidate's own doc comment.
	offload.ClearKeepAliveCandidate(thread.Key(e.session, e.thread))
}

// RetireSession releases every thread of one session: the primary thread's key and every key
// under it, matched on Key.Session or a client alias (see RetireSessionStartedBefore). The
// separator is NUL, which no session id contains, so a prefix can never reach into another
// session.
func (k *Keeper) RetireSession(tenantID, session string) {
	k.RetireSessionStartedBefore(tenantID, session, time.Time{})
}

// forget releases everything held for one tenant, for the paths that end an account's authority
// rather than change its mind: deletion, token revocation, disablement. Consent withdrawal is
// handled in record, and the hard deadline bounds anything that goes quiet — but this repo holds
// the stronger invariant explicitly (tenant.TestDeleteCascadesEveryCredential), and the keeper is
// a credential store that cascade did not know about.
//
// ponytail: called from the four control-plane paths that end a hold's authority or its consent;
// a fifth would have to remember. Delete, revoke-token and disable end AUTHORITY — the credential
// stops working. PUT /api/me ends CONSENT — the credential still works and the hold is still
// authorized, but permission to use it is gone; that distinction is why ctlRevokeSession is
// deliberately not in the set (a browser cookie is neither). Move this inside TenantSource.Forget
// if revoke and disable ever start invalidating the cached tenancy too, which would make one hook
// cover all of them.
func (k *Keeper) Forget(tenantID string) {
	if k == nil {
		return
	}
	prefix := tenantID + "\x00"
	k.mu.Lock()
	defer k.mu.Unlock()
	for key, e := range k.live {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		k.bytes -= int64(len(e.body))
		k.releaseLocked(e)
		e.clear()
		delete(k.live, key)
		delete(k.turns, key)
		delete(k.tier1h, key)
		offload.ClearKeepAliveCandidate(thread.Key(e.session, e.thread))
	}
	// Every step the tenant's threads left, including threads with no live entry: lastVary has no
	// other bound while Forget keeps turns under MaxTurnKeys (#712 review).
	for key := range k.lastVary {
		if strings.HasPrefix(key, prefix) {
			delete(k.lastVary, key)
		}
	}
	// Client lifecycle end marks (see lifecycle.go) are bucketed by tenant already, so this is
	// a single delete rather than a prefix scan.
	delete(k.ended, tenantID)
	k.dropReasonsLocked(tenantID, "")
	// The route breaker and dead-credential marks are also keyed with the tenant first (see
	// breaker.go), so they are cleared the same way: nothing of this tenant's survives Forget.
	for r := range k.routeFails {
		if strings.HasPrefix(r, prefix) {
			delete(k.routeFails, r)
		}
	}
	for r := range k.routeTripped {
		if strings.HasPrefix(r, prefix) {
			delete(k.routeTripped, r)
		}
	}
	for c := range k.credDead {
		if strings.HasPrefix(c, prefix) {
			delete(k.credDead, c)
		}
	}
	for id := range k.inflight {
		if strings.HasPrefix(id, prefix) {
			delete(k.inflight, id)
		}
	}
}

// evictLocked enforces both bounds by dropping the entries whose deadline is furthest away
// — the ones with the longest still to wait, and therefore the least imminent value.
func (k *Keeper) evictLocked() {
	for len(k.live) > MaxSessions || k.bytes > MaxBytes {
		// Manual-only holds go before any automatic one, whichever tenant's: they have no schedule and
		// protect nothing a ping is doing. Among the candidates the entry whose ping is least imminent
		// goes, as before.
		var worstKey string
		var worst time.Time
		var worstManual bool
		for key, e := range k.live {
			better := worstKey == "" || (e.manualOnly && !worstManual) ||
				(e.manualOnly == worstManual && e.startedAt.After(worst))
			if better {
				worstKey, worst, worstManual = key, e.startedAt, e.manualOnly
			}
		}
		if worstKey == "" {
			return
		}
		e := k.live[worstKey]
		k.bytes -= int64(len(e.body))
		k.releaseLocked(e)
		e.clear()
		delete(k.live, worstKey)
		offload.ClearKeepAliveCandidate(thread.Key(e.session, e.thread))
		k.skipped.Add(1)
	}
}

// Sweep fires every due ping. Returns how many it started. Exported for tests and replay tooling
// (Start runs it on a ticker); hosts do not need it.
//
// The map lock is held only to select and to stamp: a ping is an HTTP round trip and holding
// the lock across it would block every arriving request on this map.
func (k *Keeper) Sweep(now time.Time) int {
	if k == nil {
		return 0
	}
	// The host's own per-tick hygiene (in the proxy: expiring session overrides, so that map
	// cannot grow on the strength of authorizations nobody withdrew), outside the keeper lock.
	if k.o.OnTick != nil {
		k.o.OnTick(now)
	}
	k.mu.Lock()
	var due []pingJob
	for key, e := range k.live {
		// EAGER release of a span that can do nothing more, so material goes as soon as it is
		// useless rather than at the outer deadline. The GUARANTEE is kaEntry.timer, which fires
		// on schedule whether or not this loop is running; this is the earlier of the two, and
		// having both is deliberate — a security bound must not depend on a liveness loop, and a
		// liveness loop should not wait for the security bound.
		if e.manualOnly {
			// Never pinged. Released at its deadline, here as well as by its own timer.
			if e.manualExpired(now) {
				k.setReasonLocked(e.key, ReasonExpired)
				k.bytes -= int64(len(e.body))
				k.releaseLocked(e)
				e.clear()
				delete(k.live, key)
				offload.ClearKeepAliveCandidate(thread.Key(e.session, e.thread))
			}
			continue
		}
		if e.stopped || e.pings >= e.pol.MaxPings || !e.pingable() {
			if !now.Before(e.startedAt.Add(e.pol.Idle)) {
				k.setReasonLocked(e.key, e.autoReleaseReason())
				k.bytes -= int64(len(e.body))
				k.releaseLocked(e)
				e.clear()
				delete(k.live, key)
			}
			continue
		}
		if !e.due(now) || k.inflight[key] > 0 {
			continue
		}
		// The route breaker and the dead-credential mark can trip or land WHILE an entry is
		// sitting idle waiting for its next ping, not only at Record time — so this is
		// checked again here, right before a ping would be sent.
		if k.routeIsTrippedLocked(routeKey(e.tenant, e.endpoint, e.route, e.model), now) ||
			k.credIsDeadLocked(e.tenant, e.cred, e.model, now) {
			e.stopped = true
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
		prevStartedAt := e.startedAt
		e.startedAt = now
		e.pings++
		// An UNMASKED copy for this ping, zeroized by fire when it is done. The entry's own copy
		// stays masked, so the long-lived hold never contains a readable transcript.
		raw := append([]byte(nil), e.body...)
		mask(raw)
		due = append(due, pingJob{e: e, raw: raw, hdr: e.hdr.Clone(), key: e.key, ref: e.ref,
			route: e.route, provider: e.provider, model: e.model,
			tenant: e.tenant, session: e.session, thread: e.thread, st: e.st, emitter: e.emitter,
			ping: e.pings, prevStartedAt: prevStartedAt})
	}
	k.mu.Unlock()

	for _, j := range due {
		j := j
		k.o.Go(func() { k.fire(j) })
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
	raw []byte
	hdr http.Header
	// ref, route, provider and model are what the Dispatcher needs to send this ping; the
	// credential is the host's, looked up by ref inside Do. key identifies the entry.
	key             Key
	ref             dispatch.Ref
	route           string
	provider        bschemas.ModelProvider
	model           string
	tenant, session string
	thread          string
	// st is the tenant's store, copied here for the same reason everything else is: the job runs
	// on its own goroutine and must hold a copy rather than a pointer into keeper state.
	st store.Store
	// emitter is e.emitter, copied out under the keeper's lock for the same reason st is: the
	// job runs on its own goroutine and must hold a copy rather than reach back into the entry.
	emitter components.Emitter
	ping    int
	// vary is the output-budget step this ping was actually sent with (Keeper.nextVaryLocked), set
	// in fire at dispatch time and read by record1 for the audit row.
	vary int
	// prevStartedAt is e.startedAt as it stood BEFORE this sweep tick overwrote it with `now` —
	// i.e. when the cache entry this ping is about to refresh was last written or read. Needed
	// for fireSummarySubstitute's own cache-phase check: by the time fire() runs, e.startedAt is
	// already this ping's own (not-yet-sent) start, so reading it there would compute "how long
	// until this ping expires" instead of "how close was this ENTRY to expiring" — answering a
	// different, always-nearly-full-TTL question.
	prevStartedAt time.Time
}

// fire sends one ping and accounts for it. Always fails open and quietly: the agent is not
// waiting on this, so there is nobody to return an error to and nothing that a retry could
// help. A failed ping is logged, counted, and forgotten.
func (k *Keeper) fire(j pingJob) {
	// The job carries its own unmasked copy of the body, so it owns the wipe. Deferred so the
	// limiter refusal, the pingBody failure and a panic all release it.
	defer func() {
		zero(j.raw) // the unmasked body copy this ping was handed
	}()
	// The tenant's own limits apply, and a ping only ever uses SLACK. Refusing rather than
	// queueing is the requirement: a queued ping would sit in front of a real request the
	// agent IS waiting on. AcquireSpare reserves headroom so a ping cannot consume the last
	// slot or the last request of the minute.
	release, err := k.spare(j.key.Tenant)
	if err != nil {
		k.skipped.Add(1)
		return
	}

	// Let cache_aware_summarizer stand in for this ping when it has material registered for the
	// session: same cache-read refresh, plus a compaction instead of one output token. Never
	// double-pinged — on success this returns, and the ordinary ping below never runs for this
	// due cycle; on any kind of "no" it falls through to the ordinary ping, fail open.
	//
	// RELEASED BEFORE THE CALL, not deferred to the end of fire(): the substitute's own call can
	// run for up to cacheAwareTimeout (300 s), against an ordinary ping's single round trip of a
	// second or two. AcquireSpare's job is only to confirm the tenant currently has slack to
	// spare one more concurrent request; holding the slot for the whole substitute call would
	// starve the tenant's real traffic of that headroom for minutes instead of a moment, on every
	// due cycle this fires for. The substitute call needs no slot of its own to stay bounded — it
	// already shares cache_aware_summarizer's own, separate concurrency gate (summarySlots in
	// cache_aware_async.go), sized for exactly this kind of call.
	release()
	// ONE substitute per ping, per thread (#404 point 10). The claim names who may take the ping and
	// holds the thread until the attempt is over. The summary owns the ping when its trigger fired; the
	// sweep asks only when it did not. They never both run from the keeper. One gap remains: the in-flight
	// check is check-then-act, so a detached summary that a real request starts after the look can
	// overlap the sweep's question (see ClaimPing; PR C decides whether to close it).
	if k.fireSubstitute(j) {
		return
	}

	// Falling through to an ordinary ping needs its own slot — the one above was already
	// released and only ever covered the eligibility check, not this request.
	release, err = k.spare(j.key.Tenant)
	if err != nil {
		k.skipped.Add(1)
		return
	}
	defer release()

	k.mu.Lock()
	j.vary = k.nextVaryLocked(j.key.id())
	k.mu.Unlock()
	body, ok := pingBodyN(j.raw, j.route, j.vary)
	if !ok {
		k.markStopped(j.e)
		k.skipped.Add(1)
		return
	}
	start := k.now()
	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	resp, err := k.o.Dispatcher.Do(ctx, dispatch.Request{Purpose: dispatch.PurposeKeepAlivePing,
		Ref: j.ref, Provider: j.provider, Method: http.MethodPost, Path: j.route, Header: j.hdr,
		Body: body})
	cancel()
	zero(body)
	status := resp.Status
	var u Usage
	if err == nil {
		u = PingUsage(resp.Body)
	}
	ms := float64(k.now().Sub(start).Microseconds()) / 1000.0
	k.pings.Add(1)
	if err != nil {
		k.failed.Add(1) // a transport error: counted, not stopped, not a breaker failure
		retry := k.giveBackFailedPing(j)
		slog.Debug("context-guru: cache keep-alive ping failed",
			"tenant", tenantLabel(j.tenant), "session", j.session, "thread", j.thread, "err", err,
			"retry", retry)
		return
	}
	switch k.noteOutcome(j, status, u) {
	case outcomeTransient:
		k.record1(j, u, status, ms, start) // the audit row; the next ping may work
		return
	case outcomeFailed:
		return
	case outcomeFailedRecorded:
		k.record1(j, u, status, ms, start)
		return
	case outcomeMissed:
		cost := k.record1(j, u, status, ms, start)
		slog.Info("context-guru: cache keep-alive ping missed (entry had expired); cache re-written",
			"tenant", tenantLabel(j.tenant), "session", j.session, "thread", j.thread,
			"cache_write", u.CacheWrite, "fresh_input", u.FreshInput, "cost_usd", cost)
		return
	}
	cost := k.record1(j, u, status, ms, start)
	slog.Debug("context-guru: cache keep-alive ping",
		"tenant", tenantLabel(j.tenant), "session", j.session, "thread", j.thread, "ping", j.ping,
		"cache_read", u.CacheRead, "cache_write", u.CacheWrite, "output", u.Output,
		"cost_usd", cost, "ms", ms)
}

// markStopped stops pinging one session, if it is still tracked.
func (k *Keeper) markStopped(e *kaEntry) {
	k.mu.Lock()
	defer k.mu.Unlock()
	e.stopped = true
}

// outcome is what one ping's answer means for its entry. The order of checks is main's (#527).
type outcome int

const (
	outcomeRead outcome = iota
	outcomeMissed
	outcomeTransient
	outcomeFailed         // stopped, no audit row
	outcomeFailedRecorded // stopped, with an audit row
	outcomeStopped        // the guard: wrote more than it read, or missed twice running
)

// noteOutcome classifies a ping's answer and applies it to the entry and the counters.
func (k *Keeper) noteOutcome(j pingJob, status int, u Usage) outcome {
	// A 401 or 403 against a credential the host could identify (j.e.cred != "") is the
	// credential's own fault, not the route's: mark IT dead rather than tripping the breaker,
	// so another credential on the same route keeps pinging. "" (a server-held key) falls
	// through to the ordinary status >= 400 branch below, which feeds the route breaker instead
	// — see TestEmptyTenantAndCredUseTheRouteBreaker.
	if (status == http.StatusUnauthorized || status == http.StatusForbidden) && j.e.cred != "" {
		k.failed.Add(1)
		k.markStopped(j.e)
		k.mu.Lock()
		k.markCredDeadLocked(j.tenant, j.e.cred, j.e.model, status, k.now())
		k.mu.Unlock()
		slog.Debug("context-guru: cache keep-alive ping refused the credential; not pinging with it again",
			"tenant", tenantLabel(j.tenant), "session", j.session, "thread", j.thread, "status", status)
		return outcomeFailed
	}
	switch {
	case status == http.StatusTooManyRequests || status >= 500:
		k.skipped.Add(1)
		slog.Debug("context-guru: cache keep-alive ping hit a transient error; the next ping comes a full idle period later",
			"tenant", tenantLabel(j.tenant), "session", j.session, "thread", j.thread, "status", status)
		return outcomeTransient
	case status >= 400:
		k.failed.Add(1)
		k.markStopped(j.e)
		k.routeFailed(j)
		slog.Debug("context-guru: cache keep-alive ping refused; not pinging this session again",
			"tenant", tenantLabel(j.tenant), "session", j.session, "thread", j.thread, "status", status)
		return outcomeFailed
	case u.FreshInput == 0 && u.CacheRead == 0 && u.CacheWrite == 0:
		k.failed.Add(1)
		k.markStopped(j.e)
		k.routeFailed(j)
		slog.Warn("context-guru: cache keep-alive ping got no usable answer; not pinging this session again",
			"tenant", tenantLabel(j.tenant), "session", j.session, "thread", j.thread, "status", status)
		return outcomeFailedRecorded
	}
	k.mu.Lock()
	k.routeOKLocked(routeKey(j.tenant, j.e.endpoint, j.e.route, j.e.model))
	k.mu.Unlock()
	if u.CacheRead == 0 && k.notePingMiss(j.e) {
		k.missed.Add(1)
		return outcomeMissed
	}
	if u.CacheWrite > u.CacheRead || u.CacheRead == 0 {
		k.wrote.Add(1)
		k.markStopped(j.e)
		slog.Error("context-guru: cache keep-alive ping did not read the held prefix (it wrote more than it read, "+
			"or missed twice running); not pinging this session again",
			"tenant", tenantLabel(j.tenant), "session", j.session, "thread", j.thread,
			"cache_write", u.CacheWrite, "cache_read", u.CacheRead)
		return outcomeStopped
	}
	k.notePingRead(j.e)
	return outcomeRead
}

// notePingMiss counts a miss on e and reports whether it is the FIRST in a row.
func (k *Keeper) notePingMiss(e *kaEntry) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	e.missed++
	return e.missed == 1
}

// notePingRead resets e's run of misses.
func (k *Keeper) notePingRead(e *kaEntry) {
	k.mu.Lock()
	e.missed = 0
	e.retried = false
	k.mu.Unlock()
}

// cacheTTL is how long the provider keeps this entry's cache after a read: 30 minutes on an
// OpenAI Responses thread (the GPT-5.6+ minimum; ttl1h is never set there), else 1 hour or
// 5 minutes by the tier the provider billed.
func (e *kaEntry) cacheTTL() time.Duration { return cacheLifetime(e.provider, e.ttl1h) }

// cacheLifetime is THE rule for how long a provider keeps a thread's cache after a read, so the
// retry guard (cacheTTL) and the substitute's phase check (cachePhaseAt) cannot disagree (#715):
// 30 minutes on OpenAI (the GPT-5.6+ minimum; the one-hour tier is never set there), else 1 hour
// on the one-hour tier, else 5 minutes.
func cacheLifetime(provider bschemas.ModelProvider, ttl1h bool) time.Duration {
	switch {
	case provider == bschemas.OpenAI:
		return modelinfo.OpenAIMinimumCacheTTL
	case ttl1h:
		return time.Hour
	default:
		return 5 * time.Minute
	}
}

// giveBackFailedPing undoes Sweep's stamp for a ping that failed in transport, so the next sweep
// tick sends it again, and reports whether it did. A failed ping refreshed nothing, but Sweep had
// already moved e.startedAt to the ping's own start and spent one of e.pol.MaxPings. Left so, the
// next ping comes a full Idle after the FAILED one, past the cache's lifetime, and has to re-write
// the prefix. Seen live (#703): the last refresh at 16:06:18, a ping that failed at 16:11:49, and
// the next ping at 16:15:26, 548 s after the refresh, re-wrote 903,705 tokens for $4.52.
//
// Once per run of failures (e.retried), and only while the cache entry the last good read or
// request refreshed is still alive: after that a retry can only write, which costs more than it
// saves. A ping that times out at pingTimeout usually returns after that point, so this mostly
// helps a failure that comes back fast (EOF, a refused or reset connection).
func (k *Keeper) giveBackFailedPing(j pingJob) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	e := j.e
	if e.stopped || e.retried || e.pings != j.ping {
		return false
	}
	if !k.now().Before(e.refreshedAt.Add(e.cacheTTL())) {
		return false
	}
	e.startedAt = j.prevStartedAt
	e.pings--
	e.retried = true
	return true
}

// record1 books one ping: the session's running spend, the process counter, and whatever
// audit row the host's Emitter chooses to keep. Returns the ping's cost.
//
// The row is the whole reason this is shippable. It carries the ping's own tokens and cost,
// marked keepalive, attributed to the tenant and the session — so the money this mechanism
// spends is visible in the same ledger as the money it saves, and an account can see it
// without being told.
//
// This function used to build a dash.Event and call k.h.rec.Record(ev) directly — the one
// piece of host-specific machinery wired into the keeper itself (issue #382). It now computes
// the ping's own cost (the figure its OWN bookkeeping needs: e.spent, k.spentUSD, the debug
// log line in fire) without any sink's help, and separately reports a components.KeepAliveReport
// through j.emitter for whichever sink the host configured — cmd/context-guru-proxy wires a
// dashEmitter that writes the identical dash.Event this used to build inline.
//
// startedAt is when THIS ping's request began, not when it completed. The provider's cache lifetime
// runs from request start, which is the same anchor kaEntry.startedAt uses and for the same reason.
func (k *Keeper) record1(j pingJob, u Usage, status int, ms float64, startedAt time.Time) float64 {
	e := j.e
	// The identifying fields are read under the lock with everything else: they are set once
	// at record time and never mutated, but a retired entry is concurrent with this goroutine
	// and the race detector is right to insist.
	k.mu.Lock()
	model, provider, route, preset, agent := e.model, e.provider, e.route, e.preset, e.agent
	appliedStrategy, endpoint, prefix := e.tag, e.endpoint, e.prefix
	k.mu.Unlock()
	// A ping is not agent traffic, so it gets no cache-miss attribution and never touches
	// the session-recency map: doing so would re-date the session and make the NEXT real
	// request's gap read as four minutes instead of the twenty it actually was, hiding the
	// very thing this mechanism is here to demonstrate.
	//
	// BUT IT DOES REFRESH THE PROVIDER'S ENTRY, and until #243 nothing told the cache-liveness
	// clock. A ping's whole job is to READ the cached prefix, and a read resets that entry's TTL —
	// so after this the entry is warm again. With only real requests writing the clock, a
	// kept-alive session's idle time grew without bound while the provider held the entry alive,
	// and summarize's gate concluded the cache was long dead: a strict clock test said "certainly
	// cold", the then-shipped `pre_expiry_or_cold` permitted a rewrite, and the component compacted
	// a LIVE prefix — on exactly the sessions someone is paying pings to protect. Those cold-gated
	// states have since been withdrawn, so the reader that made this expensive is gone; what still
	// reads the clock is `cache_state: pre_expiry`, whose question is whether the prefix is LIVE.
	//
	// TWO CLOCKS, TWO QUESTIONS, and that is why this is not a contradiction of the paragraph
	// above. "Is the provider still holding this prefix?" counts pings; "how long was the USER
	// away?" must not. The recency map answers the second and stays untouched; this writes only
	// the first, so the dashboard's idle-gap statistics are unaffected.
	//
	// GATED ON CacheRead > 0, because only a ping that actually read the entry refreshed it. A ping
	// that WROTE (the entry was already gone) refreshed nothing that was there before, and claiming
	// otherwise would make the clock lie in the expensive direction.
	//
	// Anchored on the ping's START, not its completion, for the same reason kaEntry.startedAt is:
	// the provider's lifetime runs from request start.
	//
	// j.raw is the UNMASKED body copy, and reading it here is safe: fire zeroizes it in a defer that
	// runs after record1 returns. It is read rather than the masked kaEntry.body because the alias is
	// derived from the transcript head, which a masked copy does not contain.
	if status >= 200 && status < 300 && u.CacheRead > 0 {
		apply.RecordCacheTouch(j.st, j.tenant, j.raw, provider, startedAt.UnixMilli())
	}
	cost, _ := k.pingCost(endpoint, model, prefix, u)
	k.mu.Lock()
	e.spent += cost
	e.refreshed = u.CacheRead
	if u.CacheRead > 0 {
		e.refreshedAt = startedAt
	}
	k.mu.Unlock()
	k.addSpent(cost)
	if ka, ok := j.emitter.(components.KeepAliveEmitter); ok {
		ka.KeepAlivePing(components.KeepAliveReport{
			Tenant: j.tenant, Session: j.session, Thread: j.thread, Model: model, Provider: string(provider),
			Route: route, Pings: j.ping, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite,
			Output: u.Output, CostUSD: cost, Status: status, DurationMs: ms,
			TS: k.now().UnixMilli(), Agent: agent, Preset: preset, StopReason: u.StopReason,
			// Which manager-controlled strategy resolved this PING's policy, "" when none did
			// — tagged on the ping's own row, never on the real request it later rescues, since
			// only the ping's policy resolution ever consults the strategy list.
			Strategy:   appliedStrategy,
			FreshInput: u.FreshInput, CacheWrite1h: u.CacheWrite1h,
			// What the ping actually asked for, so the row says it rather than reading as a
			// request with no output budget. The audit trail is the reason these rows exist; a
			// column that is blank because nobody filled it in is the same defect at a smaller
			// scale.
			MaxTokens: pingOutputBudget(route) + j.vary%pingVary,
		})
	}
	return cost
}

// pingCost prices one ping's usage. The same gate dash.Event.Price used to apply before pricing
// anything: no cost without a usable rate, and no cost claimed for a ping whose usage reported
// nothing at all. priced is false when the model has no rate.
func (k *Keeper) pingCost(endpoint, model string, prefix int64, u Usage) (cost float64, priced bool) {
	if model == "" {
		return 0, false
	}
	price, ok := k.price(endpoint, model, prefix)
	if !ok || price.Zero() {
		return 0, false
	}
	if u.CacheRead > 0 || u.CacheWrite > 0 || u.Output > 0 {
		// See modelinfo.Price.CostWithCacheWrite1h for the one-hour write premium this folds
		// in — the same helper dash.Event.Price uses, so the two cannot drift apart again.
		cost = price.CostWithCacheWrite1h(u.FreshInput, u.CacheRead, u.CacheWrite, u.Output, u.CacheWrite1h)
	}
	return cost, true
}

// addSpent adds one ping's cost to the process-wide counter.
func (k *Keeper) addSpent(cost float64) {
	for {
		old := k.spentUSD.Load()
		if k.spentUSD.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+cost)) {
			return
		}
	}
}

// Stats is the keeper's own ledger, for /stats. Every field is a way the mechanism
// can be worthless, which is the point of publishing them together: pings with no
// conversions is a policy that spends and gains nothing, and `wrote` above zero is a bug.
type Stats struct {
	Live    int   `json:"live_sessions"`
	Pings   int64 `json:"pings"`
	Skipped int64 `json:"skipped"`
	Failed  int64 `json:"failed"`
	Wrote   int64 `json:"wrote_instead_of_read"`
	// Missed counts pings that read nothing — a real cache miss, not a failure. A FIRST miss
	// in a row keeps pinging; a SECOND stops it (see noteOutcome), which is why Missed and
	// Wrote can both be 1 for the same session (TestASecondMissInARowStops).
	Missed   int64   `json:"missed"`
	SpentUSD float64 `json:"spend_usd"`
	// RouteTrips counts how many times the route breaker has tripped (main #527, see
	// breaker.go) — a route failing routeFailLimit times in a row. Climbing here, unlike
	// Failed, says the SAME route keeps failing rather than scattered one-off failures.
	RouteTrips int64 `json:"route_trips"`
	// SummarySubstituted, SummarySubstituteFailed and SummarySubstituteOverBudget are
	// cache_aware_summarizer's share of Pings — see fireSummarySubstitute. A deployment running
	// that component should see SummarySubstituted climb instead of a flat Pings/Wrote pair; a
	// SummarySubstituteFailed or SummarySubstituteOverBudget that climbs just as fast says the
	// substitution is being attempted and declining, not that it is dead.
	SummarySubstituted          int64 `json:"keepalive_summary_substituted"`
	SummarySubstituteFailed     int64 `json:"keepalive_summary_substitute_failed"`
	SummarySubstituteOverBudget int64 `json:"keepalive_summary_substitute_over_budget"`
	// SummarySubstitutePhaseMismatch counts a routine `pre_expiry` candidate declined because
	// this particular ping did not land inside the component's own pre-expiry window.
	SummarySubstitutePhaseMismatch int64 `json:"keepalive_summary_substitute_phase_mismatch"`
	// SummarySubstituteNoCandidate/CheckpointExists/InFlight/ConcurrencyFull split what used to
	// be one silent "no substitute" outcome — see fireSummarySubstitute. NoCandidate is the
	// ordinary case for most sessions; the other three, climbing on a session that SHOULD have a
	// registered candidate, are what makes a registration gap diagnosable instead of looking
	// identical to the component simply not being in that tenant's pipeline.
	SummarySubstituteNoCandidate      int64 `json:"keepalive_summary_substitute_no_candidate"`
	SummarySubstituteCheckpointExists int64 `json:"keepalive_summary_substitute_checkpoint_exists"`
	SummarySubstituteInFlight         int64 `json:"keepalive_summary_substitute_in_flight"`
	SummarySubstituteConcurrencyFull  int64 `json:"keepalive_summary_substitute_concurrency_full"`
	// SummarySubstituteRefreshPaid/RefreshNotWorthIt are the money gate's own ledger (#415) for a
	// `pre_expiry` reserve that already exists: Paid when a refresh's expected saving on the
	// eventual cold return covered its own projected cost, NotWorthIt when it did not. Climbing
	// NotWorthIt on a deployment with a lot of idle time between turns is expected and healthy —
	// it is the gate doing its job, not a failure.
	SummarySubstituteRefreshPaid       int64 `json:"keepalive_summary_substitute_refresh_paid"`
	SummarySubstituteRefreshNotWorthIt int64 `json:"keepalive_summary_substitute_refresh_not_worth_it"`
	// Sweep* are extract_llm_sweep's share of Pings (#404): the question it sends in place of a ping.
	// SweepSubstituted climbing while SweepAsked/SweepHeld lag means the question reaches the
	// provider and its answers do not parse. SweepModelDeclined is sweep_model_declined (#422): pings
	// sent plain because the question would break the cache on that model.
	SweepSubstituted   int64 `json:"keepalive_sweep_substituted"`
	SweepAskFailed     int64 `json:"keepalive_sweep_ask_failed"`
	SweepOverBudget    int64 `json:"keepalive_sweep_over_budget"`
	SweepAsked         int64 `json:"sweep_asked"`
	SweepHeld          int64 `json:"sweep_held"`
	SweepModelDeclined int64 `json:"sweep_model_declined"`
	// SweepSummaryOwnedPing is #404 point 10: pings where the summarizer had a candidate, so it owned the
	// ping and the sweep did not ask (also when the summary then declined). SweepSummaryInFlight: a
	// summary was already running. SweepThreadBusy: an earlier substitute still held the thread.
	SweepSummaryOwnedPing int64 `json:"sweep_summary_owned_ping"`
	SweepSummaryInFlight  int64 `json:"sweep_summary_in_flight"`
	SweepThreadBusy       int64 `json:"sweep_ping_thread_busy"`
	SweepNoInventory      int64 `json:"sweep_no_inventory"`
	SweepAlreadyAsked     int64 `json:"sweep_already_asked"`
	SweepNoCandidate      int64 `json:"sweep_no_candidate"`
	SweepHoldNoToolID     int64 `json:"sweep_hold_no_tool_id"`
	SweepPanics           int64 `json:"sweep_ping_panics"`
	SweepConcurrency      int64 `json:"sweep_concurrency_full"`
	SweepNotRegistered    int64 `json:"sweep_not_registered"`
	// SweepAskBatches counts the batches asked in all (#132): a sweep of 20 candidates is 2 batches.
	// SweepBatchFailed counts batches that failed open, SweepBatchFailedBy giving the reason. The answer
	// cost sums every batch.
	SweepAskBatches    int64            `json:"sweep_ask_batches"`
	SweepBatchFailed   int64            `json:"sweep_batch_failed"`
	SweepBatchFailedBy map[string]int64 `json:"sweep_batch_failed_by,omitempty"`
	// SweepBatchSkipped counts batches never started because the sweep's deadline passed. SweepBatchOverCap
	// counts candidates left unasked because fewer batches were allowed. SweepBatchesLowered counts sweeps
	// where the per-ping cost ceiling admitted fewer questions than planned.
	SweepBatchSkipped   int64 `json:"sweep_batch_skipped"`
	SweepBatchOverCap   int64 `json:"sweep_batch_over_cap"`
	SweepBatchesLowered int64 `json:"sweep_batches_lowered"`
	// SweepDrops is what became of the drops the sweep applied (#404 point 7), overall and for the
	// busiest threads, under hashed labels. The false-drop rate is the number to pull the sweep from
	// xhigh on. Metadata only: counts, never content, never a session id.
	SweepDrops *SweepDropStats `json:"sweep_drops,omitempty"`
	// SweepGates counts the adjudication's own gate and event names across ping-time questions
	// (sweep_kept, sweep_unparseable, sweep_dropped, ...), the counters a request's Report carries
	// for an in-request question.
	SweepGates map[string]int64 `json:"sweep_gates,omitempty"`
}

// SweepDropStats is offload.SweepDropStats in /stats shape.
type SweepDropStats struct {
	Applied              int64   `json:"applied"`
	AppliedCold          int64   `json:"applied_cold"`
	AppliedChangedPrefix int64   `json:"applied_changed_prefix"`
	ApplyDeclined        int64   `json:"apply_declined"`
	Referenced           int64   `json:"referenced_later"`
	Expanded             int64   `json:"expanded"`
	FalseDrops           int64   `json:"false_drops"`
	FalseDropRate        float64 `json:"false_drop_rate"`
	ExpandEvents         int64   `json:"expand_events"`
	ExpandTokens         int64   `json:"expand_tokens"`
	// ApplyPanics counts recovered cold-apply panics; HeldStaleWrites older answers refused.
	ApplyPanics int64 `json:"apply_panics"`
	// Threads are the busiest threads by drops applied, most first.
	Threads []SweepDropThread `json:"threads,omitempty"`
}

// SweepDropThread is one thread's share, under a hash label that is not the session id.
type SweepDropThread struct {
	Thread        string  `json:"thread"`
	Applied       int64   `json:"applied"`
	FalseDrops    int64   `json:"false_drops"`
	FalseDropRate float64 `json:"false_drop_rate"`
}

// sweepDropStats reads the offload ledger, or nil when the sweep never applied anything, so a
// deployment without the sweep shows no field.
func sweepDropStats() *SweepDropStats {
	d := offload.SweepDropCounters()
	o := d.Overall
	if o.Applied == 0 && o.ApplyDeclined == 0 && o.ExpandEvents == 0 {
		return nil
	}
	out := &SweepDropStats{Applied: o.Applied, AppliedCold: o.AppliedCold,
		AppliedChangedPrefix: o.AppliedChangedPrefix, ApplyDeclined: o.ApplyDeclined,
		Referenced: o.Referenced, Expanded: o.Expanded, FalseDrops: o.FalseDrops(),
		FalseDropRate: o.FalseDropRate(), ExpandEvents: o.ExpandEvents, ExpandTokens: o.ExpandTokens,
		ApplyPanics: offload.SweepApplyPanics()}
	for _, t := range d.Threads {
		out.Threads = append(out.Threads, SweepDropThread{Thread: t.Label, Applied: t.Applied,
			FalseDrops: t.FalseDrops(), FalseDropRate: t.FalseDropRate()})
	}
	return out
}

// PendingPings counts entries with a ping still ahead of them. Nil-safe: a keeper whose
// sweep never launched (the CONTEXT_GURU_KEEPALIVE kill switch) has nothing pending, which
// correctly lets an idle proxy exit.
func (k *Keeper) PendingPings() int {
	if k == nil {
		return 0
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	n := 0
	for _, e := range k.live {
		if !e.stopped && e.pol.On() && e.pings < e.pol.MaxPings {
			n++
		}
	}
	return n
}

// Stats snapshots the keeper's counters.
func (k *Keeper) Stats() Stats {
	if k == nil {
		return Stats{}
	}
	k.mu.Lock()
	live := len(k.live)
	k.mu.Unlock()
	sw := offload.SweepPingCounters()
	bc := offload.SweepBatchCounters() // ONE snapshot, so the three batch fields agree
	return Stats{Live: live, Pings: k.pings.Load(), Skipped: k.skipped.Load(),
		Failed: k.failed.Load(), Wrote: k.wrote.Load(), Missed: k.missed.Load(),
		RouteTrips:                         k.routeTrips.Load(),
		SpentUSD:                           math.Float64frombits(k.spentUSD.Load()),
		SummarySubstituted:                 k.summarySubstituted.Load(),
		SummarySubstituteFailed:            k.summarySubstituteFailed.Load(),
		SummarySubstituteOverBudget:        k.summarySubstituteOverBudget.Load(),
		SummarySubstitutePhaseMismatch:     k.summarySubstitutePhaseMismatch.Load(),
		SummarySubstituteNoCandidate:       k.summarySubstituteNoCandidate.Load(),
		SummarySubstituteCheckpointExists:  k.summarySubstituteCheckpointExists.Load(),
		SummarySubstituteInFlight:          k.summarySubstituteInFlight.Load(),
		SummarySubstituteConcurrencyFull:   k.summarySubstituteConcurrencyFull.Load(),
		SummarySubstituteRefreshPaid:       k.summarySubstituteRefreshPaid.Load(),
		SummarySubstituteRefreshNotWorthIt: k.summarySubstituteRefreshNotWorthIt.Load(),
		SweepSubstituted:                   k.sweepSubstituted.Load(),
		SweepAskFailed:                     k.sweepAskFailed.Load(),
		SweepOverBudget:                    k.sweepOverBudget.Load(),
		SweepAsked:                         sw.Asked,
		SweepHeld:                          sw.Held,
		SweepModelDeclined:                 sw.ModelDeclined,
		SweepSummaryOwnedPing:              sw.SummaryOwnedPing,
		SweepSummaryInFlight:               sw.SummaryInFlight,
		SweepThreadBusy:                    sw.ThreadBusy,
		SweepNoInventory:                   sw.NoInventory,
		SweepAlreadyAsked:                  sw.AlreadyAsked,
		SweepNoCandidate:                   sw.NoCandidate,
		SweepHoldNoToolID:                  sw.HoldNoToolID,
		SweepPanics:                        sw.Panics,
		SweepConcurrency:                   sw.ConcurrencyFull,
		SweepNotRegistered:                 sw.NotRegistered,
		SweepAskBatches:                    bc.Batches,
		SweepBatchFailed:                   bc.Failed,
		SweepBatchFailedBy:                 bc.FailedBy,
		SweepBatchSkipped:                  bc.Skipped,
		SweepBatchOverCap:                  bc.OverCap,
		SweepBatchesLowered:                k.sweepBatchesLowered.Load(),
		SweepDrops:                         sweepDropStats(),
		SweepGates:                         sw.Gates,
	}
}

// LiveSessionKeys returns the session ids the keeper currently considers live — a copy,
// so mutating the result never touches k.live. k.live is keyed by tenant:session:thread, but
// each entry carries its own raw session id (kaEntry.session), which is what
// dash.Filter.Session expects. Each session appears once, however many of its threads are live.
func (k *Keeper) LiveSessionKeys() []string {
	if k == nil {
		return nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	keys := make([]string, 0, len(k.live))
	seen := make(map[string]bool, len(k.live))
	for _, e := range k.live {
		if !seen[e.session] {
			seen[e.session] = true
			keys = append(keys, e.session)
		}
	}
	return keys
}

// Plan is one thread's keep-alive plan, for a status line. Times are absolute; the host converts.
type Plan struct {
	RefreshedAt time.Time // the last moment the provider's entry is known refreshed
	NextPing    time.Time // startedAt + Idle
	PingsLeft   int
	Idle        time.Duration
}

// Plans returns the plan of every thread of ONE session still to be pinged, keyed by thread
// ("" = main). session must always match — this never lists "every session" — and all only
// drops the TENANT filter, matching main's allTenants: a single-tenant deployment calls this
// with all=true and its one real session id, never with session="". Only an entry that will
// still be pinged has a plan: the same conditions as PendingPings, plus pingable. Port of main's
// Handler.KeepAlivePlans (git show 67c5f758:proxy/keepalive.go L2102: `e.session != session ||
// (!allTenants && e.tenant != tenantID)`, keyed by e.thread unconditionally), for the status
// line's "held warm until".
//
// Keyed by thread even under all=true, same as main: if two different tenants' entries for this
// session land on the same thread (e.g. both "", the main thread — ordinarily impossible, since a
// session id is scoped to its tenant, but all=true deliberately steps outside that scope), one
// overwrites the other in the result map. Map iteration order is unspecified, so which tenant's
// entry survives is nondeterministic — exactly main's own behavior, which keys the identical way
// with no tie-break either.
func (k *Keeper) Plans(tenant string, all bool, session string) map[string]Plan {
	if k == nil {
		return nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	out := map[string]Plan{}
	for _, e := range k.live {
		if e.session != session || (!all && e.tenant != tenant) ||
			e.stopped || !e.pol.On() || e.pings >= e.pol.MaxPings || !e.pingable() {
			continue
		}
		out[e.thread] = Plan{RefreshedAt: e.refreshedAt, NextPing: e.startedAt.Add(e.pol.Idle),
			PingsLeft: e.pol.MaxPings - e.pings, Idle: e.pol.Idle}
	}
	return out
}

// Entry snapshots one tracked entry. Exported for tests and replay tooling; hosts do not need it.
// MaskedBody is a copy and stays masked, so a caller can measure it without being handed a
// readable transcript.
func (k *Keeper) Entry(key Key) (EntryView, bool) {
	if k == nil {
		return EntryView{}, false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	e, ok := k.live[key.id()]
	if !ok {
		return EntryView{}, false
	}
	return EntryView{Pings: e.pings, Turn: e.turn, Prefix: e.prefix, PingUSD: e.pingUSD,
		Spent: e.spent, Refreshed: e.refreshed, Stopped: e.stopped, TTL1h: e.ttl1h, Policy: e.pol, Tag: e.tag,
		MaskedBody: append([]byte(nil), e.body...)}, true
}

// tenantLabel is the tenant id as a log label. Single-tenant deployments have no
// tenant id at all, and an empty label is DROPPED by promtail — which would leave the
// logs dashboard's tenant selector empty for every local proxy, i.e. for most users.
// "local" says the true thing (there is one tenant, it is this deployment) and makes
// the same dashboard work in both modes.
func tenantLabel(id string) string {
	if id == "" {
		return "local"
	}
	return id
}

// predictorFor resolves a predictor id to a probability function over the entry's own
// stop_reason, and reports whether the id is known. This is the whole production-safe
// predictor class: a rule, or (a future entry) a portable logistic-regression dot product
// — never an embedded model or a call out of the hot path. See kaEntry.stopReason and
// Policy.PredictorID/PredictorThreshold for where the result is used.
//
// The set of ids a host may accept for a strategy (cmd/context-guru-proxy's knownPredictorIDs)
// is kept in lockstep with this switch by the proxy's TestKnownPredictorIDsMatchPredictorFor,
// which checks it through Keeper.Record rather than by calling this directly.
func predictorFor(id string) (func(stopReason string) float64, bool) {
	switch id {
	case "stop-reason-gated":
		return func(stopReason string) float64 {
			if kvcache.ClusterOf(stopReason) == kvcache.ClusterActuallyDone {
				return 1.0
			}
			return 0.0
		}, true
	}
	return nil, false
}

// PredictorFor is predictorFor, exported for a host that needs to replay the same gate
// outside the keeper (the adapt-cache report's currentPlan).
func PredictorFor(id string) (func(stopReason string) float64, bool) { return predictorFor(id) }

// fireSubstitute runs the one substitute the claim names for this ping and reports whether it took the
// ping. The claim is released by a defer, so every exit frees the thread, a panic included. The sweep's
// collection slot is released the same way inside SweepPingSubstitute, so a claimed ping holds neither
// past this call.
func (k *Keeper) fireSubstitute(j pingJob) bool {
	claim := offload.ClaimPing(thread.Key(j.session, j.thread))
	defer claim.Release()
	switch claim.Choice {
	case offload.PingSummary:
		return k.fireSummarySubstitute(j)
	case offload.PingSweep:
		return k.fireSweepSubstitute(j)
	case offload.PingNone:
		// Nothing registered. Both lookups decline and count their own no-candidate reason.
		return k.fireSummarySubstitute(j) || k.fireSweepSubstitute(j)
	}
	return false
}
