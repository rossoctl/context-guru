package components

import (
	"fmt"

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
	rr := &RunReport{Session: c.Session, TokensBefore: schema.MessagesTokens(req), Mode: c.effMode(), Deferred: c != nil && c.Deferred}
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
	rep = Report{Component: comp.Name(), Mode: c.effMode(), Deferred: c != nil && c.Deferred}
	before := schema.CloneMessages(req.Input)
	rep.TokensBefore = tokensOf(before)
	start := clock()

	defer func() {
		rep.DurationMs = float64(clock().Sub(start).Microseconds()) / 1000.0
		if r := recover(); r != nil {
			// Fail open: revert and record. A component panic never breaks the request.
			req.Input = before
			rep.Reverted = true
			rep.TokensAfter = rep.TokensBefore
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
		rep.TokensAfter = rep.TokensBefore
		rep.Err = err
	case rep.Kind == "offload" && after < rep.TokensBefore && len(rep.CacheKeys) == 0 && !rep.Skipped && !rep.Irreversible:
		// An Offload that dropped content without stashing an original is a
		// contract violation — reversibility would be broken. Revert. (A
		// deliberate lossy drop under marker_mode summary/off sets rep.Irreversible
		// and is exempt: it chose no restoration, not forgot it.)
		req.Input = before
		rep.Reverted = true
		rep.TokensAfter = rep.TokensBefore
		rep.Err = fmt.Errorf("offload dropped content without stashing a cache_key")
	case after > rep.TokensBefore:
		// never-worse: a component must not grow the request.
		req.Input = before
		rep.Reverted = true
		rep.TokensAfter = rep.TokensBefore
	default:
		rep.TokensAfter = after
	}
	return rep
}

func tokensOf(msgs []schemas.ChatMessage) int {
	return schema.MessagesTokens(&schemas.BifrostChatRequest{Input: msgs})
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
