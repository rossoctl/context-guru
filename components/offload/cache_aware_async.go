package offload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

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

// startAsyncSummary commissions the summary and returns a gate name when it declined to, so the
// caller can file it. Everything the goroutine needs is read HERE, on the request's goroutine —
// reading c.Store or calling effectiveMode from inside the goroutine would put a concurrent read
// on a struct the request owns, which is the shape a review already caught once in summarize.
func (s *CacheAwareSummarizer) startAsyncSummary(c *components.Ctx, mm components.MessagesModel,
	ask, span []bschemas.ChatMessage, coveredCount int) string {
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

	askCopy := make([]bschemas.ChatMessage, len(ask))
	copy(askCopy, ask)
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
		out, err := mm.CompleteMessages(ctx, "", askCopy)
		// Deferred BEFORE the error check: a call that timed out may still have been billed for its
		// input, and a cost we incurred is a cost we report.
		deferUsage(st, session, callSink)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
				atomic.AddInt64(&cacheAwareTimeouts, 1)
			} else {
				atomic.AddInt64(&cacheAwareErrors, 1)
			}
			return
		}
		if strings.TrimSpace(out) == "" {
			atomic.AddInt64(&cacheAwareEmpty, 1)
			return
		}
		s.commitAsyncSummary(mode, session, st, spanCopy, out, coveredCount)
	}()
	return ""
}

// commitAsyncSummary stashes the span and writes the checkpoint, in that order, from the background
// goroutine. Stash first: a checkpoint whose marker resolves to nothing is an irreversible loss
// dressed up as a reversible one. If the stash is refused NO checkpoint is written, and the next
// turn simply sends what it would have sent anyway.
func (s *CacheAwareSummarizer) commitAsyncSummary(mode markerMode, session string, st store.Store,
	span []bschemas.ChatMessage, raw string, coveredCount int) {
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
		CoveredHash: spanHash(span), Key: key,
	}); err == nil {
		st.Put(store.SumPrefix+session, b)
		atomic.AddInt64(&cacheAwareAsyncCommitted, 1)
	}
}
