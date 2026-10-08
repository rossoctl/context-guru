package offload

import (
	"sync"
	"sync/atomic"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
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
type coldDeferral struct {
	s            *CacheAwareSummarizer
	ctx          *components.Ctx // minimal: Session, Store, Ctx — see deferColdSummary
	call         summaryCaller
	path         string
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

// deferColdSummary stores one cold-turn commission, replacing any earlier one for the session
// (stale either way: a session cannot have two triggering turns want two different spans
// deferred at once without a real request landing in between, and a real request clears the
// deferral along with every checkpoint-affecting state this component owns).
//
// Everything stored is read-only, request-scoped data copied out of the live request — the same
// discipline startAsyncSummary already follows and for the same reason: this goroutine's caller
// (the fallback timer, or ResolveDeferredCacheAwareSummary) runs long after the request that
// built it has returned.
func (s *CacheAwareSummarizer) deferColdSummary(c *components.Ctx, call summaryCaller, path string,
	span []bschemas.ChatMessage, coveredCount int) {
	if c == nil || c.Session == "" {
		return
	}
	spanCopy := append([]bschemas.ChatMessage(nil), span...)
	// A minimal Ctx: startAsyncSummary reads only Session, Store and Ctx off it (see its own
	// comment on why those three are read on the request's goroutine rather than from inside the
	// goroutine it starts). c.Ctx is a value-holder past the request's lifetime — WithoutCancel
	// detaches it from cancellation anyway, inside startAsyncSummary — so holding onto it here is
	// safe even though the request itself has long since returned by the time this resolves.
	lite := &components.Ctx{Session: c.Session, Store: c.Store, Ctx: c.Ctx}
	d := &coldDeferral{s: s, ctx: lite, call: call, path: path, span: spanCopy, coveredCount: coveredCount}

	coldDeferredMu.Lock()
	if len(coldDeferred) >= maxColdDeferred && coldDeferred[c.Session] == nil {
		// The registry is full and this is a NEW session, not a replacement — fail open by firing
		// now rather than refusing to remember the commission at all. This reintroduces the
		// double-rewrite risk for this one turn, which is the lesser cost against losing the
		// compaction, or the memory bound, entirely.
		coldDeferredMu.Unlock()
		atomic.AddInt64(&cacheAwareColdDeferredDropped, 1)
		s.startAsyncSummary(lite, call, path, "cold_deferred", false, spanCopy, coveredCount)
		return
	}
	if prev, ok := coldDeferred[c.Session]; ok && prev.timer != nil {
		prev.timer.Stop()
	}
	d.timer = time.AfterFunc(coldDeferralFallback, func() { s.resolveFallback(c.Session) })
	coldDeferred[c.Session] = d
	coldDeferredMu.Unlock()
	atomic.AddInt64(&cacheAwareColdDeferredStarted, 1)
}

// takeColdDeferral removes and returns the session's pending deferral, if any. Shared by the
// proxy hook and the fallback timer so exactly one of them ever dispatches a given deferral.
func takeColdDeferral(session string) *coldDeferral {
	coldDeferredMu.Lock()
	defer coldDeferredMu.Unlock()
	d, ok := coldDeferred[session]
	if !ok {
		return nil
	}
	delete(coldDeferred, session)
	if d.timer != nil {
		d.timer.Stop()
	}
	return d
}

// resolveFallback is the deferral's own safety-net exit — see coldDeferralFallback.
func (s *CacheAwareSummarizer) resolveFallback(session string) {
	d := takeColdDeferral(session)
	if d == nil {
		return // already resolved via the proxy hook
	}
	atomic.AddInt64(&cacheAwareColdDeferredFallbackFired, 1)
	d.s.startAsyncSummary(d.ctx, d.call, d.path, "cold_deferred", false, d.span, d.coveredCount)
}

// ResolveDeferredCacheAwareSummary dispatches a cold-commissioned summary once the triggering
// turn's own forwarded request has itself rewritten the prompt cache, so the side call reads a
// now-warm prefix instead of racing the forwarded request to rewrite it twice.
//
// Called by the proxy right after that turn's own upstream usage is observed — success or
// failure. FAILURE STILL RESOLVES, deliberately: a forwarded request that errored or timed out
// may or may not have reached the provider far enough to write the cache, but there is no
// decidable "did it rewrite" signal narrower than "the round trip finished", and waiting for one
// that never fires would silently lose the compaction via the fallback timer only — a worse
// outcome than occasionally dispatching the side call against a prefix that, in fact, never got
// rewritten (which is simply the ordinary cold-and-will-miss case this deferral did not create).
//
// A no-op when no deferral is pending for the session — the common case, since most sessions
// never arrive cold in the first place.
func ResolveDeferredCacheAwareSummary(session string) {
	d := takeColdDeferral(session)
	if d == nil {
		return
	}
	atomic.AddInt64(&cacheAwareColdDeferredResolved, 1)
	d.s.startAsyncSummary(d.ctx, d.call, d.path, "cold_deferred", false, d.span, d.coveredCount)
}
