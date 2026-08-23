package expand

import (
	"regexp"
	"sync/atomic"
)

// UNRESOLVED-EXPAND ACCOUNTING.
//
// An expand call the proxy cannot satisfy is the failure mode that makes lossy compaction unsafe, and
// until now it was INVISIBLE: nothing in /stats recorded it, so three experiment iterations ran while
// the model was calling the tool and being refused. The refusal was only found by grepping the
// benchmark client's own transcripts for the string "not found".
//
// Two causes, kept apart because they call for opposite responses:
//
//   MALFORMED — the id does not look like a marker this proxy ever issues. The model invented or
//   garbled it. Nothing to fix here; it is the model's error, and a placeholder is the right answer.
//
//   MISSING — the id is well formed but nothing is stashed under it. THIS is a context-guru defect:
//   a marker was issued and its original is gone, so a cut advertised as reversible is not. Every
//   count here is a case where reversibility silently failed.
//
// Package-level like modes.compactionResets and offload's frozen counters, for the same reason: the
// host merges them into the snapshot at serve time, since metrics cannot import this package.
var (
	unresolvedMalformed atomic.Int64
	unresolvedMissing   atomic.Int64
)

// noteUnresolved records one expand id the proxy could not satisfy. wellFormed distinguishes a
// garbled id from a lost stash.
func noteUnresolved(wellFormed bool) {
	if wellFormed {
		unresolvedMissing.Add(1)
		return
	}
	unresolvedMalformed.Add(1)
}

// Unresolved returns (malformed, missing) since process start. missing > 0 means reversibility
// failed for that many cuts.
func Unresolved() (malformed, missing int64) {
	return unresolvedMalformed.Load(), unresolvedMissing.Load()
}

// WellFormedID reports whether id has the shape of a marker this proxy issues. Used to classify an
// unresolvable expand call rather than to validate input.
func WellFormedID(id string) bool { return idShapeRe.MatchString(id) }

// idShapeRe is the marker payload shape from markerRe in expand.go, anchored so a whole id
// can be tested. Kept beside the classifier so the two cannot drift apart silently.
var idShapeRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
