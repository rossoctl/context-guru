package lifecycle

import (
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/adjudicate"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/thread"
)

// Pre is the input of Prepare: the facts of one request and the steps only the host can do.
// Every hook may be nil; a nil hook is skipped.
type Pre struct {
	Wire     string
	Provider bschemas.ModelProvider
	Mode     components.Mode
	// Bypassed is the request's final bypass flag (the header, or CompactionGate's size bypass).
	Bypassed bool
	Pipe     *components.Pipeline
	// Store is the tenant's store. Expand injection asks it whether it persists. It is read inside
	// Prepare, where a panic fails open.
	Store interface{ Persists() bool }
	// InjectExpand is the expand injection mode ("" means expand.InjectAuto).
	InjectExpand string
	// Orig is the request as the agent sent it. Prepare forwards it when a step panics.
	Orig []byte
	// MakeThreadOf builds the thread resolver of the request (thread.Tracker.ResolveSession bound to
	// it). Prepare calls it after the repair and anchor steps and just before the pipeline, and passes
	// the result to Apply. Prepare calls the resolver again when apply returned before it resolved
	// the thread. The result is in Prepared.ThreadOf.
	MakeThreadOf func() func(session string) thread.Result

	// RepairExpand puts back the content a client's own error answer to our expand tool replaced.
	// It runs first, only when the host rewrites the request. It returns the body and the number of
	// repairs.
	RepairExpand func(body []byte) ([]byte, int)
	// Apply runs the pipeline: the host's observe-versus-enforce choice. It gets the body and the
	// thread resolver, and gives back the body, the time to charge to the request and the trace.
	Apply func(body []byte, threadOf func(session string) thread.Result) ([]byte, time.Duration, apply.Trace)
	// Applied runs right after the pipeline, before the thread is resolved. The host notes the
	// latency and the trace here.
	Applied func(tr apply.Trace, added time.Duration)
	// Observed runs once the thread is known, and before the keep-alive arrival. The host notes the
	// thread and the savings record here.
	Observed func(th thread.Result, tr apply.Trace, added time.Duration)
	// Arrive tells the keep-alive and the predictor that a real request arrived on this thread.
	Arrive func(session, threadID string)
	// Settled runs after the stale keep-alive candidate is cleared.
	Settled func(tr apply.Trace, added time.Duration)
	// ToolDefer runs last, on the final tools array, only when the host rewrites the request. It
	// gives back the body to forward.
	ToolDefer func(tr *apply.Trace, th thread.Result, body []byte) []byte
	// Done runs at the end with the body that goes upstream. The host closes the savings record.
	Done func(th thread.Result, body []byte, tr apply.Trace)
	// OnRepair and OnStray report counts of repaired and answered calls, for the host's log.
	OnRepair func(n int)
	OnStray  func(n int)
}

// Prepared is what Prepare gives back. It is filled in step by step: when a step panics, the
// fields set before it stay set, as the host's own hoisted variables did.
type Prepared struct {
	// Body is the body to forward. On a panic it is Pre.Orig.
	Body []byte
	// PreBody is the body the pipeline received, before any component ran.
	PreBody []byte
	// Anchor is where in-band expands are restored from the next turn on. nil when fixed restore is off.
	Anchor *expand.Anchor
	Trace  apply.Trace
	Added  time.Duration
	// ThreadID is the thread of the request. "" when the thread was not resolved yet.
	ThreadID string
	// ThreadOf is the resolver MakeThreadOf built. nil when a step before it panicked.
	ThreadOf func(session string) thread.Result
	// Panic is the recovered value, nil when no step panicked.
	Panic any
}

// Prepare runs every step before the request goes upstream, in the one order that is right:
//
//  1. expand repair, then the answer to a stray call to our adjudication tool;
//  2. the fixed-restore anchor;
//  3. the pipeline (Pre.Apply), with the keep-alive start time taken just before it;
//  4. the thread (from apply, else resolved here);
//  5. Pre.Observed, then Pre.Arrive, then the clear of the stale keep-alive candidate;
//  6. the expand injection, then the adjudication injection, then tool deferral.
//
// Steps 1, 2 and 6 run only when the host rewrites the request (not in observe mode, not bypassed).
// Prepare fails open: a panic in any step, hooks included, gives back Pre.Orig.
func Prepare(p Pre) *Prepared {
	out := &Prepared{Body: p.Orig}
	body := p.Orig
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				out.Panic = rec
				out.Body = p.Orig
			}
		}()
		enforce := Enforcing(p.Mode, p.Bypassed)
		if enforce {
			if p.RepairExpand != nil {
				var n int
				body, n = p.RepairExpand(body)
				if n > 0 && p.OnRepair != nil {
					p.OnRepair(n)
				}
			}
			var strays int
			if body, strays = adjudicate.AnswerStrayCalls(p.Wire, body); strays > 0 && p.OnStray != nil {
				p.OnStray(strays)
			}
		}
		if enforce && p.Pipe.FixedRestore() {
			if at, fp, ok := expand.AnchorPoint(p.Wire, body); ok {
				out.Anchor = &expand.Anchor{At: at, Fp: fp}
			}
		}
		// Taken BEFORE the pipeline: the clear below must spare a candidate this request's own run
		// wrote. See offload.ClearStaleKeepAliveCandidate.
		startedAt := time.Now()
		var threadOf func(session string) thread.Result
		if p.MakeThreadOf != nil {
			threadOf = p.MakeThreadOf()
		}
		out.ThreadOf = threadOf
		out.PreBody = body
		out.Body = body
		var tr apply.Trace
		var added time.Duration
		body, added, tr = p.Apply(body, threadOf)
		out.Body, out.Added, out.Trace = body, added, tr
		if p.Applied != nil {
			p.Applied(tr, added)
		}
		th := tr.Thread
		if !tr.ThreadResolved {
			// apply returned before it resolved the thread (no message list, observe mode, bypass).
			if threadOf != nil {
				th = threadOf(tr.Session)
			} else {
				th = thread.Result{ID: thread.Primary, Source: thread.SourceSession}
			}
		}
		out.ThreadID = th.ID
		if p.Observed != nil {
			p.Observed(th, tr, added)
		}
		if p.Arrive != nil {
			p.Arrive(tr.Session, th.ID)
		}
		offload.ClearStaleKeepAliveCandidate(thread.Key(tr.Session, th.ID), startedAt)
		if p.Settled != nil {
			p.Settled(tr, added)
		}
		if enforce {
			body = Inject(p.Wire, p.Provider, p.Pipe, p.InjectExpand, p.Store.Persists(), body)
			if p.ToolDefer != nil {
				body = p.ToolDefer(&tr, th, body)
				out.Trace = tr
			}
		}
		out.Body = body
		if p.Done != nil {
			p.Done(th, body, tr)
		}
	}()
	return out
}

// Inject advertises the proxy's own tools on a request the host rewrites: the expand tool, then
// the adjudication tool. Both are sticky and appended last, so the provider's cached prefix
// stays warm. Under expand.InjectAuto the expand tool goes in only when the pipeline can mint a
// marker (Pipeline.HasOffload). The adjudication tool goes in only for a pipeline with
// extract_llm_sweep, on a wire whose prefixed ask has an adapter.
func Inject(wire string, provider bschemas.ModelProvider, pipe *components.Pipeline, injectExpand string, storePersists bool, body []byte) []byte {
	im := injectExpand
	if im == "" {
		im = expand.InjectAuto
	}
	if im != expand.InjectAuto || pipe.HasOffload() {
		body, _ = expand.Inject(wire, im, body, storePersists)
	}
	if (provider == bschemas.Anthropic || schema.IsResponses(wire) || wire == schema.WireOpenAI) && pipe.Has("extract_llm_sweep") {
		body, _ = adjudicate.Inject(wire, body)
	}
	return body
}
