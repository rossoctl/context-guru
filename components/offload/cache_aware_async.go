package offload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/cheapmodel"
	"github.com/rossoctl/context-guru/internal/logging"
	"github.com/rossoctl/context-guru/store"
)

// The detached summarization path, mirroring summarize_async.go and REUSING its machinery:
// the same per-session flight registry (inFlight), the same global concurrency bound
// (summarySlots), the same deferred-usage accounting, the same panic discipline.
//
// WHY THE CALL CANNOT BE SYNCHRONOUS. A summary here covers most of the transcript, so the call
// is large by construction and its budget is 300 s. Doing that inline puts a multi-minute stall on
// the turn that triggered it, billed against the agent's own timeout — summarize_async.go exists
// because that was already found too expensive to do on the request path. A component whose whole
// argument is about cost cannot pay for its own compaction in latency the agent notices.
//
// SO THE CONTROL FLOW IS TWO-TURN, exactly as summarize's is. The turn that triggers commissions
// a summary and forwards UNTOUCHED; the next eligible turn finds the checkpoint and splices. The
// checkpoint is the only channel — this goroutine has no request to mutate and no Report to file
// against, because both belong to a turn that has already been answered.
//
// ⚠️ Sharing inFlight with summarize is safe for the same reason sharing the checkpoint is: both
// components restructure the message list and therefore must run ALONE, so they cannot both be in
// one pipeline. It also means WaitForSummaryForTest drains this path too, which is what lets a test
// drive the real two-turn sequence rather than a sleep.
var (
	cacheAwareAsyncStarted   int64
	cacheAwareAsyncCommitted int64
	cacheAwareAsyncRefused   int64
	cacheAwareAsyncPanics    int64
)

// CacheAwareAsyncStats returns (started, committed, refused, panics). The PAIR is the signal:
// started without committed is a summary that was paid for and lost, which no other counter here
// would reveal. Refused is the global bound shedding compaction under load rather than queueing a
// call nobody is waiting for.
func CacheAwareAsyncStats() (started, committed, refused, panics int64) {
	return atomic.LoadInt64(&cacheAwareAsyncStarted),
		atomic.LoadInt64(&cacheAwareAsyncCommitted),
		atomic.LoadInt64(&cacheAwareAsyncRefused),
		atomic.LoadInt64(&cacheAwareAsyncPanics)
}

// summaryCaller is how a commission actually reaches a model, built ONCE in Offload on the
// request's own goroutine and then carried, opaquely, through every path that may dispatch it
// later (the detached async path, the cold-turn deferral, the keep-alive substitute). It has
// exactly two implementations, both in Offload: a MessagesModel sent the whole conversation plus
// one appended instruction, or — on a route whose client has no MessagesModel at all, which is
// every Anthropic incoming-model request today (#275: internal/cheapmodel.Anthropic implements
// only Complete) — a components.PrefixAsker appending one question to the provider's own cached
// prefix (the previous turn's SENT body, held by the host). Everything downstream of Offload
// neither knows nor needs to know which one it is holding; both record cache_read/cache_write
// into the ambient cheapmodel sink identically (CompleteMessages and PrefixAsker.Ask's underlying
// CompletePrefixed/CompletePrefixedResponses both call cheapmodel.recordUsageCache), so
// deferUsage/takeDeferredUsage and commissionSync's own sink read work unmodified either way.
type summaryCaller func(ctx context.Context) (string, error)

// classifyCallErr counts and, for a genuine failure, WARN-logs a summaryCaller's error — shared
// by every path that invokes one. ErrNoPrefix is NOT a failure: it is what a PrefixAsker-backed
// caller returns on a session's first turn, when there is nothing stashed yet to append to, and
// counting or logging that as a failure would make the ordinary, once-per-session case read as
// something wrong. A MessagesModel-backed caller can never return it, so this is unconditionally
// safe to call either way.
//
// The WARN is the fix for the exact gap found live on this deployment's extract_llm_sweep
// (sweep_ask_failed, cause not recorded): a provider error folded into a silent decline is a
// clue an operator reaching for CG_LOG_LEVEL=debug specifically wants and could not get.
func classifyCallErr(ctx context.Context, session string, err error) {
	if errors.Is(err, components.ErrNoPrefix) {
		atomic.AddInt64(&cacheAwareNoPrefix, 1)
		return
	}
	if errors.Is(err, components.ErrStalePrefix) {
		atomic.AddInt64(&cacheAwareStalePrefix, 1)
		logging.From(ctx).Debug("cg.cache_aware_summarizer.stale_prefix", "session", session)
		return
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		atomic.AddInt64(&cacheAwareTimeouts, 1)
	} else {
		atomic.AddInt64(&cacheAwareErrors, 1)
	}
	logging.From(ctx).Warn("cg.cache_aware_summarizer.call_failed", "session", session, "err", err)
}

// cacheAwareSummaryIncomplete reports whether a RAW model reply is missing the closing
// `</summary>` tag the summarizer prompt asks for (summarizer_model_profiles.yaml: "wrapped in
// <summary></summary> tags"). Checked on `out` as the call returned it — BEFORE sanitizeSummary
// (which strips every spelling of that tag as an untrusted control string; see its own doc) and
// BEFORE ensureSummaryTags (which unconditionally re-adds a missing closing tag). Checking after
// either of those would always see "complete", because the second one manufactures the very tag
// being tested for: a reply cut off mid-generation by the model's own token or thinking budget
// would be repaired into a checkpoint that looks exactly like a finished summary and replaces
// real history with a fragment.
//
// `summarize`'s OWN prompt (summarizerUserPrompt) asks for the identical format, so it has the
// same theoretical exposure — but ensureSummaryTags/sanitizeSummary are summarize's long-shipped,
// deliberately lenient contract: it already tolerates a model that never wraps in tags at all
// (every one of its own test fixtures uses untagged stub text on the strength of that leniency).
// Retrofitting a strict completeness gate onto summarize's accepted contract is a separate,
// bigger behaviour change this fix does not make — see cache_aware_summarizer's own PR for the
// reasoning. This check is therefore local to cache_aware_summarizer, not added to the shared
// ensureSummaryTags/sanitizeSummary helpers themselves.
func cacheAwareSummaryIncomplete(raw string) bool {
	return !strings.Contains(raw, "</summary>")
}

// startAsyncSummary commissions the summary and returns a gate name when it declined to, so the
// caller can file it. Everything the goroutine needs is read HERE, on the request's goroutine —
// reading c.Store or calling effectiveMode from inside the goroutine would put a concurrent read
// on a struct the request owns, which is the shape a review already caught once in summarize.
func (s *CacheAwareSummarizer) startAsyncSummary(c *components.Ctx, call summaryCaller, path, trigger string,
	reserved bool, span []bschemas.ChatMessage, coveredCount int) string {
	j, ok := inFlight.begin(c.Session)
	if !ok {
		return "summary_already_in_flight"
	}
	// The global bound, acquired WITHOUT blocking: a saturated proxy declines to compact rather
	// than queueing a call nobody is waiting for. Release the per-session flight first so a
	// refusal here does not leave the session marked busy.
	select {
	case summarySlots <- struct{}{}:
	default:
		inFlight.finish(c.Session, j)
		atomic.AddInt64(&cacheAwareAsyncRefused, 1)
		return "summary_concurrency_full"
	}
	atomic.AddInt64(&cacheAwareAsyncStarted, 1)
	// Logged because the counters cannot survive a restart and this is the only record that can: a
	// commission with no matching resolution line is a call that was paid for and lost.
	logging.From(c.Ctx).Info("cache_aware_summarizer: commissioned a detached summary",
		"session", c.Session, "covered_messages", coveredCount)

	spanCopy := make([]bschemas.ChatMessage, len(span))
	copy(spanCopy, span)
	session, st, mode := c.Session, c.Store, effectiveMode(c, s.mode)
	// Detached from the request's cancellation but keeping its logger, so the resolution line
	// carries the same request-scoped fields the commission line did.
	baseCtx := context.WithoutCancel(c.Ctx)

	go func() {
		defer inFlight.finish(session, j)
		defer func() {
			<-summarySlots
			logging.From(baseCtx).Info("cache_aware_summarizer: detached summary resolved",
				"session", session)
		}()
		// Fail open, always: a panic in a detached goroutine takes the process down, which is far
		// worse than a missing summary — and the pipeline's own recover cannot reach here. NOT
		// silent, because nothing on the hot path waits for this call, so a panicking summarizer
		// would otherwise show up only as a growing started/committed gap.
		defer func() {
			if r := recover(); r != nil {
				atomic.AddInt64(&cacheAwareAsyncPanics, 1)
				logging.From(baseCtx).Error("cache_aware_summarizer: detached summary panicked; no "+
					"summary was committed for this turn", "session", session, "panic", fmt.Sprint(r))
			}
		}()
		ctx, cancel := context.WithTimeout(baseCtx, cacheAwareTimeout)
		defer cancel()
		// A DETACHED sink: this goroutine inherits the commissioning request's context, so a
		// chained sink would charge one call twice — once to that request's row and again on the
		// replay. Detaching makes the attribution single-valued.
		ctx, callSink := cheapmodel.WithDetachedSink(ctx)
		atomic.AddInt64(&cacheAwareCalls, 1)
		callStart := time.Now()
		out, err := call(ctx)
		callMs := float64(time.Since(callStart).Milliseconds())
		recordCacheAwareCacheTokens(callSink)
		// Deferred BEFORE the error check: a call that timed out may still have been billed for its
		// input, and a cost we incurred is a cost we report.
		deferUsage(st, session, "cache_aware_summarizer", path, trigger, callMs, callSink)
		if err != nil {
			classifyCallErr(ctx, session, err)
			return
		}
		if strings.TrimSpace(out) == "" {
			atomic.AddInt64(&cacheAwareEmpty, 1)
			return
		}
		if cacheAwareSummaryIncomplete(out) {
			atomic.AddInt64(&cacheAwareSummaryTruncated, 1)
			logging.From(baseCtx).Warn("cg.cache_aware_summarizer.summary_truncated",
				"session", session)
			return
		}
		s.commitAsyncSummary(mode, session, st, spanCopy, out, coveredCount, reserved)
	}()
	return ""
}

// recordCacheAwareCacheTokens adds one call's cache tiers to the process-wide /stats counters —
// the direct answer to "is cache_aware_summarizer's own call actually reading warm", which the
// component's whole design rests on and which was otherwise visible only by reading a dashboard
// row (and, before this round, not even that).
func recordCacheAwareCacheTokens(sink *cheapmodel.Sink) {
	cw, cr := sink.CacheTotals()
	atomic.AddInt64(&cacheAwareCacheWriteTokens, cw)
	atomic.AddInt64(&cacheAwareCacheReadTokens, cr)
}

// commitAsyncSummary stashes the span and writes the checkpoint, in that order, from the background
// goroutine. Stash first: a checkpoint whose marker resolves to nothing is an irreversible loss
// dressed up as a reversible one. If the stash is refused NO checkpoint is written, and the next
// turn simply sends what it would have sent anyway.
// reserved marks the checkpoint as commissioned under cache_state: pre_expiry and not yet
// applied — see sumCheckpoint.Reserved and tryReuse's own comment on how it graduates. Always
// false for a commission summarize ever makes (it shares this store key namespace but never
// reserves) and for cache_aware_summarizer's own cache_state: any (which has no reserve concept
// at all — a fresh checkpoint there is live the moment it is committed, exactly as before).
func (s *CacheAwareSummarizer) commitAsyncSummary(mode markerMode, session string, st store.Store,
	span []bschemas.ChatMessage, raw string, coveredCount int, reserved bool) {
	// The reply is UNTRUSTED. The whole trajectory reached the summarizer, so planted text in any
	// tool output had a long run at it — strip forged expand markers (both spellings), the summary
	// sentinel and a premature </summary> before this text is framed as trustworthy context.
	summary := ensureSummaryTags(sanitizeSummary(raw))
	var key string
	if mode == markerFull {
		spanJSON, err := json.Marshal(span)
		if err != nil {
			return
		}
		key = hashKey(string(spanJSON))
		if !store.PutStash(st, key, spanJSON) {
			atomic.AddInt64(&cacheAwareRefusedStash, 1)
			return
		}
	}
	summaryText := cacheAwareSummaryWrapper(summary, key, mode)
	if b, err := json.Marshal(sumCheckpoint{
		SummaryMsg: summaryText, CoveredCount: coveredCount,
		CoveredHash: spanHash(span), Key: key, Reserved: reserved,
	}); err == nil {
		st.Put(store.SumPrefix+session, b)
		atomic.AddInt64(&cacheAwareAsyncCommitted, 1)
	}
}

// KeepAliveSummaryResult is what one keep-alive-substituted summary call cost and produced, read
// straight off the model call's own usage sink so the keeper can book it exactly as it would a
// ping — see cache_aware_keepalive.go.
type KeepAliveSummaryResult struct {
	Model                                     string
	FreshInput, Output, CacheWrite, CacheRead int
	// CacheWrite1h is the SUBSET of CacheWrite this call billed at the one-hour write premium
	// (the `1-hour-head` cache strategy's own breakpoint) — never an addition to CacheWrite. The
	// keeper's recordSummarySubstitute needs this to price the call with
	// modelinfo.Price.CostWithCacheWrite1h the same way record1 prices an ordinary ping, instead
	// of folding every write into the five-minute rate.
	CacheWrite1h int
	// Committed is false either because the call could not be started (another commission —
	// async or keep-alive — is already in flight for this session, or the global concurrency
	// bound is full), or because it ran and produced nothing usable (error, timeout, empty
	// reply). Either way the caller's answer is the same: fall back to an ordinary ping.
	Committed bool
	// Reason explains a false Committed — KeepAliveReasonInFlight or KeepAliveReasonConcurrencyFull
	// for the two refusals that happen before any call is made, or "" (KeepAliveReasonNone) for a
	// call that was made and simply did not commit (its own error/empty counters already say why —
	// see classifyCallErr and cacheAwareEmpty).
	Reason KeepAliveSubstituteReason
}

// commissionSync runs the whole commission call INLINE and blocks until it resolves, for the one
// caller that cannot use the detached startAsyncSummary path: the idle keep-alive substitution,
// which needs to know whether a usable summary landed before it decides to fall back to an
// ordinary ping (see cache_aware_keepalive.go). It is called from the keeper's own ping goroutine,
// which is already off the request path and already budgets its own timeout — unlike
// startAsyncSummary there is no request here to avoid stalling.
//
// It shares startAsyncSummary's single-flight registry and global concurrency bound rather than
// duplicating either: a session has, at most, one commission in flight at a time regardless of
// which caller started it, and the two callers racing to summarize the same span would otherwise
// write the same checkpoint twice.
func (s *CacheAwareSummarizer) commissionSync(c *components.Ctx, call summaryCaller, path string,
	reserved bool, span []bschemas.ChatMessage, coveredCount int, timeout time.Duration) KeepAliveSummaryResult {
	j, ok := inFlight.begin(c.Session)
	if !ok {
		return KeepAliveSummaryResult{Reason: KeepAliveReasonInFlight}
	}
	defer inFlight.finish(c.Session, j)
	select {
	case summarySlots <- struct{}{}:
	default:
		atomic.AddInt64(&cacheAwareAsyncRefused, 1)
		return KeepAliveSummaryResult{Reason: KeepAliveReasonConcurrencyFull}
	}
	defer func() { <-summarySlots }()
	atomic.AddInt64(&cacheAwareAsyncStarted, 1)
	logging.From(c.Ctx).Info("cache_aware_summarizer: commissioned a keep-alive-substitute summary",
		"session", c.Session, "covered_messages", coveredCount)
	defer logging.From(c.Ctx).Info("cache_aware_summarizer: keep-alive-substitute summary resolved",
		"session", c.Session)

	session, st, mode := c.Session, c.Store, effectiveMode(c, s.mode)
	if timeout <= 0 {
		timeout = cacheAwareTimeout
	}
	ctx, cancel := context.WithTimeout(c.Ctx, timeout)
	defer cancel()
	// A DETACHED sink: this call is not attributed to any one request either, same as the async
	// path's own goroutine — see its comment on why a chained sink would double-charge.
	ctx, callSink := cheapmodel.WithDetachedSink(ctx)
	atomic.AddInt64(&cacheAwareCalls, 1)
	callStart := time.Now()
	out, err := call(ctx)
	callMs := float64(time.Since(callStart).Milliseconds())
	// Deferred BEFORE the error check, and READ before being deferred: a call that timed out may
	// still have been billed for its input, and the result the keeper books is the same figures
	// deferUsage is about to replay onto this session's next real turn — the keeper's ping ledger
	// and the ordinary LLM-cost ledger are two different books of the SAME spend, not two spends.
	_, in, outTok := callSink.Totals()
	cw, cr := callSink.CacheTotals()
	res := KeepAliveSummaryResult{Model: callSink.Model(),
		FreshInput: int(in), Output: int(outTok), CacheWrite: int(cw), CacheRead: int(cr),
		CacheWrite1h: int(callSink.CacheWrite1h())}
	recordCacheAwareCacheTokens(callSink)
	deferUsage(st, session, "cache_aware_summarizer", path, "keepalive_substitute", callMs, callSink)
	if err != nil {
		classifyCallErr(ctx, session, err)
		return res
	}
	if strings.TrimSpace(out) == "" {
		atomic.AddInt64(&cacheAwareEmpty, 1)
		return res
	}
	if cacheAwareSummaryIncomplete(out) {
		atomic.AddInt64(&cacheAwareSummaryTruncated, 1)
		logging.From(c.Ctx).Warn("cg.cache_aware_summarizer.summary_truncated", "session", session)
		return res
	}
	committedBefore := atomic.LoadInt64(&cacheAwareAsyncCommitted)
	s.commitAsyncSummary(mode, session, st, span, out, coveredCount, reserved)
	res.Committed = atomic.LoadInt64(&cacheAwareAsyncCommitted) != committedBefore
	return res
}
