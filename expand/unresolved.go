package expand

import (
	"sync/atomic"
)

// Why an unresolvable marker id is TWO events and not one.
//
// When a marker's id resolves to nothing, the model gets Unavailable() either way and the request
// proceeds. But the two causes are not the same thing and they need opposite responses:
//
//	MALFORMED  the model invented or corrupted the id. Nothing to fix here — it is the model
//	           being a model, and the placeholder is the correct answer.
//	MISSING    the id is one this proxy could have minted, and nothing is behind it. That is a
//	           context-guru DEFECT: a cut was advertised as reversible and it was not. The stash
//	           expired, the store did not persist, or the key was written under a session id
//	           nothing reads.
//
// Collapsed into one placeholder they were indistinguishable, and the second is the one an operator
// must be able to alert on. Nothing else can stand in for it: RecordExpand / wasted_tokens counts
// tokens successfully re-served, and sse_expand_after_stream counts a streaming miss — neither can
// go non-zero for a broken stash, so a silent no-op looked exactly like "no expand calls happened".
// That is how expand refusals ran unnoticed for three iterations, found by grepping a benchmark
// client's transcripts rather than from any counter here.
var (
	unresolvedMalformed int64
	unresolvedMissing   int64
)

// Unresolved returns (malformed, missing) counts for marker ids that resolved to nothing.
//
// `missing` is the alertable one. Non-zero means this proxy removed content, told the model it could
// have it back, and then could not produce it.
func Unresolved() (malformed, missing int64) {
	return atomic.LoadInt64(&unresolvedMalformed), atomic.LoadInt64(&unresolvedMissing)
}

// NoteUnresolved classifies one failed resolution and counts it. Exported because both halves of
// reversibility reach it — the request-path repair in this package, and the proxy's response-side
// continuation loop — and a defect counted in only one of them would read as half as bad.
func NoteUnresolved(id string) { noteUnresolved(id) }

// noteUnresolved classifies one failed resolution and counts it.
func noteUnresolved(id string) {
	if WellFormedID(id) {
		atomic.AddInt64(&unresolvedMissing, 1)
		return
	}
	atomic.AddInt64(&unresolvedMalformed, 1)
}

// idHexLens are the marker-id lengths this proxy mints. Every stash key that reaches a
// <<cg:HASH>> marker is a lowercase-hex prefix of a sha256, and there are exactly two lengths in
// use: 16 (components/offload.hashKey, which summarize's span stash uses) and 24
// (internal/extract.ContentKey and offload's state key).
//
// COUPLED TO THOSE MINTERS ON PURPOSE, AND THE COUPLING IS TESTED. Validating shape is the only
// option that does not change marker bytes, and marker bytes are prefix-cache-relevant — injecting
// a different marker text on a later turn is a full prefix miss (see inject.go). The alternative,
// making ids self-identifying with an HMAC or a prefix, would be exact but would rewrite every
// marker.
//
// So the risk is that a minter changes shape and this quietly starts calling real ids malformed —
// which would zero the alertable counter, the worst direction. TestEveryMintedKeyIsWellFormed calls
// the real minters and asserts this accepts what they produce, so that change fails loudly here
// rather than silently downgrading a defect signal.
var idHexLens = map[int]bool{16: true, 24: true}

// WellFormedID reports whether id has the shape of a marker id this proxy mints, i.e. whether an
// unresolved lookup for it is a context-guru defect rather than a model invention.
//
// Deliberately conservative in the direction that matters: an id this returns false for is counted
// as the model's fault, so a shape check that is too NARROW hides our own defects. Hence the tested
// round-trip against the minters rather than a hand-maintained pattern.
func WellFormedID(id string) bool {
	if !idHexLens[len(id)] {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
