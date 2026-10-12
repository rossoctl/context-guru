// Package modes holds the per-session state context-guru's operating modes need.
//
// Today that is one thing: the cached-prefix boundary, i.e. how many normalized
// messages the previous turn of a session carried. Everything at or below it is already
// committed to the provider's cache, so supersession/age-based offloaders must confine
// their mutations to the tail above it (components.Ctx.MaxCachedIdx).
//
// It lives here rather than in the TTL store because it is turn accounting, not cached
// payload, and because reading it and recording the new value must be ONE atomic step.
// The previous implementation read it from the store and wrote it back in a `defer`, so
// two concurrent turns of one session raced: both read the same length, and the second's
// write-back could land before the first's, leaving the boundary describing neither turn.
// A boundary that is too high lets an offloader mutate content the provider has cached,
// which costs a full cache-write of the suffix.
package modes

import (
	"sync"
	"sync/atomic"
)

// compactionResets counts, process-wide, how many turns had their cached-prefix
// boundary RESET because the transcript shrank under a stable session id — i.e. how
// many times a session restarted its prefix. /stats reports it as compaction_resets.
// Package-level like offload's frozenHits/frozenMisses, and for the same reason: the
// host merges it into the snapshot at serve time (metrics cannot import modes).
var compactionResets atomic.Int64

// CompactionResets returns the cumulative compaction resets since process start.
func CompactionResets() int64 { return compactionResets.Load() }

// Boundary returns the cached-prefix boundary a turn carrying n normalized messages
// must be built against, given that the previous turn of the same session carried prev.
//
// A SHRINK means compaction: the agent replaced its own transcript with a summary and
// continued, so this request is not an extension of the one prev was recorded against —
// prev describes message indices that no longer exist. The boundary restarts at 0 (whole
// request is the mutable tail) so components can act against the NEW prefix, and the
// reset is counted.
//
// The rule is "any shrink", not "a shrink past some fraction". Distinguishing a real
// compaction from a rewind/retry/truncated resend would need content comparison on the
// hot path, and a fractional threshold silently misses a partial compaction (Claude Code's
// "RECENT portion" bodies trim far less than half). The trade is asymmetric: a spurious
// reset costs at most one bounded cache-write over a transcript that is by definition
// SHORTER (and the freeze/reapply machinery keeps the resulting decision byte-stable
// thereafter), while failing to reset freezes every message of every later turn for the
// REST OF THE SESSION — savings gone permanently. So reset, and count it.
//
// This replaces the older "the boundary only ever grows" rule, which was load-bearing
// only while a compaction flipped the session id and started the tracker fresh. Its
// stated reason — content the provider already cached would fall back into the mutable
// tail — does not survive a real compaction: the agent has already replaced that content,
// so the provider's cached prefix stops matching beyond the common head no matter what we
// do. The invalidation has happened; rewriting the new tail is not what costs it.
//
// One consequence, deliberate: two CONCURRENT turns of one session that arrive
// out of order now look like a shrink, so the later-but-shorter one resets and reports a
// compaction. That is lost savings for one turn, never a wrong rewrite, and per-session
// concurrency is not how agents talk.
func Boundary(prev, n int) int {
	if n < prev {
		compactionResets.Add(1)
		return 0
	}
	return prev
}

// Tracker holds the per-session cached-prefix boundary, each session's state guarded by
// one lock so concurrent turns cannot interleave a read and a write.
type Tracker struct {
	mu  sync.Mutex
	m   map[string]turn
	max int    // bound on tracked keys; 0 => default
	seq uint64 // use counter: each TurnAt stamps its key, and the smallest stamp is evicted
}

// turn is what one session's previous request left behind: how long the transcript was,
// and WHEN it happened.
//
// The timestamp is here rather than in a second map because the two are read together and
// under the same lock, and because this is already the one place per-session request state
// is bounded. It answers a question nothing on the request path could answer before: how
// long has this session been idle — and therefore, given the provider's cache TTL, is the
// prefix we are about to protect still cached at all?
type turn struct {
	n    int
	at   int64  // unix milliseconds; 0 = no previous turn recorded
	used uint64 // Tracker.seq at the last TurnAt on this key
}

// defaultMaxSessions bounds the tracker so an unbounded stream of distinct keys cannot grow it
// without limit. The key is a THREAD (thread.Key), not a session (#484), so one busy session
// holds one entry per subagent. 4,096 entries of about 40 bytes plus the key is well under 1 MB.
// Eviction is least-recently-used, so the threads still sending requests keep their record.
const defaultMaxSessions = 4096

// NewTracker returns an empty tracker. maxSessions <= 0 uses the default bound.
func NewTracker(maxSessions int) *Tracker {
	if maxSessions <= 0 {
		maxSessions = defaultMaxSessions
	}
	return &Tracker{m: map[string]turn{}, max: maxSessions}
}

// Turn records that this session's current turn carries n normalized messages and
// returns the PREVIOUS turn's count — the cached-prefix boundary the request must be
// built against. Read and write happen under one lock, which is what removes the race
// described in the package comment.
//
// A shorter transcript under the same session id is the agent's own compaction, and
// resets the boundary to 0 — see Boundary for the rule and why it is not "grow only".
func (t *Tracker) Turn(session string, n int) (prevLen int) {
	prevLen, _ = t.TurnAt(session, n, 0)
	return prevLen
}

// TurnAt is Turn plus the previous turn's timestamp, so the caller can tell how long this
// session has been idle. nowMs of 0 means "do not record a time" (the legacy Turn path);
// the clock is the caller's so this stays a pure, testable function.
//
// prevAtMs is 0 when there is no previous turn recorded — a new session, or one evicted by
// the bound above. A caller MUST treat that as "unknown", never as "idle forever": acting
// on a fabricated idle time is how a warm cache gets invalidated after a proxy restart.
func (t *Tracker) TurnAt(session string, n int, nowMs int64) (prevLen int, prevAtMs int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	prev, ok := t.m[session]
	if !ok && len(t.m) >= t.max {
		t.evictLRULocked()
	}
	// Always record THIS turn's length: on growth it is the max anyway, and on a shrink
	// it is the new prefix every later turn must be measured against.
	at := prev.at
	if nowMs > 0 {
		at = nowMs
	}
	t.seq++
	t.m[session] = turn{n: n, at: at, used: t.seq}
	return Boundary(prev.n, n), prev.at
}

// evictLRULocked drops the least recently used key. A dropped key restarts at 0 ("treat
// everything as tail", idle time unknown): correct, just less saving. The eviction used to be
// arbitrary (a random map key), which was harmless while one session was one entry; with one
// entry per thread, a burst of new subagents could evict the main thread of a session that was
// still busy. The scan is O(n), and it runs only when a NEW key arrives at a full tracker.
func (t *Tracker) evictLRULocked() {
	var oldest string
	var least uint64
	first := true
	for k, v := range t.m {
		if first || v.used < least {
			oldest, least, first = k, v.used, false
		}
	}
	if !first {
		delete(t.m, oldest)
	}
}

// Sessions reports how many sessions are tracked (test/telemetry aid).
func (t *Tracker) Sessions() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.m)
}
