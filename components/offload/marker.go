package offload

import (
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
)

// markerMode selects what an Offload component leaves behind in place of the
// content it drops. It is per-component config (marker_mode), defaulting to full.
//
//   - full    (default): stash the original in the Store and leave a resolvable
//     <<cg:HASH>> marker, so the expand tool can restore it. Fully reversible.
//   - summary: leave a non-resolvable ⟪cg⟫ sentinel next to the component's own
//     human note. Nothing is stashed; there is no restoration. The note is the
//     "short summary of what was compacted."
//   - off:     leave no marker at all — just the reduced content / note. No stash,
//     no restoration, no sentinel.
//
// summary/off are deliberate lossy drops, so mark records rep.Irreversible to
// exempt them from the pipeline's "dropped content without stashing → revert"
// guard (which still catches a component that forgot to stash in full mode).
type markerMode int

const (
	markerFull markerMode = iota
	markerSummary
	markerOff
)

// parseMarkerMode maps the yaml value to a mode; unknown/empty → full (so
// existing configs keep their reversible behavior).
func parseMarkerMode(s string) markerMode {
	switch s {
	case "summary":
		return markerSummary
	case "off":
		return markerOff
	default:
		return markerFull
	}
}

// effectiveMode degrades a full (reversible) marker to off when the store cannot
// persist the stash (store disabled). Without this, a full marker would leave an
// unresolvable <<cg:HASH>> in the request and silently lose the dropped content.
// Every Offload that honors marker_mode routes its mode through this first.
func effectiveMode(c *components.Ctx, mode markerMode) markerMode {
	if mode == markerFull && !c.Store.Persists() {
		return markerOff
	}
	return mode
}

// markToken computes the marker token and store key for a mode WITHOUT stashing.
// It returns the effective mode (full degrades to off when the store can't persist).
// This is the "plan" half of the split that lets a component build its candidate
// rewrite and size-check it (marker included) before committing any side effect.
func markToken(c *components.Ctx, mode markerMode, original, hint string) (token, key string, eff markerMode) {
	eff = effectiveMode(c, mode)
	switch eff {
	case markerFull:
		key = hashKey(original)
		return expand.Marker(key) + hint, key, eff
	case markerSummary:
		return expand.SummaryMarker, "", eff
	default: // off
		return "", "", eff
	}
}

// tryMark builds the candidate replacement text an Offload wants to write —
// assemble(token), where the component's layout closure places the marker token —
// and reports whether that text is strictly smaller than the original, MARKER
// INCLUDED. It performs no side effects (no stash), so a caller that gets ok=false
// leaves the message verbatim. This is the shared marker-inclusive never-worse guard
// (the aggregate pipeline guard is per-request, not per-message, so without this a
// single small output could grow by the marker's tokens while the request still net-shrinks).
func tryMark(c *components.Ctx, mode markerMode, original, hint string, assemble func(token string) string) (newText, key string, eff markerMode, ok bool) {
	token, key, eff := markToken(c, mode, original, hint)
	newText = assemble(token)
	ok = schema.TextTokens(newText) < schema.TextTokens(original)
	return newText, key, eff, ok
}

// commitMark performs the side effects once a caller accepts a tryMark candidate:
// stash the original under key (full mode) or record the deliberate lossy drop
// (summary/off set rep.Irreversible so the pipeline's "dropped without stashing"
// guard doesn't revert them). Call only when tryMark returned ok.
func commitMark(c *components.Ctx, rep *components.Report, eff markerMode, key, original string) {
	if eff == markerFull {
		c.Store.Put(key, []byte(original))
		recordOwner(c, key) // scope GET /expand retrieval to this session
		return
	}
	rep.Irreversible = true
}
