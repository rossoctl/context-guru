package offload

import (
	"sync/atomic"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
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
//
// It reports whether the removal MAY PROCEED, and a caller that ignores the answer
// reintroduces #187. In full mode the marker about to be written is a promise that the
// original can be produced on request, so the stash write is the point at which that promise
// is either backed or not: when the store's rewind reserve cannot take the payload
// (store.PutStash false) there is nothing to serve, and stamping the marker anyway is how a
// reversible removal silently became an irreversible one — 209 times in iteration 024's arm B,
// where the agent asked for content back and got a placeholder.
//
// false therefore means REFUSE the removal: leave the content verbatim. Not "drop it without a
// marker" (marker_mode: off), which is a lossy drop the operator did not ask for, and not "drop
// it with a marker", which is the bug. Leaving it verbatim costs tokens the pipeline wanted to
// save and keeps the invariant CLAUDE.md states as a hard boundary — every lossy Offload is
// reversible — true regardless of load.
//
// THE CALLER'S HALF OF THE CONTRACT, and it is an invariant rather than a courtesy: NOTHING
// DERIVED FROM THE REMOVAL MAY BE RECORDED BEFORE THIS RETURNS TRUE. Not the splice, not a
// frozen decision (putResult / putResultGlobal / freeze), not a metric, not a debug counter,
// not rep.Replay, not a saved token figure. This function is now the only place that knows
// whether the removal is happening at all, so anything written ahead of it describes work that
// may not occur — and a frozen decision with no splice behind it is the SAME broken promise
// one layer up: a later turn's replay path reads it, deliberately bypasses the cache-tail gate
// on the reasoning that these bytes were already sent, and splices into a message that is by
// then inside the provider's cached prefix, forcing a full-suffix cache write at ~11.5x the
// read price. TestNoStateIsRecordedBeforeTheCommitGate holds every registered Offload to this.
//
// Replaying a decision ALREADY stamped and already sent is the other case, and it is not this
// function — see commitRefresh, which never refuses because refusing there is itself the
// cache-destructive move.
func commitMark(c *components.Ctx, rep *components.Report, eff markerMode, key, original string) bool {
	if eff == markerFull {
		if !store.PutStash(c.Store, key, []byte(original)) {
			stashRefusals.Add(1)
			rep.Gate("stash_reserve_exhausted")
			return false
		}
		recordOwner(c, key) // scope GET /expand retrieval to this session
		return true
	}
	rep.Irreversible = true
	return true
}

// commitRefresh refreshes the payload behind a marker THIS SESSION HAS ALREADY STAMPED AND
// SENT, and reports whether the payload is actually there.
//
// It never refuses, and that is the point of it being a different function from commitMark.
// The two look alike — both write a payload for a marker — but they sit on opposite sides of
// the decision:
//
//   - commitMark is asked "may I make this promise?", and a no costs only the tokens the
//     removal would have saved.
//   - commitRefresh is telling the store about a promise ALREADY OUTSTANDING. The marker is in
//     the provider's cached prefix. Declining here would mean sending the message verbatim
//     instead — flipping already-cached content and re-writing the whole suffix — to protect a
//     reversibility guarantee that a refusal cannot restore anyway, because the marker went out
//     turns ago. So the replay always proceeds.
//
// The return value is therefore not a permission but a DIAGNOSIS: false means the payload has
// left the store (the TTL reclaimed it) and the marker on the wire is dangling. That is the
// genuinely broken promise, and it is counted apart from stashRefusals — see stashMissing for
// why conflating them made the one dangerous outcome invisible.
func commitRefresh(c *components.Ctx, rep *components.Report, eff markerMode, key, original string) bool {
	if eff != markerFull {
		// THE DEGRADED MODES STILL NEED rep.Irreversible, and this is the whole reason the
		// parameters are here. commitMark's non-full branch sets it, so when the replay branches
		// stopped calling commitMark they stopped setting it — and a replay-only turn under
		// marker_mode summary/off then spliced with no keys, rep.Skipped false and
		// rep.Irreversible false, which is exactly the shape components/pipeline.go reverts as
		// "offload dropped content without stashing a cache_key". The transcript went upstream
		// verbatim on EVERY turn: a full-suffix cache write, the harm this change exists to stop.
		//
		// Owned here rather than at each call site because there are four of them and the last
		// two got it wrong.
		rep.Irreversible = true
		return true
	}
	if !store.PutStash(c.Store, key, []byte(original)) {
		stashMissing.Add(1)
		return false
	}
	return true
}

// stashRefusals counts removals declined because the store could not hold the original.
//
// Process-wide, like frozenHits, and reported separately from the store's own
// StashStats: the store counts refusals its reserve issued, this counts removals a
// COMPONENT abandoned because of one, and the two differ whenever a caller consults the
// answer and does something other than skip. It is the leading indicator for
// expand_unresolved_missing — which cannot move until the agent happens to ask for
// something — so a run that has quietly stopped being able to promise reversibility is
// visible here at the moment the budget binds rather than whenever the model next calls
// expand.
var stashRefusals atomic.Int64

// StashRefusals returns how many removals were declined because the store's rewind reserve
// was full. Non-zero means max_entries is too small for what this configuration removes;
// the removals did not happen, so nothing became irreversible.
func StashRefusals() int64 { return stashRefusals.Load() }

// stashMissing counts REPLAYS of a marker whose payload has left the store — a dangling marker
// that just went out on the wire.
//
// It is separate from stashRefusals because the two are opposite outcomes, and one shared
// counter made the dangerous one unreadable. Every operator-facing description of a refusal —
// /stats, cg_stash_refused_total, docs/reference/routes.md — promises that "the content was
// left verbatim and nothing became irreversible". That is true of a declined removal and FALSE
// of a dangling replay, which is the only case that actually breaks the guarantee #187 was
// about. Counting them together meant the number an operator watches to confirm nothing broke
// was incremented by things breaking.
//
// They also have different shapes over time. A refusal is one event per declined removal. A
// missing payload cannot be restored — the replayed bytes must stay byte-identical to the turn
// that created them — so it re-reports on every turn for every affected message: this counter
// grows with TURN COUNT, not with distinct broken markers. Read it as "dangling replays are
// happening", not as "N markers are dangling", and raise ttl_seconds rather than max_entries.
var stashMissing atomic.Int64

// StashMissing returns how many marker replays found no payload behind them. Non-zero means
// markers ARE on the wire that the store cannot resolve — unlike StashRefusals, this is a
// broken promise, and the leading indicator for expand_unresolved_missing.
func StashMissing() int64 { return stashMissing.Load() }

// markerModeField is the marker_mode descriptor, shared by every Offload that honors the
// key. Declared once because the accepted values are parseMarkerMode's, above.
func markerModeField() components.Field {
	return components.Field{Key: "marker_mode", Type: components.FieldEnum, Default: "full",
		Options: []string{"full", "summary", "off"},
		Hint:    "How elided content is referenced. full = a reversible <<cg:HASH>> marker the expand tool can restore; summary = a one-line digest, nothing stashed; off = no marker (irreversible)."}
}
