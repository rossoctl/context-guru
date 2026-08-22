package components

import (
	"crypto/sha256"
	"encoding/hex"
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
	// ONE snapshot for the whole run, kept in step with req.Input as components land
	// (see runOne). Cloning the entire transcript per component was ~20% of the rewrite
	// path's CPU on real 600 KB Claude Code requests, and all but a handful of its
	// messages were re-cloned unchanged six times over.
	snap := schema.CloneMessages(req.Input)
	for _, comp := range p.comps {
		if !comp.Enabled(c) {
			continue
		}
		rep := p.runOne(comp, req, c, &snap)
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
// snap is the run's live snapshot: on entry it holds an independent deep copy of
// req.Input, and runOne leaves it holding one of req.Input on exit (re-cloning only
// what changed, or the lot after a revert or a count change).
func (p *Pipeline) runOne(comp Component, req *schemas.BifrostChatRequest, c *Ctx, snap *[]schemas.ChatMessage) (rep Report) {
	rep = Report{Component: comp.Name(), Mode: c.effMode()}
	before := *snap
	// revert installs the snapshot and re-establishes an independent one, because
	// req.Input now aliases the copy we were holding.
	revert := func() {
		req.Input = before
		*snap = schema.CloneMessages(before)
	}
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
			revert()
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
		revert()
		rep.Reverted = true
		rep.TokensBefore, rep.TokensAfter = baseline, baseline
		rep.Err = err
	case rep.Kind == "offload" && after < baseline && len(rep.CacheKeys) == 0 && !rep.Skipped && !rep.Irreversible:
		// An Offload that dropped content without stashing an original is a
		// contract violation — reversibility would be broken. Revert. (A
		// deliberate lossy drop under marker_mode summary/off sets rep.Irreversible
		// and is exempt: it chose no restoration, not forgot it.)
		revert()
		rep.Reverted = true
		rep.TokensBefore, rep.TokensAfter = baseline, baseline
		rep.Err = fmt.Errorf("offload dropped content without stashing a cache_key")
	case after > baseline:
		// never-worse: a component must not grow the request.
		revert()
		rep.Reverted = true
		rep.TokensBefore, rep.TokensAfter = baseline, baseline
	default:
		rep.TokensBefore, rep.TokensAfter = baseline, after
		// Only a change that SURVIVED can be discarded by the writeback layer. Recording
		// it in the revert branches above would charge a rolled-back component for a
		// discard it never caused.
		rep.ChangedIdx = changedIdx(before, req.Input)
		if rep.Kind == "reformat" {
			rep.CacheKeys = reformatKeys(before, rep.ChangedIdx)
		}
		resync(snap, req.Input, rep.ChangedIdx)
	}
	return rep
}

// reformatKeys gives a Reformat the content-derived dedup keys an Offload gets for free
// from its stashes: one per message it rewrote, hashed over that message's text BEFORE
// the fold.
//
// Without them every Reformat run took metrics' keyless fallback (`SavedUnique += saved`,
// metrics/metrics.go), so SavedUnique was *defined* equal to Saved and
// overcount_ratio came out 1.0 by construction for format/textclean/toon/searchfold —
// read as "every token these remove is new money" when the truth is the opposite: an
// idempotent in-place fold re-folds the SAME message on every later turn, so its saving
// is re-counted per turn. Measured replay on the captured corpus: format 10.17x,
// textclean 95.29x. Only the first removal is new money, exactly as for an Offload.
//
// Keys are hashed over `before`, not the rewritten text, so the same tool output re-sent
// next turn maps to the same key however the fold re-renders it — and they are
// namespaced away from the store's stash keys because nothing is stashed here: a
// Reformat is lossless, these exist only to be counted.
func reformatKeys(before []schemas.ChatMessage, changed []int) []string {
	if len(changed) == 0 {
		return nil
	}
	keys := make([]string, 0, len(changed))
	for _, i := range changed {
		if i < 0 || i >= len(before) {
			continue
		}
		sum := sha256.Sum256([]byte(schema.MessageText(before[i])))
		keys = append(keys, "cg:fold:"+hex.EncodeToString(sum[:]))
	}
	return keys
}

// resync brings the run's snapshot back in step with a component's surviving output,
// re-cloning only the messages that changed. A changed COUNT (summarize) is the one case
// the index list cannot express — changedIdx returns nil for it, exactly as it does for
// "nothing changed" — so the length decides, and only that case re-clones the lot.
//
// Every snapshot entry stays exactly ONE clone of some live message — a message touched
// by several components is re-cloned from that component's own output each time — so
// this is as faithful as the per-component full clone it replaces, not a clone of a clone.
func resync(snap *[]schemas.ChatMessage, live []schemas.ChatMessage, changed []int) {
	if len(*snap) != len(live) {
		*snap = schema.CloneMessages(live)
		return
	}
	for _, i := range changed {
		(*snap)[i] = schema.CloneMessages(live[i : i+1])[0]
	}
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

// Find returns the configured component with this name, or nil. It is Has for the case
// where the host needs the component's CONFIGURATION and not just its presence — toolfilter
// carries the account's removal list, and a body-level transform in apply cannot read it off
// a boolean. Callers type-assert to the narrow interface they need.
func (p *Pipeline) Find(name string) Component {
	if p == nil {
		return nil
	}
	for _, c := range p.comps {
		if c != nil && c.Name() == name {
			return c
		}
	}
	return nil
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
