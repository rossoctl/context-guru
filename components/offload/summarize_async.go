package offload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/cheapmodel"
	"github.com/rossoctl/context-guru/internal/logging"
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

// maxConcurrentSummaries bounds detached summarizer calls ACROSS sessions.
//
// Single-flight is per session, which is the right unit for correctness — two summaries over one
// transcript race to write the same checkpoint — but it is not a ceiling. The inline path was
// self-limiting in a way that is easy to miss: a call occupied a request, so it was bounded by the
// server's own concurrency, and it showed up in that request's latency and row. Detached, a burst
// of sessions returning from idle can each commission a ~57k-token call at the same moment, with no
// bound, no visible latency, and no row anywhere until each session's next turn.
//
// 8 is chosen to be small. This is not a throughput knob: a summary is a cost-saving measure, so a
// proxy under enough load to saturate it should decline to compact rather than queue work nobody is
// waiting for. Saturation is therefore a REFUSAL (counted as a gate), never a wait.
const maxConcurrentSummaries = 8

// summarySlots is the global bound. Buffered channel rather than a semaphore type because the
// non-blocking acquire is the whole point — see startAsyncSummary.
var summarySlots = make(chan struct{}, maxConcurrentSummaries)

// summarizeUnresolved is how many detached calls are outstanding RIGHT NOW.
//
// It says nothing about a restart, and an earlier version of this comment claimed it did — which it
// cannot: it lives in process memory and dies with the process, exactly like the
// started/committed pair whose gap it was supposed to explain. What survives a restart is the LOG,
// which is why commissioning and resolution each emit a line (see logCommission). An unmatched pair
// in the log is a call that was paid for and lost, and it is queryable after the fact.
//
// Nothing drains on shutdown, and that is deliberate rather than unfinished: a shutdown hook that
// waits on in-flight summaries adds a path that can hang the process for a cost-saving measure
// nobody is awaiting. The log makes the loss visible without that risk.
var summarizeUnresolved int64

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
func (f *summaryFlight) waitFor(ctx context.Context, session string, limit time.Duration) (waited time.Duration, landed bool) {
	j, ok := f.peek(session)
	if !ok {
		return 0, false
	}
	if limit <= 0 {
		limit = defaultSummaryWait
	}
	t := time.NewTimer(limit)
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
// It returns the REASON it declined, or "" when the call was started. Two refusals, two remedies: a
// session already summarizing is ordinary and self-correcting, while the global bound being full
// means the proxy is shedding compaction under load. One gate name for both would report a busy
// deployment as a busy session.
func (s *Summarize) startAsyncSummary(c *components.Ctx, model components.Model,
	span []bschemas.ChatMessage, goal string, coveredCount int) string {
	j, ok := inFlight.begin(c.Session)
	if !ok {
		return "summary_already_in_flight"
	}
	// The global bound, acquired WITHOUT blocking: a saturated proxy declines to compact rather
	// than queueing a call nobody is waiting for. Releasing the per-session flight first, so a
	// refusal here does not leave the session marked busy.
	select {
	case summarySlots <- struct{}{}:
	default:
		inFlight.finish(c.Session, j)
		atomic.AddInt64(&summarizeAsyncRefused, 1)
		return "summary_concurrency_full"
	}
	atomic.AddInt64(&summarizeAsyncStarted, 1)
	atomic.AddInt64(&summarizeUnresolved, 1)
	// Logged, because the counters cannot survive a restart and this is the only record that can.
	// A commission with no matching resolution line is a call that was paid for and lost.
	logging.From(c.Ctx).Info("summarize: commissioned a detached summary",
		"session", c.Session, "covered_messages", coveredCount)
	spanCopy := make([]bschemas.ChatMessage, len(span))
	copy(spanCopy, span)
	// Everything the goroutine needs, read HERE while we are still on the request's goroutine.
	//
	// `st`, not `store`: the obvious name shadows the store PACKAGE, which this goroutine also
	// calls into (store.PutStash, store.SumPrefix). It compiled, which is what makes it worth
	// renaming rather than leaving.
	// `mode` is read HERE too, and that is the point of this line existing separately.
	//
	// commitAsyncSummary used to take the live `*Ctx` and call effectiveMode(c, s.mode) from inside
	// the goroutine, which reads c.Store — contradicting the discipline stated two lines above and
	// putting a concurrent read on a struct the request owns. No production write to a Ctx field
	// exists today (a Ctx is per-request and never pooled), so it was latent rather than a live
	// race; the race detector trips on it, and it is exactly the shape where the NEXT field write
	// becomes a silent production race. A review found it.
	session, st, mode := c.Session, c.Store, effectiveMode(c, s.mode)
	// Detached from the request's cancellation but keeping its logger, so the resolution line
	// carries the same request-scoped fields the commission line did.
	baseCtx := context.WithoutCancel(c.Ctx)
	go func() {
		defer inFlight.finish(session, j)
		defer func() {
			atomic.AddInt64(&summarizeUnresolved, -1)
			<-summarySlots
			// The matching half of the commission line. Emitted from a defer so it fires on every
			// exit — success, model error, timeout, or a recovered panic — because an unmatched
			// commission is exactly the signal being preserved and a missed line would fake one.
			logging.From(baseCtx).Info("summarize: detached summary resolved", "session", session)
		}()
		// Fail open, always: a panic in a detached goroutine takes the process down, which is a
		// far worse outcome than a missing summary. The pipeline's own recover cannot reach here.
		//
		// AND IT IS NOT SILENT. `_ = recover()` swallowed the only evidence that anything had gone
		// wrong: nothing on the hot path waits for this call, so a panicking summarizer produced a
		// growing asyncStarted/asyncCommitted gap and no other trace at all — a component failing
		// invisibly, which is the shape this repo has been bitten by before. A counter so it shows
		// up in /stats beside the other async figures, and a log line with the value so the cause
		// is diagnosable rather than merely countable.
		defer func() {
			if r := recover(); r != nil {
				atomic.AddInt64(&summarizeAsyncPanics, 1)
				logging.From(baseCtx).Error("summarize: detached summary panicked; no summary was "+
					"committed for this turn", "session", session, "panic", fmt.Sprint(r))
			}
		}()
		ctx, cancel := context.WithTimeout(baseCtx, asyncSummaryBudget())
		defer cancel()
		// A DETACHED sink, not a nested one. This goroutine inherits the commissioning request's
		// context, so a chained sink would also reach THAT request's row — and when the call
		// finishes before the row is written, the cost lands there as well as being replayed onto
		// the next turn, charging one call twice. Measured at exactly 2x. Detaching makes the
		// attribution single-valued; process totals are counted by the model wrapper regardless.
		ctx, callSink := cheapmodel.WithDetachedSink(ctx)
		summary, err := s.summarize(ctx, model, spanCopy, goal)
		// Deferred BEFORE the error check: a call that timed out or failed may still have been
		// billed for its input, and a cost we incurred is a cost we report.
		deferUsage(st, session, callSink)
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
		s.commitAsyncSummary(mode, session, st, spanCopy, summary, coveredCount)
	}()
	return ""
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
func (s *Summarize) commitAsyncSummary(mode markerMode, session string, st store.Store,
	span []bschemas.ChatMessage, summary string, coveredCount int) {
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
	// summarizeAsyncRefused counts commissions the GLOBAL bound turned away — distinct from the
	// per-session single-flight refusal, which is ordinary and expected on a busy session. A
	// climbing value here means the proxy is shedding compaction under load, which is the designed
	// behaviour but worth seeing.
	summarizeAsyncRefused int64
	// summarizeAsyncPanics counts recovered panics in the detached goroutine. It exists because the
	// recover() was silent: nothing on the hot path waits for this call, so a panicking summarizer
	// showed up ONLY as a growing started/committed gap and no other trace anywhere. Fail-open is
	// right; failing open without a countable trace is not.
	summarizeAsyncPanics int64
)

// MaxConcurrentSummaries is the global bound, reported beside the counts for the reason
// llm_call_timeout_ms travels with its timeout total: a refusal count is meaningless without the
// ceiling it was measured against.
func MaxConcurrentSummaries() int64 { return maxConcurrentSummaries }

// AsyncSummaryStats reports the async path's counters for /stats.
func AsyncSummaryStats() (started, committed, waitedMs, waitTimeouts, refused, unresolved, panics int64) {
	return atomic.LoadInt64(&summarizeAsyncStarted),
		atomic.LoadInt64(&summarizeAsyncCommitted),
		atomic.LoadInt64(&summarizeWaitedMs),
		atomic.LoadInt64(&summarizeWaitTimeouts),
		atomic.LoadInt64(&summarizeAsyncRefused),
		atomic.LoadInt64(&summarizeUnresolved),
		atomic.LoadInt64(&summarizeAsyncPanics)
}

// deferredUsage is what a detached summarizer call used, waiting for a turn to attribute it to.
type deferredUsage struct {
	Model      string `json:"model"`
	In         int    `json:"in"`
	Out        int    `json:"out"`
	CacheWrite int    `json:"cache_write"`
	CacheRead  int    `json:"cache_read"`
}

// deferUsage records what a detached call used, for this session's next turn to attribute.
//
// Additive rather than replacing: two calls can complete between one turn and the next (a
// commission and a roll-forward), and the second must not erase the first's cost. The whole point
// of this record is that a cost we incurred is a cost we report.
// deferredUsageMu serializes the read-modify-write on a session's deferred-usage record.
//
// deferUsage does Get -> add -> Put on a background goroutine while takeDeferredUsage does
// Get -> clear -> replay on a request. Without a lock, a call finishing between the take's Get and
// its clear has its record erased UNREAD: the cost silently vanishes, which is the one direction the
// episode panel must not lean, and it is the same hazard deferUsage's additive read guards against
// from the other side. store.Store offers no atomic read-and-clear, so the mutex is the mechanism.
var deferredUsageMu sync.Mutex

func deferUsage(st store.Store, session string, sink *cheapmodel.Sink) {
	if st == nil || session == "" || sink == nil {
		return
	}
	deferredUsageMu.Lock()
	defer deferredUsageMu.Unlock()
	calls, in, out := sink.Totals()
	cw, cr := sink.CacheTotals()
	if calls == 0 || (in == 0 && out == 0 && cw == 0 && cr == 0) {
		return
	}
	u := deferredUsage{Model: sink.Model(), In: int(in), Out: int(out),
		CacheWrite: int(cw), CacheRead: int(cr)}
	if b, ok := st.Get(store.UsagePrefix + session); ok && len(b) > 0 {
		var prev deferredUsage
		if json.Unmarshal(b, &prev) == nil {
			u.In += prev.In
			u.Out += prev.Out
			u.CacheWrite += prev.CacheWrite
			u.CacheRead += prev.CacheRead
			if u.Model == "" {
				u.Model = prev.Model
			}
		}
	}
	if b, err := json.Marshal(u); err == nil {
		st.Put(store.UsagePrefix+session, b)
	}
}

// takeDeferredUsage attributes any pending detached-call usage to THIS request, and clears it.
//
// Read-then-delete-then-replay, in that order: replaying without deleting would charge the same
// call to every later turn, which is a worse error than the missing cost it fixes.
func takeDeferredUsage(c *components.Ctx) {
	if c == nil || c.Store == nil || c.Session == "" {
		return
	}
	deferredUsageMu.Lock()
	key := store.UsagePrefix + c.Session
	b, ok := c.Store.Get(key)
	if !ok || len(b) == 0 {
		deferredUsageMu.Unlock()
		return
	}
	// CLEARED BEFORE REPLAYING, not after: replaying without clearing charges the same call to
	// every later turn of the session, which is a worse error than the missing cost it fixes.
	// Overwritten rather than deleted because store.Store has no Delete — an empty record reads
	// back as all-zero and is refused by the guard below.
	c.Store.Put(key, []byte("{}"))
	// Unlocked as soon as the record is CLEARED: the replay below touches only this request's sink,
	// so holding the lock across it would serialize unrelated requests for nothing.
	deferredUsageMu.Unlock()
	var u deferredUsage
	if json.Unmarshal(b, &u) != nil {
		return
	}
	if u.In == 0 && u.Out == 0 && u.CacheWrite == 0 && u.CacheRead == 0 {
		// An already-cleared record. Returning here matters: ReplayUsage would otherwise
		// increment the sink's CALL count for a call that is being counted a second time.
		return
	}
	cheapmodel.ReplayUsage(c.Ctx, u.Model, u.In, u.Out, u.CacheWrite, u.CacheRead)
}
