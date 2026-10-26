package offload

import (
	"sync"
	"sync/atomic"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/thread"
)

// Cold-turn ordering for cache_aware_summarizer's `any` cache state.
//
// A turn whose request-level gates (fill, cache_state) all permit firing can still arrive with a
// COLD cache: `cache_state: any` fires regardless of phase, and CachePhaseCold is one of the
// phases it is allowed to fire on. Firing the side call immediately in that state races the
// forwarded request to rewrite the SAME prefix — both calls miss, both pay the 1.25x
// cache-creation rate, and the side call's whole argument (reuse a prefix the backend already
// has) is false for it. Deferring the side call until the forwarded request's own usage confirms
// the rewrite happened turns the miss into a guaranteed hit: the forwarded request pays the
// rewrite once (it was always going to), and the side call then reads what it just wrote.
//
// This registry is the handoff. Offload (running on the request's own goroutine, before the
// request goes upstream) stores the commission material here instead of calling
// startAsyncSummary directly; proxy calls ResolveDeferredCacheAwareSummary once that same
// request's usage is observed (succeeded or not — see its own doc for why "not" still resolves).
//
// THE SAME HANDOFF SERVES A WARM PREFIX-ASK COMMISSION (#722). That call appends its question to
// the host's stash of this thread's last forwarded body, and checks that the stash covers the span
// it is about to summarize. Started at Offload time, it ran about 1 ms later, while the stash still
// held the PREVIOUS turn's body, so the coverage check refused it every time (stale_prefix, 3 of 3
// on a clean session) and no detached summary ever committed. The host stashes this turn's body on
// its upstream 200, before the hook below runs, so a commission dispatched from the hook reads the
// body it was built against.
type coldDeferral struct {
	s            *CacheAwareSummarizer
	ctx          *components.Ctx // minimal: Session, Thread, Store, Ctx — see deferSummary
	call         summaryCaller
	path         string
	trigger      string
	reserved     bool
	span         []bschemas.ChatMessage
	coveredCount int
	timer        *time.Timer
}

var (
	coldDeferredMu sync.Mutex
	coldDeferred   = map[string]*coldDeferral{}
)

// maxColdDeferred bounds the registry the same way maxConcurrentSummaries bounds the async path:
// a few hundred sessions simultaneously arriving cold and never sending a follow-up request is
// not a shape this proxy needs to hold open-ended memory for.
const maxColdDeferred = 256

// coldDeferralFallback is the longest a deferred commission waits for
// ResolveDeferredCacheAwareSummary before firing anyway. Primary resolution is the proxy's hook
// right after the triggering turn's own usage is observed — normally milliseconds away — so this
// is a safety net for a host that never calls the hook (a library consumer of `apply` directly,
// or a request whose response capture failed) rather than the expected path. Firing anyway after
// the fallback is the fail-open answer: the alternative is losing the compaction forever on a
// session nothing will ever resolve.
//
// A var, not a const, so a test can shrink it rather than waiting out the real 90s — the same
// seam defaultSummaryWait's own callers use elsewhere in this package.
var coldDeferralFallback = 90 * time.Second

// cacheAwareColdDeferredStarted/Resolved/FallbackFired/Dropped are the pair (and its two
// explanations) the async path already has: started vs resolved says whether a deferral is being
// lost, and FallbackFired/Dropped say which of the two non-hook exits happened.
var (
	cacheAwareColdDeferredStarted       int64
	cacheAwareColdDeferredResolved      int64
	cacheAwareColdDeferredFallbackFired int64
	cacheAwareColdDeferredDropped       int64
)

// CacheAwareColdDeferralStats reports the cold-deferral registry's counters for /stats.
func CacheAwareColdDeferralStats() (started, resolved, fallbackFired, dropped int64) {
	return atomic.LoadInt64(&cacheAwareColdDeferredStarted),
		atomic.LoadInt64(&cacheAwareColdDeferredResolved),
		atomic.LoadInt64(&cacheAwareColdDeferredFallbackFired),
		atomic.LoadInt64(&cacheAwareColdDeferredDropped)
}

// deferSummary stores one commission until the turn's own forwarded request finishes, replacing any
// earlier one for the session
// (stale either way: a session cannot have two triggering turns want two different spans
// deferred at once without a real request landing in between, and a real request clears the
// deferral along with every checkpoint-affecting state this component owns).
//
// Everything stored is read-only, request-scoped data copied out of the live request — the same
// discipline startAsyncSummary already follows and for the same reason: this goroutine's caller
// (the fallback timer, or ResolveDeferredCacheAwareSummary) runs long after the request that
// built it has returned.
//
// trigger and reserved are handed to startAsyncSummary unchanged when the deferral resolves: a cold
// commission passes "cold_deferred" and false, a warm prefix-ask commission its own "turn" and the
// reserve flag it computed (#722). warm selects the registry: a warm prefix-ask commission is held
// apart from the cold ones, under its own larger bound (see warmDeferred).
func (s *CacheAwareSummarizer) deferSummary(c *components.Ctx, call summaryCaller, path, trigger string,
	reserved, warm bool, span []bschemas.ChatMessage, coveredCount int) {
	if c == nil || c.Session == "" {
		return
	}
	spanCopy := append([]bschemas.ChatMessage(nil), span...)
	// A minimal Ctx: startAsyncSummary reads only Session, Thread, Store and Ctx off it (see its own
	// comment on why those three are read on the request's goroutine rather than from inside the
	// goroutine it starts). c.Ctx is a value-holder past the request's lifetime — WithoutCancel
	// detaches it from cancellation anyway, inside startAsyncSummary — so holding onto it here is
	// safe even though the request itself has long since returned by the time this resolves.
	lite := &components.Ctx{Session: c.Session, Thread: c.Thread, Store: c.Store, Ctx: c.Ctx}
	conv := convKey(c)
	d := &coldDeferral{s: s, ctx: lite, call: call, path: path, trigger: trigger, reserved: reserved,
		span: spanCopy, coveredCount: coveredCount}
	if warm {
		s.deferWarm(conv, d)
		return
	}

	coldDeferredMu.Lock()
	if len(coldDeferred) >= maxColdDeferred && coldDeferred[conv] == nil {
		// The registry is full and this is a NEW session, not a replacement — fail open by firing
		// now rather than refusing to remember the commission at all. This reintroduces the
		// double-rewrite risk for this one turn, which is the lesser cost against losing the
		// compaction, or the memory bound, entirely.
		coldDeferredMu.Unlock()
		atomic.AddInt64(&cacheAwareColdDeferredDropped, 1)
		s.startAsyncSummary(lite, call, path, trigger, reserved, spanCopy, coveredCount)
		return
	}
	if prev, ok := coldDeferred[conv]; ok && prev.timer != nil {
		prev.timer.Stop()
	}
	d.timer = time.AfterFunc(coldDeferralFallback, func() { s.resolveFallback(conv) })
	coldDeferred[conv] = d
	coldDeferredMu.Unlock()
	atomic.AddInt64(&cacheAwareColdDeferredStarted, 1)
}

// WARM PREFIX-ASK DEFERRALS (#722), held apart from the cold ones. Every qualifying warm turn on a
// prefix-ask thread defers, from Offload until its own request ends, so these far outnumber cold
// arrivals; sharing the cold registry's 256 slots would, under load, force the early dispatch #722
// removes (#724 review). Their own bound and counters make that cap visible if it is ever reached.
var (
	warmDeferredMu sync.Mutex
	warmDeferred   = map[string]*coldDeferral{}
)

// maxWarmDeferred bounds warm deferrals. One entry per thread with a commissioning request in
// flight, each holding a span copy for one request's lifetime.
const maxWarmDeferred = 4096

// Warm-deferral counters: started, resolved by the host hook, dispatched because a newer request on
// the same thread replaced them, fired by the fallback, dispatched early because the registry was
// full, and dispatches startAsyncSummary refused (in flight, or the concurrency bound).
var (
	cacheAwareWarmDeferredStarted    int64
	cacheAwareWarmDeferredResolved   int64
	cacheAwareWarmDeferredReplaced   int64
	cacheAwareWarmDeferredFallback   int64
	cacheAwareWarmDeferredFull       int64
	cacheAwareWarmDeferredRefused    int64
	cacheAwareWarmDeferredSkippedDup int64
)

// CacheAwareWarmDeferralStats reports the warm-deferral counters, in the order of the vars above.
func CacheAwareWarmDeferralStats() (started, resolved, replaced, fallback, full, refused, skippedDup int64) {
	return atomic.LoadInt64(&cacheAwareWarmDeferredStarted), atomic.LoadInt64(&cacheAwareWarmDeferredResolved),
		atomic.LoadInt64(&cacheAwareWarmDeferredReplaced), atomic.LoadInt64(&cacheAwareWarmDeferredFallback),
		atomic.LoadInt64(&cacheAwareWarmDeferredFull), atomic.LoadInt64(&cacheAwareWarmDeferredRefused),
		atomic.LoadInt64(&cacheAwareWarmDeferredSkippedDup)
}

// deferWarm stores a warm deferral. A deferral it REPLACES is dispatched at once, never dropped: a
// newer request on the same thread has reached Offload, so the older turn's request already ended
// and its body is in the stash, which is all the older commission was waiting for. Without that, a
// host that never calls ResolveDeferredCacheAwareSummary would see every deferral replaced before
// its fallback fired, on any thread whose turns come less than the fallback apart (#724 review).
func (s *CacheAwareSummarizer) deferWarm(conv string, d *coldDeferral) {
	warmDeferredMu.Lock()
	prev := warmDeferred[conv]
	if prev == nil && len(warmDeferred) >= maxWarmDeferred {
		warmDeferredMu.Unlock()
		atomic.AddInt64(&cacheAwareWarmDeferredFull, 1)
		dispatchDeferred(d, &cacheAwareWarmDeferredRefused)
		return
	}
	if prev != nil {
		markDispatching(conv) // the replaced one leaves the registry now and is started below
		if prev.timer != nil {
			prev.timer.Stop()
		}
	}
	d.timer = time.AfterFunc(coldDeferralFallback, func() { s.resolveWarmFallback(conv) })
	warmDeferred[conv] = d
	warmDeferredMu.Unlock()
	atomic.AddInt64(&cacheAwareWarmDeferredStarted, 1)
	if prev != nil {
		atomic.AddInt64(&cacheAwareWarmDeferredReplaced, 1)
		dispatchDeferred(prev, &cacheAwareWarmDeferredRefused)
		doneDispatching(conv)
	}
}

// dispatching counts, per thread key, the deferrals that have left a registry but whose
// startAsyncSummary has not returned yet. A deferral is marked here BEFORE it leaves its registry
// (under that registry's lock), so there is no moment when it is neither registered nor in flight;
// WaitForSummaryForTest reads it, and a test that waits right after a request therefore cannot see
// "nothing pending" while a dispatch is about to start.
// betweenTakeAndStartForTest, set only by tests, runs after the hook has taken a warm deferral and
// before it is started: the window a test waiting on the thread must still see as pending.
var betweenTakeAndStartForTest func()

var (
	dispatchingMu sync.Mutex
	dispatching   = map[string]int{}
)

func markDispatching(conv string) { dispatchingMu.Lock(); dispatching[conv]++; dispatchingMu.Unlock() }

func doneDispatching(conv string) {
	dispatchingMu.Lock()
	if dispatching[conv]--; dispatching[conv] <= 0 {
		delete(dispatching, conv)
	}
	dispatchingMu.Unlock()
}

// takeWarmDeferral removes and returns the thread's pending warm deferral, already marked as
// dispatching; the caller must call doneDispatching(conv) once it has been started.
func takeWarmDeferral(conv string) *coldDeferral {
	warmDeferredMu.Lock()
	defer warmDeferredMu.Unlock()
	d, ok := warmDeferred[conv]
	if !ok {
		return nil
	}
	markDispatching(conv)
	delete(warmDeferred, conv)
	if d.timer != nil {
		d.timer.Stop()
	}
	return d
}

func (s *CacheAwareSummarizer) resolveWarmFallback(conv string) {
	if d := takeWarmDeferral(conv); d != nil {
		atomic.AddInt64(&cacheAwareWarmDeferredFallback, 1)
		dispatchDeferred(d, &cacheAwareWarmDeferredRefused)
		doneDispatching(conv)
	}
}

// dispatchDeferred starts a held commission. It skips one whose span a checkpoint already covers:
// between Offload and the hook, the idle keep-alive can commit the same span through its own
// substitute, and starting this one would pay for that summary twice (#724 review). A refusal by
// startAsyncSummary (in flight, or the concurrency bound) is counted into refused, since no turn's
// Report is left to file it on.
func dispatchDeferred(d *coldDeferral, refused *int64) {
	if cp, ok := loadCheckpoint(d.ctx); ok && cp.CoveredCount >= d.coveredCount &&
		cp.CoveredHash == spanHash(d.span) {
		atomic.AddInt64(&cacheAwareWarmDeferredSkippedDup, 1)
		return
	}
	if gate := d.s.startAsyncSummary(d.ctx, d.call, d.path, d.trigger, d.reserved, d.span, d.coveredCount); gate != "" {
		atomic.AddInt64(refused, 1)
	}
}

// takeColdDeferral removes and returns the thread's pending deferral, if any (conv is convKey). Shared by the
// proxy hook and the fallback timer so exactly one of them ever dispatches a given deferral.
func takeColdDeferral(conv string) *coldDeferral {
	coldDeferredMu.Lock()
	defer coldDeferredMu.Unlock()
	d, ok := coldDeferred[conv]
	if !ok {
		return nil
	}
	markDispatching(conv)
	delete(coldDeferred, conv)
	if d.timer != nil {
		d.timer.Stop()
	}
	return d
}

// resolveFallback is the deferral's own safety-net exit — see coldDeferralFallback.
func (s *CacheAwareSummarizer) resolveFallback(conv string) {
	d := takeColdDeferral(conv)
	if d == nil {
		return // already resolved via the proxy hook
	}
	atomic.AddInt64(&cacheAwareColdDeferredFallbackFired, 1)
	d.s.startAsyncSummary(d.ctx, d.call, d.path, d.trigger, d.reserved, d.span, d.coveredCount)
	doneDispatching(conv)
}

// ResolveDeferredCacheAwareSummary dispatches a summary held for the triggering turn's own forwarded
// request: a cold commission, so the side call reads a now-warm prefix instead of racing the
// forwarded request to rewrite it twice, and a warm prefix-ask commission, so its call reads this
// turn's body from the host's stash rather than the previous turn's (#722).
//
// A HOST THAT SETS Ctx.PrefixAsk MUST CALL THIS when each request ends, after it has stashed that
// request's forwarded body. Without it a warm prefix-ask commission is dispatched only when a newer
// request on the thread replaces it, or by the fallback timer.
//
// Called by the proxy right after that turn's own upstream usage is observed — success or
// failure. FAILURE STILL RESOLVES, deliberately: a forwarded request that errored or timed out
// may or may not have reached the provider far enough to write the cache, but there is no
// decidable "did it rewrite" signal narrower than "the round trip finished", and waiting for one
// that never fires would silently lose the compaction via the fallback timer only — a worse
// outcome than occasionally dispatching the side call against a prefix that, in fact, never got
// rewritten (which is simply the ordinary cold-and-will-miss case this deferral did not create).
//
// A no-op when no deferral is pending for the thread.
//
// threadID is the request's thread (Ctx.Thread): the deferral is keyed by thread (convKey, #430),
// so a subagent's forwarded request resolves only the subagent's own deferral, never the main
// thread's, whose cache that request did not rewrite.
func ResolveDeferredCacheAwareSummary(session, threadID string) {
	key := thread.Key(session, threadID)
	if d := takeColdDeferral(key); d != nil {
		atomic.AddInt64(&cacheAwareColdDeferredResolved, 1)
		d.s.startAsyncSummary(d.ctx, d.call, d.path, d.trigger, d.reserved, d.span, d.coveredCount)
		doneDispatching(key)
	}
	if d := takeWarmDeferral(key); d != nil {
		atomic.AddInt64(&cacheAwareWarmDeferredResolved, 1)
		if betweenTakeAndStartForTest != nil {
			betweenTakeAndStartForTest()
		}
		dispatchDeferred(d, &cacheAwareWarmDeferredRefused)
		doneDispatching(key)
	}
}
