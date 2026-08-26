package proxy

import (
	"context"
	"strconv"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
)

// Operating modes on the request path (#31).
//
//	sync    — run the pipeline inline and forward its output. Unchanged from before modes
//	          existed, down to the bytes; this is the default.
//	observe — forward the ORIGINAL body, byte for byte, and run the pipeline off-path on a
//	          copy purely to record what it WOULD have saved.
//
// Byte-identity in observe mode is structural, not a property of careful copying: the
// request path never touches the pipeline at all. Fail-open follows from that too — there
// is nothing for a failure to damage, because the forwarded body is the input.

// applyMode rewrites body for forwarding according to the handler's mode, and returns the
// body to forward, the wall time to charge to the request path, and the observational
// trace the dashboard's capture reads. Never returns a nil body.
//
// In observe mode the returned trace is the ZERO value, and deliberately so: the enforced
// path ran nothing, so there is nothing about this request to observe. Reporting the
// off-path projection here would credit a hypothetical saving to a request that was
// forwarded untouched — the exact confusion the potential_* namespace exists to prevent.
func (h *Handler) applyMode(r *reqInfo) ([]byte, time.Duration, apply.Trace) {
	mode := r.tn.Mode
	start := time.Now()

	// Observe: the enforced path does nothing at all. Not "runs and discards" — it never
	// runs, which is what makes the byte-identity guarantee structural. The measurement
	// happens off-path, on a copy, and the request pays only the enqueue.
	if mode == components.ModeObserve && !r.bypassed {
		h.observe(r)
		return r.body, time.Since(start), apply.Trace{}
	}

	res := apply.BodyOpts(r.ctx, r.tn.Pipe, r.tn.Store, apply.Opts{
		Provider: r.provider, Body: r.body, Session: r.session, Tenant: r.tn.ID, Bypass: r.bypassed,
		Models: r.models, Window: r.window, CacheMode: h.opts.CacheMode,
		Mode: mode, Tracker: h.tracker,
		PrefixAsk: h.prefixAskerFor(r.provider, r.models),
	})
	added := time.Since(start)
	if res.Body == nil {
		return r.body, added, res.Trace
	}
	return res.Body, added, res.Trace
}

// reqInfo is the per-request input both the inline pass and an off-path observation need.
// Bundled because an off-path run outlives the *http.Request it came from and must
// therefore hold a copy of everything, never a pointer into request-scoped state.
type reqInfo struct {
	ctx      context.Context
	provider bschemas.ModelProvider
	body     []byte
	session  string
	bypassed bool
	models   components.ModelSpec
	window   int
	// tn is the authenticated caller's tenancy: its pipeline, its state store, its
	// mode. Carried here rather than read off the Handler because in a hosted
	// deployment there is no such thing as "the" pipeline — and because this bundle
	// is already the thing an off-path observation copies, so the store an
	// observation writes to cannot drift from the tenant it belongs to.
	tn *Tenancy
}

func (h *Handler) mode() components.Mode {
	if h.opts.Mode == "" {
		return components.ModeSync
	}
	return h.opts.Mode
}

// observe runs the pipeline off-path on a COPY of the request, against observe's own
// disjoint store, and records the result into the hypothetical metric namespace. Two
// independent reasons the enforced request cannot be affected: it was already forwarded
// from the untouched original, and this run touches no state the live path reads.
func (h *Handler) observe(r *reqInfo) {
	if h.pool == nil {
		return
	}
	// The job runs after the response is written, so nothing may alias request-scoped
	// memory — and the context must be the pool's, not the request's, which is cancelled
	// the moment the handler returns.
	info := *r
	info.body = append([]byte(nil), r.body...)
	// A plain counter as the dedup key: one observation per call, coalescing nothing. Also
	// why observe needs no session resolve on the request path — one more thing the
	// enforced path does not pay for.
	key := "observe:" + strconv.FormatUint(h.observeSeq.Add(1), 10)

	h.pool.Enqueue(key, func(ctx context.Context) {
		apply.BodyOpts(ctx, info.tn.Pipe, info.tn.Shadow, apply.Opts{
			Provider: info.provider, Body: info.body, Session: info.session, Tenant: info.tn.ID,
			Models: info.models, Window: info.window, CacheMode: h.opts.CacheMode,
			Mode: components.ModeObserve,
			// The Tracker, so the projection is measured under the SAME cached-prefix
			// boundary an enforcing mode would use. Without it the boundary is unknown,
			// MaxCachedIdx is -1, the tail gate never fires, and every message in the
			// transcript looks compactable — which inflates the projection against what
			// sync actually achieves. Measured on SWE-bench: 9.5% projected against 0.8%
			// enforced, because 50 extract_llm candidates passed the gate instead of 5.
			//
			// Out-of-order jobs no longer leave the boundary untouched: since the
			// compaction reset (modes.Boundary) a late job carrying a SHORTER turn reads
			// as a compaction, rebases the boundary on its own length, and bumps
			// compaction_resets. That errs toward a LOWER boundary — a bigger tail, so a
			// projection that over- rather than under-states what sync would achieve —
			// and it cannot touch an enforced request, because observe mode's enforced
			// path never calls BodyOpts at all. Read compaction_resets in observe mode as
			// "resets plus off-path reordering", not as a compaction count.
			Tracker: h.tracker,
			// h.shadow, not the live store: see Handler.shadow. The live store must stay
			// clean (a real request must never replay a decision that was never enforced),
			// but the frozen decisions still have to accumulate across turns or the
			// projection under-reports what enforcing would achieve.
		})
		// The pipeline already emitted mode-stamped reports through the emitter; the
		// Aggregator routes anything stamped observe into the potential_* namespace. No
		// separate recording call here, which is what keeps the two namespaces from
		// drifting apart.
	})
}
