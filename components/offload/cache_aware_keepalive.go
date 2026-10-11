package offload

import (
	"context"
	"sync"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/thread"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// Letting cache_aware_summarizer stand in for the idle keep-alive's bare cache-read ping.
//
// proxy/keepalive.go's keeper spends the caller's own credential on a ping whose only job is to
// read the cached prefix and reset the provider's TTL. When this component is in the pipeline,
// has already decided a session's fill is worth a summary, and is only waiting on the right
// cache phase (or lost the race for a concurrency slot, or any other transient reason it did not
// fire on the turn that registered this material), the keeper's next scheduled ping is a session
// that is ABOUT TO be pinged anyway — and the summarizer's own call reads the exact same cached
// prefix a ping would, so it refreshes the same TTL while also producing a compaction instead of
// one output token.
//
// The registration happens in Offload, on the request's own goroutine, every turn that reaches
// the point of having real commission material (a resolved summaryCaller plus a span) — regardless of
// whether the cache-state gate let it actually fire THIS turn. See cache_aware_summarizer.go's
// own comment at the call site for why registering unconditionally is safe.
type keepAliveCandidate struct {
	s            *CacheAwareSummarizer
	ctx          *components.Ctx // minimal: Session, Store, Ctx
	call         summaryCaller
	path         string
	span         []bschemas.ChatMessage
	coveredCount int
	// cacheState/preExpirySeconds are the component's OWN trigger.cache_state and
	// trigger.pre_expiry_seconds, carried so the keeper can tell whether ITS idea of "due for a
	// ping right now" also satisfies what THIS candidate was configured to wait for — see
	// KeepAliveCandidateInfo.
	cacheState       string
	preExpirySeconds int
	// registeredAt is when THIS candidate was written, so ClearStaleKeepAliveCandidate can tell
	// "a candidate left over from an EARLIER request" from "the one THIS request's own pipeline
	// run just registered" without needing to know the session's resolved key before running the
	// pipeline — see that function's own comment for why that distinction has to be timestamp-based
	// rather than ordering-based.
	registeredAt time.Time
}

// KeepAliveCandidateInfo is what a registered candidate asked for, surfaced so the keeper can
// decide whether ITS OWN notion of "due" also satisfies the candidate's cache_state — a
// `pre_expiry` candidate must not be substituted on a ping the keeper is sending for some other
// reason (e.g. a strategy-driven K>1 schedule, or simply because Idle has elapsed on a session
// whose cache is still warm by the component's own, possibly narrower, pre_expiry_seconds).
type KeepAliveCandidateInfo struct {
	CacheState       string
	PreExpirySeconds int
	// IsRefresh is true when a Dispatch, if run, would REFRESH an existing `pre_expiry` reserve —
	// replace it with a new one covering a larger span — rather than create the session's first
	// one. The keeper's own money gate (issue #415: refresh only when the expected saving on the
	// eventual cold return covers the call's own projected cost) applies ONLY to a refresh:
	// creating the first reserve is compared against having none at all (the full-rewrite cold
	// return this whole mechanism exists to avoid), not against a slightly smaller reserve, so it
	// has no such test.
	IsRefresh bool
	// RefreshTailTokens is the token size of the material a refresh would fold in beyond what the
	// existing reserve already covers — the number the money gate weighs against its own
	// projected cost. Meaningful only when IsRefresh is true.
	RefreshTailTokens int
}

// keepAliveCand is keyed by thread.Key(session, thread) (#423). Every exported function below that
// takes a "session" takes that key: the session itself for the primary thread.
var (
	keepAliveCandMu sync.Mutex
	keepAliveCand   = map[string]*keepAliveCandidate{}
)

// KeepAliveSubstituteReason explains why KeepAliveSubstitute declined (ok=false), or why a
// Dispatch it returned did not commit — so a caller's counters and logs can tell "nothing was
// ever registered for this session" from "something WAS registered and was lost", which used to
// be one silent boolean. See fireSummarySubstitute's own comment on why that distinction matters:
// a session that should have had a candidate and does not is a real bug, while one that never
// had cache_aware_summarizer in its pipeline at all is the ordinary, overwhelming majority case.
type KeepAliveSubstituteReason string

const (
	// KeepAliveReasonNone is the zero value: either ok=true (a Dispatch was returned), or
	// Dispatch's own result committed — there is nothing to explain.
	KeepAliveReasonNone KeepAliveSubstituteReason = ""
	// KeepAliveReasonNoCandidate: no turn has registered material for this session at all — the
	// component is not in this tenant's pipeline, no turn has reached the registration point
	// yet, or ClearKeepAliveCandidate ran and nothing has re-registered since. The ordinary case
	// for the vast majority of sessions.
	KeepAliveReasonNoCandidate KeepAliveSubstituteReason = "no_candidate"
	// KeepAliveReasonCheckpointExists: a candidate WAS registered, but a checkpoint already
	// exists for this session — the next real turn will splice it for free, so spending a call
	// here would summarize a span nobody is waiting on.
	KeepAliveReasonCheckpointExists KeepAliveSubstituteReason = "checkpoint_exists"
	// KeepAliveReasonInFlight: Dispatch collided with a commission already in flight for this
	// session (the async path, or another keep-alive dispatch) — checked only inside Dispatch
	// itself via the shared single-flight registry, so it cannot be known at lookup time.
	KeepAliveReasonInFlight KeepAliveSubstituteReason = "in_flight"
	// KeepAliveReasonConcurrencyFull: Dispatch was refused by the global concurrency bound this
	// path shares with the detached async one — the proxy is shedding compaction under load.
	KeepAliveReasonConcurrencyFull KeepAliveSubstituteReason = "concurrency_full"
)

// maxKeepAliveCandidates bounds the registry the same way the keeper's own kaEntry map is
// bounded (proxy/keepalive.go's maxKeepAliveSessions): a session count, not a request-rate
// knob, so a generous but finite ceiling costs nothing in the ordinary case and fails safe in
// the pathological one.
//
// ENFORCED BY REFUSAL, NOT EVICTION: at the ceiling, a NEW session's registration is refused
// outright and the oldest ones are kept — the opposite of what an earlier version of this
// comment claimed ("the oldest registration is simply overwritten"). That was never true: Go's
// map semantics do not evict anything on insert, and the code below has always `return`ed
// before writing a new key once at capacity. The practical effect used to be a registry that
// could fill once and then never register another session until the proxy restarted, because
// nothing ever removed an ENDED session's entry — proxy's keeper (keeper.retire, keeper.forget,
// keeper.evictLocked) now does, by calling ClearKeepAliveCandidate wherever it stops tracking a
// session, and pruneStaleKeepAliveCandidatesLocked below catches whatever that misses (a host
// embedding this package without that keeper, or a session the keeper itself never tracked).
const maxKeepAliveCandidates = 2048

// maxKeepAliveCandidateAge bounds how long a registration may sit unconsumed before it is swept
// as abandoned, regardless of whether anything ever called ClearKeepAliveCandidate for it. Set
// well above the longest gap this repo's own mechanisms expect between a session's turns (the
// `1-hour-head` cache strategy is the longest-lived one it ships) so a session merely slow to
// return is never swept out from under it.
const maxKeepAliveCandidateAge = 2 * time.Hour

// pruneStaleKeepAliveCandidatesLocked drops every candidate older than maxKeepAliveCandidateAge.
// Called with keepAliveCandMu already held, from registerKeepAliveCandidate, so the registry
// self-heals even on a deployment that never retires a session the way proxy's keeper does.
// Proxy's own exits (keeper.retire, keeper.forget, keeper.evictLocked) clear a candidate
// immediately via ClearKeepAliveCandidate and so never depend on this running; this is the
// fallback for everything that does not go through the keeper at all.
func pruneStaleKeepAliveCandidatesLocked(now time.Time) {
	for session, cand := range keepAliveCand {
		if now.Sub(cand.registeredAt) > maxKeepAliveCandidateAge {
			delete(keepAliveCand, session)
		}
	}
}

// registerKeepAliveCandidate stores this turn's commission material as the keep-alive
// substitute, replacing any earlier registration for the session (always safe to replace: the
// stored copy is only ever read later, between requests, and a newer turn's conversation is a
// strict superset of an older one's for the same session).
func (s *CacheAwareSummarizer) registerKeepAliveCandidate(c *components.Ctx, call summaryCaller, path string,
	span []bschemas.ChatMessage, coveredCount int) {
	if c == nil || c.Session == "" || call == nil {
		return
	}
	cand := &keepAliveCandidate{
		s: s,
		// context.WithoutCancel: this candidate is read later, between requests, after the
		// REGISTERING request has long since completed — and completing cancels its own
		// context, which net/http does unconditionally once the handler returns. A keep-alive
		// ping that then tried to call through the raw c.Ctx would get "context canceled" on
		// every single dispatch, which is indistinguishable at a glance from the call itself
		// failing — startAsyncSummary's own detached goroutine already needs the identical
		// detachment for the identical reason (see its own baseCtx).
		ctx:  &components.Ctx{Session: c.Session, Thread: c.Thread, Store: c.Store, Ctx: context.WithoutCancel(c.Ctx)},
		call: call, path: path,
		span:             append([]bschemas.ChatMessage(nil), span...),
		coveredCount:     coveredCount,
		cacheState:       s.trigger.CacheState,
		preExpirySeconds: s.trigger.PreExpirySeconds,
		registeredAt:     time.Now(),
	}
	// Keyed by THREAD, not session (#423): a candidate's span is one thread's conversation, and
	// the keep-alive is per thread. Keyed by session, a subagent's later registration replaced the
	// main thread's candidate, and the main thread's ping then lost its substitute.
	key := thread.Key(c.Session, c.Thread)
	keepAliveCandMu.Lock()
	if _, exists := keepAliveCand[key]; !exists {
		if len(keepAliveCand) >= maxKeepAliveCandidates {
			// Try to make room from entries nothing ever cleared before refusing outright — see
			// pruneStaleKeepAliveCandidatesLocked.
			pruneStaleKeepAliveCandidatesLocked(cand.registeredAt)
		}
		if len(keepAliveCand) >= maxKeepAliveCandidates {
			// Fail toward NOT remembering a new session rather than growing without bound — the
			// consequence is only that this session's idle pings stay bare pings, not that
			// anything already relying on a registration loses it.
			keepAliveCandMu.Unlock()
			return
		}
	}
	keepAliveCand[key] = cand
	keepAliveCandMu.Unlock()
}

// ClearKeepAliveCandidate drops a session's registered substitute material unconditionally.
// Test-only: a real caller on the request path must use ClearStaleKeepAliveCandidate instead —
// see that function's own comment for why an unconditional clear on every real request made
// keep-alive substitution impossible in practice (every request cleared the very candidate its
// own pipeline run had just registered, a few lines earlier in the same call).
func ClearKeepAliveCandidate(session string) {
	if session == "" {
		return
	}
	keepAliveCandMu.Lock()
	delete(keepAliveCand, session)
	keepAliveCandMu.Unlock()
}

// ClearStaleKeepAliveCandidate drops session's registered substitute material ONLY IF it was
// registered before requestStartedAt — i.e. by an EARLIER request's pipeline run, not by this
// one. The proxy calls this once a real request's own pipeline has run (so cache_aware_summarizer
// has already had its chance to register this turn's material) and tr.Session is known, passing
// the wall-clock instant it captured BEFORE running the pipeline.
//
// WHY TIMESTAMP-BASED, NOT CALL-ORDER-BASED. The proxy cannot call this BEFORE the pipeline runs:
// the pipeline is what resolves tr.Session (session.Scoped, off the request's own tenant/header/
// body), so the key this function would need to clear by is not known yet at that point. Calling
// it AFTER the pipeline with an unconditional delete (the bug this function replaces) deletes
// whatever the pipeline's own run just wrote, a few lines earlier in the exact same request —
// cache_aware_summarizer's registration and the "drop a superseded one" cleanup were racing each
// other within a single, synchronous call stack, and the cleanup always won. A timestamp
// comparison needs no key resolved in advance: requestStartedAt is captured the instant this
// request began, trivially available before the pipeline runs, and a candidate's own
// registeredAt field (set the moment registerKeepAliveCandidate writes it) answers "was this
// written by code that ran before or after that instant" without the caller ever needing to name
// the session before now.
//
// A registration from a CONCURRENT request for the same session (two requests on one session
// racing each other) can register after requestStartedAt was captured but before this call runs
// — that candidate's registeredAt is then >= requestStartedAt and survives, which is correct: it
// is not stale, it is simply newer than the request now doing the clearing.
func ClearStaleKeepAliveCandidate(session string, requestStartedAt time.Time) {
	if session == "" {
		return
	}
	keepAliveCandMu.Lock()
	if cand, exists := keepAliveCand[session]; exists && cand.registeredAt.Before(requestStartedAt) {
		delete(keepAliveCand, session)
	}
	keepAliveCandMu.Unlock()
}

// checkpointRefreshOpportunity reports whether an existing checkpoint, matched against candSpan
// (the latest registered commission span), has any new tail material a refresh could fold in —
// and if so, how many tokens that tail is. Used only for a `cache_state: pre_expiry` candidate:
// see KeepAliveSubstitute's own comment on why `any` never asks this question.
//
// Deliberately NOT gated on resummarize_tokens — that knob keeps its original meaning for the
// TURN-based re-summarize decision (`Offload`'s own tail check) and plays no part here since
// issue #415: whether a refresh is worth DISPATCHING is a money question answered by
// fireSummarySubstitute's own gate (expected saving vs. projected cost), not a token-count
// threshold tuned for a different decision. This function answers only "is there anything new to
// weigh at all" — ok=false (and tailTokens=0) when the tail is empty, which is the one case
// categorically not worth pricing: zero new tokens cannot earn back any nonzero call cost.
func checkpointRefreshOpportunity(cp sumCheckpoint, candSpan []bschemas.ChatMessage) (tailTokens int, ok bool) {
	if cp.CoveredCount <= 0 || cp.CoveredCount > len(candSpan) {
		return 0, true // nothing recognizable to compare against — treat as an opportunity, as before
	}
	if spanHash(candSpan[:cp.CoveredCount]) != cp.CoveredHash {
		return 0, true // prefix diverged — not the same conversation this checkpoint covers
	}
	tail := candSpan[cp.CoveredCount:]
	if len(tail) == 0 {
		return 0, false // truly current: nothing new since the checkpoint was written
	}
	return schema.MessagesTokens(&bschemas.BifrostChatRequest{Input: tail}), true
}

// KeepAliveSubstitute reports whether a session has commission material the idle keep-alive may
// use instead of a bare ping, and a Dispatch function to run it.
//
// ok=false means fall back to an ordinary ping either way, but reason now says WHY — see
// KeepAliveSubstituteReason. Dispatch's own in-flight/concurrency refusal cannot be known at
// lookup time (it is checked inside Dispatch itself, via the shared single-flight registry), so
// that is reported on KeepAliveSummaryResult.Reason instead, once Dispatch has actually run.
//
// Dispatch BLOCKS until the call resolves (or the given timeout elapses) and runs the real model
// call — the keeper's caller must therefore run it off its own goroutine, exactly as it already
// does for an ordinary ping.
//
// info is returned alongside ok=true so the caller can additionally check cache_state against
// its OWN timing before deciding to dispatch — see KeepAliveCandidateInfo.
//
// AN EXISTING CHECKPOINT REFUSES SUBSTITUTION, UNLESS IT IS A `pre_expiry` RESERVE WITH SOMETHING
// NEW TO FOLD IN. For `any` — "no combination wastes calls: with any, keep today's behaviour
// exactly" — any existing checkpoint refuses, unconditionally, precisely as before: the next real
// turn splices it for free, so a call here would summarize a span nobody is waiting on. For
// `pre_expiry`, a reserve with an empty tail refuses the same way (there is nothing to gain from
// paying for a summary identical to the one already held); a reserve with ANY nonempty tail is
// offered to the caller as `info.IsRefresh` with `info.RefreshTailTokens` set, for
// fireSummarySubstitute's own money gate (issue #415) to weigh against the call's cost before
// actually dispatching — this function does not itself decide whether that tail is "enough".
func KeepAliveSubstitute(session string) (dispatch func(timeout time.Duration) KeepAliveSummaryResult, info KeepAliveCandidateInfo, reason KeepAliveSubstituteReason, ok bool) {
	keepAliveCandMu.Lock()
	cand, exists := keepAliveCand[session]
	keepAliveCandMu.Unlock()
	if !exists {
		return nil, KeepAliveCandidateInfo{}, KeepAliveReasonNoCandidate, false
	}
	isRefresh := false
	tailTokens := 0
	if cp, has := loadCheckpoint(cand.ctx); has {
		if cand.cacheState != components.CacheStatePreExpiry {
			return nil, KeepAliveCandidateInfo{}, KeepAliveReasonCheckpointExists, false
		}
		var opportunity bool
		tailTokens, opportunity = checkpointRefreshOpportunity(cp, cand.span)
		if !opportunity {
			return nil, KeepAliveCandidateInfo{}, KeepAliveReasonCheckpointExists, false
		}
		// pre_expiry AND something new since the reserve: fall through, flagged as a refresh —
		// fireSummarySubstitute decides whether that tail is actually worth paying for.
		isRefresh = true
	}
	info = KeepAliveCandidateInfo{CacheState: cand.cacheState, PreExpirySeconds: cand.preExpirySeconds,
		IsRefresh: isRefresh, RefreshTailTokens: tailTokens}
	// reserved: a pre_expiry candidate is only ever offered when the keeper's OWN clock agrees
	// this ping is inside the pre-expiry window (fireSummarySubstitute's phase check), so a
	// summary committed from here is exactly as "not yet applied" as one committed directly from
	// a PreExpiry-phase turn in Offload — see commitAsyncSummary's own comment on Reserved.
	reserved := cand.cacheState == components.CacheStatePreExpiry
	return func(timeout time.Duration) KeepAliveSummaryResult {
		return cand.s.commissionSync(cand.ctx, cand.call, cand.path, reserved, cand.span, cand.coveredCount, timeout)
	}, info, KeepAliveReasonNone, true
}

// RegisterKeepAliveCandidateForTest installs commission material directly, for proxy's keeper
// tests to exercise KeepAliveSubstitute without driving a real Offload call through a mocked
// model and a full pipeline Ctx. Test-only: nothing outside a test has a legitimate reason to
// register a candidate that was not produced by an actual commission, which is why this is the
// one function in this file that does not require a *CacheAwareSummarizer the registry built
// itself — it builds the minimal receiver inline.
//
// call takes the plain, unnamed function shape rather than the package-private summaryCaller
// type, so a test in another package (proxy's keeper tests) can build one from either a mocked
// MessagesModel or a mocked PrefixAsker without needing to see that type at all — Go's
// assignability rules let an unnamed function literal satisfy a named parameter of the identical
// underlying type regardless of which package declared it.
//
// cacheState/preExpirySeconds let a test drive KeepAliveCandidateInfo's values without
// constructing a real component through its registered constructor — "" behaves as `any`,
// matching a component whose trigger never set cache_state at all.
func RegisterKeepAliveCandidateForTest(session string, st store.Store,
	call func(ctx context.Context) (string, error), path string, span []bschemas.ChatMessage, coveredCount int,
	cacheState string, preExpirySeconds int) {
	RegisterThreadKeepAliveCandidateForTest(session, thread.Primary, st, call, path, span, coveredCount,
		cacheState, preExpirySeconds)
}

// RegisterThreadKeepAliveCandidateForTest is RegisterKeepAliveCandidateForTest for one thread of
// the session. The registry key is then thread.Key(session, threadID).
func RegisterThreadKeepAliveCandidateForTest(session, threadID string, st store.Store,
	call func(ctx context.Context) (string, error), path string, span []bschemas.ChatMessage, coveredCount int,
	cacheState string, preExpirySeconds int) {
	(&CacheAwareSummarizer{mode: markerFull,
		trigger: components.Trigger{CacheState: cacheState, PreExpirySeconds: preExpirySeconds},
	}).registerKeepAliveCandidate(
		&components.Ctx{Session: session, Thread: threadID, Store: st, Ctx: context.Background()},
		call, path, span, coveredCount)
}

// SeedReservedCheckpointForTest writes a `Reserved` checkpoint directly to the store, covering
// exactly coveredSpan, for a proxy-side test to put a session into "has a pre_expiry reserve with
// a tail of known size" without driving a real commission through Offload. Test-only, for the
// same reason RegisterKeepAliveCandidateForTest is: a legitimate caller only ever gets a
// checkpoint from an actual commission.
//
// A later RegisterKeepAliveCandidateForTest call whose span starts with these exact messages (byte
// for byte) gives checkpointRefreshOpportunity a prefix hash it recognizes, with everything past
// coveredSpan read as the refresh's own tail — this is the one lever a test needs to control
// info.RefreshTailTokens without needing to see spanHash/sumCheckpoint, which stay package-private.
func SeedReservedCheckpointForTest(session string, st store.Store, coveredSpan []bschemas.ChatMessage, summaryText string) {
	c := &components.Ctx{Session: session, Store: st, Ctx: context.Background()}
	saveCheckpoint(c, sumCheckpoint{
		SummaryMsg: summaryText, CoveredCount: len(coveredSpan), CoveredHash: spanHash(coveredSpan),
		Key: "test-key", Reserved: true,
	})
}
