package offload

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/store"
)

// Producing a summary WITHOUT holding the request that decided to produce one.
//
// # Why the hot path must not wait
//
// summarize's trigger fires when the prompt cache is at or past the end of its life, and the
// component then called the model INLINE with a 300-second budget. Those two facts are
// incompatible: `pre_expiry_seconds` is 60, so a call that takes longer than a minute guarantees
// the entry dies while we are holding the request. Measured on a live run — one call sat for its
// full 300s (upstream itself took 2.8s), the entry expired mid-pipeline, and the request went
// upstream with the FULL 197k transcript, paying the exact rewrite the trigger exists to avoid
// plus a wasted model call. Best case turned into worst case by latency alone.
//
// The same call shape measured directly against the same gateway completes in 5-14s on haiku and
// 6-19s on sonnet, so the hang was not the model and its cause is not established. It does not need
// to be: a design whose worst case is bounded by our own latency should not have that worst case at
// all.
//
// So the firing turn starts the work and forwards immediately with whatever checkpoint it already
// has. A LATER turn splices the result.
//
// # Why there is no keep-alive ping here
//
// The obvious-looking companion — ping the entry to hold it alive while the summary runs — is pure
// waste, and it is worth writing down so nobody adds it back. A ping holds the OLD, FULL prefix
// alive at 0.1x its size. That is the prefix the summary is about to replace, so we would be paying
// to preserve something we intend to discard. Expiry costs nothing once a summary exists: the next
// turn writes the compacted prefix at 1.25xC whether or not the old entry survived.
//
// # Why waiting on a later turn IS worth it
//
// A turn that arrives while a summary is in flight waits for it, up to a cap. The alternative is
// forwarding the full transcript: 1.25xF, and at high fill a real risk of the provider's hard limit
// (observed: `400 prompt is too long: 203,705 > 200,000`). Ten seconds of latency against a 197k
// write — about $0.22 on haiku, $2.70 on Opus at 0.9 of 1M — is a good trade on a turn where the
// agent is already waiting seconds for the model.
//
// EXPECT THAT WAIT TO BE THE NORMAL PATH, not a rare one. The trigger fires only after an idle gap
// of 240s+, so the user is active immediately afterwards; in an agent loop the next request arrives
// 1-3 seconds later while the summary needs 5-14s. A typical episode therefore carries ONE turn
// that stalls for a few seconds. The cap is for the pathological tail, not the common case.

// defaultSummaryWait is how long a later turn will wait for an in-flight summary.
//
// 120 seconds: long enough that the measured 5-19s case always lands and even a badly degraded
// call usually does, short enough that a hung call cannot hold an agent for the summarizer's own
// (much longer) budget. It is a latency ceiling, not a correctness parameter — whatever happens,
// the turn proceeds.
const defaultSummaryWait = 120 * time.Second

// The detached call's budget is summarizeCallTimeout — the SAME budget the synchronous path uses,
// and the same one CONTEXT_GURU_SUMMARIZE_TIMEOUT tunes.
//
// A second, private constant here was the obvious first move ("a summary that lands late is still
// useful, so give the background call longer") and it was wrong twice over: an operator who tunes
// the summarize timeout would reasonably expect it to apply to the call summarize makes, and a
// budget nothing can reach from configuration is a budget nobody can shorten when a provider goes
// bad. The "landing late is still useful" property is already provided by the WAIT CAP being
// shorter than the budget — a turn stops waiting at 120s while the call keeps going to 300s — so
// nothing needed a second constant to express it.
func asyncSummaryBudget() time.Duration { return summarizeCallTimeout }

// summaryJob is one in-flight summary for one session.
type summaryJob struct {
	done chan struct{}
	// started is when the job began, for the metric that matters when tuning the wait cap: how
	// long turns actually spend waiting.
	started time.Time
}

// summaryFlight is the per-session single-flight registry.
//
// SINGLE-FLIGHT IS NOT AN OPTIMIZATION HERE. Without it a busy session fires the trigger on turn N,
// forwards, and fires again on turn N+1 while the first call is still running — so a session that
// stays cold queues one full-transcript model call per turn, each one paying for a ~48k-token
// prompt, and the last writer's checkpoint wins arbitrarily. The registry is what makes "start the
// work and forward" safe to do on every turn.
type summaryFlight struct {
	mu   sync.Mutex
	jobs map[string]*summaryJob
}

var inFlight = &summaryFlight{jobs: map[string]*summaryJob{}}

// begin claims the flight for a session. ok=false means one is already running, and the caller must
// NOT start a second.
func (f *summaryFlight) begin(session string) (*summaryJob, bool) {
	if session == "" {
		// No session id means no way to tell two conversations apart, so there is nothing to
		// single-flight ON. Refuse rather than share one slot between unrelated transcripts.
		return nil, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, running := f.jobs[session]; running {
		return nil, false
	}
	j := &summaryJob{done: make(chan struct{}), started: time.Now()}
	f.jobs[session] = j
	return j, true
}

// finish releases the flight and wakes every waiter. Idempotent, because it runs from a defer on a
// goroutine that may also have panicked.
func (f *summaryFlight) finish(session string, j *summaryJob) {
	f.mu.Lock()
	if cur, ok := f.jobs[session]; ok && cur == j {
		delete(f.jobs, session)
	}
	f.mu.Unlock()
	select {
	case <-j.done: // already closed
	default:
		close(j.done)
	}
}

// peek reports the in-flight job for a session, if any.
func (f *summaryFlight) peek(session string) (*summaryJob, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[session]
	return j, ok
}

// waitFor blocks until the session's in-flight summary finishes, the cap expires, or the REQUEST's
// own context is cancelled — whichever comes first. It reports whether a summary landed.
//
// The request context is in the select on purpose: a client that has already given up must not be
// held here, and a wait that outlived its request would be holding a goroutine for nobody.
func (f *summaryFlight) waitFor(ctx context.Context, session string, cap time.Duration) (waited time.Duration, landed bool) {
	j, ok := f.peek(session)
	if !ok {
		return 0, false
	}
	if cap <= 0 {
		cap = defaultSummaryWait
	}
	t := time.NewTimer(cap)
	defer t.Stop()
	start := time.Now()
	select {
	case <-j.done:
		return time.Since(start), true
	case <-t.C:
		return time.Since(start), false
	case <-ctx.Done():
		return time.Since(start), false
	}
}

// summaryWait is the configured cap, or the default.
func (s *Summarize) summaryWait() time.Duration {
	if s.summaryWaitSeconds > 0 {
		return time.Duration(s.summaryWaitSeconds) * time.Second
	}
	return defaultSummaryWait
}

// startAsyncSummary produces a summary in the background and saves it as a checkpoint. It returns
// immediately; nothing about this request's response depends on it.
//
// The span is COPIED before the goroutine starts. req.Input is the live request body, which apply
// rebuilds and the caller keeps mutating after this returns — handing a goroutine a slice that
// aliases it is a data race and, worse, a summary of whatever the transcript became rather than of
// what was measured.
//
// The context is DETACHED from the request. c.Ctx is cancelled the moment the response is written,
// so a background call inheriting it would be cancelled essentially always — the failure mode being
// a component that appears to work, files its event, and never produces anything.
func (s *Summarize) startAsyncSummary(c *components.Ctx, model components.Model,
	span []bschemas.ChatMessage, goal string, coveredCount int) bool {
	j, ok := inFlight.begin(c.Session)
	if !ok {
		return false
	}
	atomic.AddInt64(&summarizeAsyncStarted, 1)
	spanCopy := make([]bschemas.ChatMessage, len(span))
	copy(spanCopy, span)
	// Everything the goroutine needs, read HERE while we are still on the request's goroutine.
	session, store := c.Session, c.Store
	go func() {
		defer inFlight.finish(session, j)
		// Fail open, always: a panic in a detached goroutine takes the process down, which is a
		// far worse outcome than a missing summary. The pipeline's own recover cannot reach here.
		defer func() { _ = recover() }()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Ctx), asyncSummaryBudget())
		defer cancel()
		summary, err := s.summarize(ctx, model, spanCopy, goal)
		if err != nil {
			// Classified here rather than swallowed: with nothing on the hot path waiting for
			// this call, these two counters are the ONLY place a degraded summarizer shows up.
			// The pair (asyncStarted, asyncCommitted) says work is being lost; these say why.
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
				atomic.AddInt64(&summarizeTimeouts, 1)
			} else {
				atomic.AddInt64(&summarizeErrors, 1)
			}
			return
		}
		if strings.TrimSpace(summary) == "" {
			return
		}
		s.commitAsyncSummary(c, session, store, spanCopy, summary, coveredCount)
	}()
	return true
}

// commitAsyncSummary stashes the span and writes the checkpoint, from the background goroutine.
//
// It does the same two things the synchronous path does and in the same order — stash first, then
// checkpoint — because a checkpoint whose marker resolves to nothing is an irreversible loss
// dressed up as a reversible one. If the stash is refused (the rewind reserve is full), NO
// checkpoint is written: the next turn then simply sends what it would have sent anyway.
//
// It deliberately does NOT touch req.Input or the Report. This goroutine has no request to mutate
// and no Report to file against — both belong to a turn that has already been answered. Everything
// it produces reaches the wire through the next turn's replay, which is the one place that splice
// happens.
func (s *Summarize) commitAsyncSummary(c *components.Ctx, session string, st store.Store,
	span []bschemas.ChatMessage, summary string, coveredCount int) {
	mode := effectiveMode(c, s.mode)
	var key string
	if mode == markerFull {
		spanJSON, err := json.Marshal(span)
		if err != nil {
			return
		}
		key = hashKey(string(spanJSON))
		if !store.PutStash(st, key, spanJSON) {
			// No stash means the summary would be an unrecoverable drop. Refuse the whole
			// checkpoint rather than ship a marker that resolves to nothing.
			return
		}
	}
	cp := sumCheckpoint{
		SummaryMsg:   summaryWrapper(summary, key, mode),
		CoveredCount: coveredCount,
		CoveredHash:  spanHash(span),
		Key:          key,
	}
	if b, err := json.Marshal(cp); err == nil {
		st.Put(store.SumPrefix+session, b)
	}
	atomic.AddInt64(&summarizeAsyncCommitted, 1)
}

// Counters for the async path, exported through /stats beside the existing summarize ones.
//
// asyncStarted vs asyncCommitted is the pair that matters: a growing gap means calls are being
// started and not landing, which is invisible from the request side now that nothing waits for
// them. summarizeWaitedMs and summarizeWaitTimeouts are what say whether the wait cap is set
// anywhere near right — a timeout count that climbs means turns are paying the full cap and
// getting nothing for it.
var (
	summarizeAsyncStarted   int64
	summarizeAsyncCommitted int64
	summarizeWaitedMs       int64
	summarizeWaitTimeouts   int64
)

// AsyncSummaryStats reports the async path's counters for /stats.
func AsyncSummaryStats() (started, committed, waitedMs, waitTimeouts int64) {
	return atomic.LoadInt64(&summarizeAsyncStarted),
		atomic.LoadInt64(&summarizeAsyncCommitted),
		atomic.LoadInt64(&summarizeWaitedMs),
		atomic.LoadInt64(&summarizeWaitTimeouts)
}
