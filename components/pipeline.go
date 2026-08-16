package components

import (
	"fmt"
	"reflect"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/schema"
)

// Pipeline runs an ordered list of components over a request. Order is set by
// config (the pipeline: name-list); this type just executes it. Each component
// is isolated: a panic or error reverts only that component, and a component
// that fails to shrink the request is reverted too (never-worse guard, after
// rtk). The original request is always a valid fallback — fail open, always.
type Pipeline struct {
	comps   []Component
	emitter Emitter
}

// NewPipeline builds a pipeline from already-constructed components in order.
func NewPipeline(comps []Component, e Emitter) *Pipeline {
	if e == nil {
		e = NopEmitter{}
	}
	return &Pipeline{comps: comps, emitter: e}
}

// Run applies every enabled component to req in place and returns the aggregate
// report. req is mutated; on any per-component failure that component's changes
// are rolled back, so the returned request is never worse than the input.
func (p *Pipeline) Run(req *schemas.BifrostChatRequest, c *Ctx) *RunReport {
	rr := &RunReport{Session: c.Session, TokensBefore: schema.MessagesTokens(req), Mode: c.effMode()}
	// Hand cmdfilter its per-family ledger sink when the emitter implements one, so no
	// host has to thread a second field through every Ctx construction site.
	if c.FilterStats == nil {
		if s, ok := p.emitter.(FilterStatsSink); ok {
			c.FilterStats = s
		}
	}
	if c.Bypass {
		rr.TokensAfter = rr.TokensBefore
		return rr
	}
	for _, comp := range p.comps {
		if !comp.Enabled(c) {
			continue
		}
		rep := p.runOne(comp, req, c)
		rr.Components = append(rr.Components, rep)
		rr.DurationMs += rep.DurationMs
		safeEmit(func() { p.emitter.Component(rep) })
	}
	rr.TokensAfter = schema.MessagesTokens(req)
	safeEmit(func() { p.emitter.Run(*rr) })
	return rr
}

// safeEmit runs an emitter callback under recover: metrics/observability must never
// break a request. A panicking emitter is swallowed (fail-open) rather than propagating
// out of Run, where — unlike component code — no other recover would catch it.
func safeEmit(fn func()) {
	defer func() { _ = recover() }()
	fn()
}

// runOne executes a single component with snapshot/restore isolation and the
// never-worse guard. It never returns an error — failures are recorded on the
// Report and the request is reverted.
func (p *Pipeline) runOne(comp Component, req *schemas.BifrostChatRequest, c *Ctx) (rep Report) {
	rep = Report{Component: comp.Name(), Mode: c.effMode()}
	before := schema.CloneMessages(req.Input)
	// baseline is the guard's OWN copy of the pre-run size. rep.TokensBefore is handed to
	// the component, so a component can write to it — mask and failed_run both did
	// (`rep.TokensBefore += saved`) — and comparing against the field let a component move
	// the goalpost and splice a request it had GROWN onto the wire. The never-worse
	// guarantee is the product's central safety claim, so it may not depend on component
	// cooperation. See TestNeverWorseGuardIgnoresAComponentInflatingItsOwnBaseline.
	baseline := tokensOf(before)
	rep.TokensBefore = baseline
	start := clock()

	defer func() {
		rep.DurationMs = float64(clock().Sub(start).Microseconds()) / 1000.0
		if r := recover(); r != nil {
			// Fail open: revert and record. A component panic never breaks the request.
			req.Input = before
			rep.Reverted = true
			rep.TokensBefore = baseline
			rep.TokensAfter = baseline
			rep.Err = fmt.Errorf("panic: %v", r)
		}
	}()

	var err error
	switch t := comp.(type) {
	case Reformat:
		rep.Kind = "reformat"
		err = t.Reformat(req, &rep, c)
	case Offload:
		rep.Kind = "offload"
		var keys []string
		keys, err = t.Offload(req, &rep, c)
		rep.CacheKeys = keys
	default:
		// Registered but implements neither interface — skip, don't fail the run.
		rep.Skipped = true
		rep.TokensAfter = rep.TokensBefore
		return rep
	}

	after := tokensOf(req.Input)
	switch {
	case err != nil:
		req.Input = before
		rep.Reverted = true
		rep.TokensBefore, rep.TokensAfter = baseline, baseline
		rep.Err = err
	case rep.Kind == "offload" && after < baseline && len(rep.CacheKeys) == 0 && !rep.Skipped && !rep.Irreversible:
		// An Offload that dropped content without stashing an original is a
		// contract violation — reversibility would be broken. Revert. (A
		// deliberate lossy drop under marker_mode summary/off sets rep.Irreversible
		// and is exempt: it chose no restoration, not forgot it.)
		req.Input = before
		rep.Reverted = true
		rep.TokensBefore, rep.TokensAfter = baseline, baseline
		rep.Err = fmt.Errorf("offload dropped content without stashing a cache_key")
	case after > baseline:
		// never-worse: a component must not grow the request.
		req.Input = before
		rep.Reverted = true
		rep.TokensBefore, rep.TokensAfter = baseline, baseline
	default:
		rep.TokensBefore, rep.TokensAfter = baseline, after
		// Only a change that SURVIVED can be discarded by the writeback layer. Recording
		// it in the revert branches above would charge a rolled-back component for a
		// discard it never caused.
		rep.ChangedIdx = changedIdx(before, req.Input)
	}
	return rep
}

func tokensOf(msgs []schemas.ChatMessage) int {
	return schema.MessagesTokens(&schemas.BifrostChatRequest{Input: msgs})
}

// changedIdx returns the indices at which a component's output differs from its
// input, so the writeback layer can attribute a discarded change to the component
// that made it. Count changes (summarize) yield nil — those go down the rebuild path,
// which never discards per-message.
//
// reflect.DeepEqual, not a marshal-and-compare: this runs per component per request
// purely for a diagnostic, so it must stay cheap. Measured on a realistic 80-message
// request, marshalling both sides cost 20.52 ms/op vs 16.56 ms (−19.3%) and 3,206 extra
// allocs. Struct equality is the same decision here — the writeback loop's own marshal
// is what actually decides whether to splice.
func changedIdx(before, after []schemas.ChatMessage) []int {
	if len(before) != len(after) {
		return nil
	}
	var out []int
	for i := range after {
		if !reflect.DeepEqual(before[i], after[i]) {
			out = append(out, i)
		}
	}
	return out
}

// RecordDiscards emits one follow-up Report per component whose changes the
// writeback layer threw away, so a silently-suppressed component is visible in
// telemetry instead of looking like a working one. discarded maps req.Input index ->
// number of discarded changes at that index; hosts call this after the splice.
//
// One discarded message is charged to exactly ONE component: the LAST one that
// changed that index. Several components can touch the same message, but the
// writeback layer discards the final cumulative state, so that component's change is
// the one actually thrown away — charging every earlier toucher too would make this
// counter a false-positive generator, and its whole point is to be trustworthy enough
// to catch a #32-class bug.
func (p *Pipeline) RecordDiscards(rr *RunReport, discarded map[int]int) {
	if p == nil || rr == nil || len(discarded) == 0 {
		return
	}
	// owner[i] = index into rr.Components of the last component that changed message i.
	owner := map[int]int{}
	for c := range rr.Components {
		for _, i := range rr.Components[c].ChangedIdx {
			owner[i] = c
		}
	}
	counts := map[int]int{} // component index -> discards charged
	for i, n := range discarded {
		if c, ok := owner[i]; ok {
			counts[c] += n
		}
	}
	for c, n := range counts {
		d := Report{Component: rr.Components[c].Component, Kind: rr.Components[c].Kind, Discarded: n}
		safeEmit(func() { p.emitter.Component(d) })
	}
}

// Has reports whether a component with this name is configured in the pipeline.
// Hosts use it to gate body-level work that belongs to a component's concern but
// cannot be done inside it — e.g. cacheinject's cache-prefix repair, which must
// touch the top-level `system` array that components never see.
func (p *Pipeline) Has(name string) bool {
	if p == nil {
		return false
	}
	for _, c := range p.comps {
		if c != nil && c.Name() == name {
			return true
		}
	}
	return false
}
