// Package modes implements context-guru's three operating modes (#31): the
// per-session compaction generation that makes an async result safe to apply or
// safe to throw away, and the bounded worker pool that computes those results off
// the request path.
//
// Why a generation at all. In async mode the expensive compaction runs after the
// request has already been forwarded, so its output lands in a session's frozen
// state at some later, unpredictable moment. Between enqueue and commit the agent
// may have taken another turn, and another job may have committed. Applying a
// result computed from a snapshot that no longer describes the session is how a
// compaction proxy corrupts a cached prefix. So every job records the generation it
// was built from, and a result whose generation is no longer current is DISCARDED —
// lost savings, never lost correctness.
//
// The generation advances only when a compaction actually LANDS. That is what makes
// the scheme non-starving: dedup on (session, generation) keeps at most one useful
// job in flight per session, a commit moves the session to the next generation, and
// the following turn enqueues a fresh job against the newer, longer transcript.
package modes

import "sync"

// Tracker holds the per-session state the modes need, each session's fields guarded
// by one lock so concurrent turns of a session cannot interleave a read and a write.
//
// Session lifetime is the bound, not an explicit end-of-session call: there is no
// session-end signal on this wire (an agent simply stops sending), so the tracker
// evicts under its own cap and a forgotten session restarts at generation 0 — correct,
// just missing the pending job's savings.
//
// It also owns prevLen — the number of normalized messages the previous turn carried,
// which is the already-cached/uncached boundary. That used to live in the TTL store
// and was read then written back in a `defer`, so two concurrent turns of one session
// raced on it (the hazard #31 calls out, overlapping with #25). Reading and writing it
// under the same lock, in one call, removes the race.
type Tracker struct {
	mu sync.Mutex
	m  map[string]*sessState
	// issued is a high-water mark across ALL sessions, so a session recreated after
	// eviction starts above any generation a still-in-flight job could be holding.
	issued uint64
	max    int // bound on tracked sessions; 0 => default
}

type sessState struct {
	gen       uint64
	prevLen   int
	landed    bool   // a deferred compaction has committed for this session
	committed uint64 // highest generation whose result already landed (0 = none)
	// pendingFrom is the lowest message index an enqueued-but-unlanded compaction may
	// rewrite — the start of the tail the turn that enqueued it was built from. 0 =
	// nothing pending. This is what async's cache protection must cover, and it is the
	// PREVIOUS turn's tail, not the current one's.
	pendingFrom int
	// barren counts consecutive deferred jobs that ran and produced nothing. Each such
	// job is a full off-path compaction — real cheap-model spend — so a session whose
	// traffic simply is not compactable would otherwise pay for one on every turn,
	// forever, for a saving that never materialises.
	barren int
}

// defaultMaxSessions bounds the tracker so an unbounded stream of distinct sessions
// cannot grow it without limit. Matches the store's sticky-set bound.
const defaultMaxSessions = 1000

// NewTracker returns an empty tracker. maxSessions <= 0 uses the default bound.
func NewTracker(maxSessions int) *Tracker {
	if maxSessions <= 0 {
		maxSessions = defaultMaxSessions
	}
	return &Tracker{m: map[string]*sessState{}, max: maxSessions}
}

// get returns the session's state, creating it under the caller-held lock.
func (t *Tracker) get(session string) *sessState {
	s := t.m[session]
	if s == nil {
		if len(t.m) >= t.max {
			// ponytail: arbitrary eviction, same policy as the store's sticky sets.
			for k := range t.m {
				delete(t.m, k)
				break
			}
		}
		// A recreated session must NOT restart at generation 0. An in-flight job from
		// before the eviction still holds its old generation, and starting over at 0
		// would let it match and commit over a session that has moved on. Seeding above
		// every generation ever issued makes any surviving job unmatchable — the safe
		// direction, since the cost is one discarded compaction.
		//
		// prevLen deliberately stays 0: it is a claim about what the provider has cached,
		// and after eviction we no longer know. 0 means "treat everything as tail", which
		// is what the cache-aware offloaders already handle; MaxCachedIdx = -1 (the
		// fail-open #25 addresses) is the separate concern.
		t.issued++
		s = &sessState{gen: t.issued}
		t.m[session] = s
	}
	return s
}

// Turn records that this session's current turn carries n normalized messages and
// returns the snapshot the request must be built from: the PREVIOUS turn's length
// (the cached-prefix boundary) and the generation this turn belongs to. Atomic, so two
// concurrent turns of one session each get a consistent pair and the second's write
// cannot be lost to the first's deferred write-back.
//
// The generation advances on every TURN, which is what makes "stale" mean what the
// design says it means: a job built from turn N is stale the moment turn N+1 ships.
// An earlier version advanced it only on commit, so a job from turn 1 still read its
// own generation as current after eight later turns and committed happily — the guard
// existed but could never fire for staleness, only for a dedup collision.
//
// The cost of getting this right is real and is the point of the mode's tuning knobs:
// at agent turn rates (seconds) a compaction that takes tens of seconds is usually
// superseded before it lands, so async trades a lot of would-be savings for never
// applying a decision computed against a transcript the session has moved past.
// `stale_discarded` is how you see that happening.
//
// prevLen only ever grows: an agent that re-sends a shorter transcript (a rewind, or
// a second, smaller request under the same session id) must not shrink the boundary,
// or content the provider already cached would fall back into the mutable tail.
func (t *Tracker) Turn(session string, n int) (prevLen int, gen uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.get(session)
	prevLen = s.prevLen
	if n > s.prevLen {
		s.prevLen = n
	}
	s.gen++
	if s.gen > t.issued {
		t.issued = s.gen
	}
	return prevLen, s.gen
}

// Pending returns the protected span's start (see sessState.pendingFrom): the lowest
// index a queued-but-unlanded compaction may rewrite. 0 = nothing pending.
func (t *Tracker) Pending(session string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.get(session).pendingFrom
}

// SetPending records that a compaction covering [from, end) is now queued for this
// session, so later turns keep their cache breakpoints out of that span. Called after a
// successful enqueue; Clear undoes it once the job resolves.
func (t *Tracker) SetPending(session string, from int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.get(session)
	// Keep the LOWEST pending start: two overlapping jobs mean everything from the
	// earlier one's tail onward is in play, and under-protecting is the expensive
	// direction (a rewritten cached span costs 11.5x a read).
	if s.pendingFrom == 0 || (from > 0 && from < s.pendingFrom) {
		s.pendingFrom = from
	}
}

// ClearPending records that no compaction is outstanding for this session, so the next
// turn may cache its tail again. Called when a job commits, is discarded, or fails —
// every terminal path, or the protection would latch on forever and permanently cost a
// breakpoint slot.
func (t *Tracker) ClearPending(session string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.get(session).pendingFrom = 0
}

// barrenLimit is how many consecutive unproductive deferred jobs a session may run
// before deferral is switched off for it. Small on purpose: the evidence that this
// traffic does not compact arrives immediately, and the cost of ignoring it recurs every
// turn. Any productive job resets the count.
//
// ponytail: a flat count, not a backoff schedule. A backoff would let spend resume
// periodically on traffic already shown not to compact; add one only if a workload turns
// out to become compactable mid-session.
const barrenLimit = 3

// Barren reports whether this session has exhausted its unproductive-job budget, in
// which case the host must stop enqueueing off-path compactions for it.
func (t *Tracker) Barren(session string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.get(session).barren >= barrenLimit
}

// RecordJobOutcome notes whether a deferred job produced anything usable, so a session
// that never compacts stops paying for compaction attempts.
func (t *Tracker) RecordJobOutcome(session string, productive bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.get(session)
	if productive {
		s.barren = 0
		return
	}
	s.barren++
}

// Gen returns the session's current compaction generation.
func (t *Tracker) Gen(session string) uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.get(session).gen
}

// CommitIfCurrent runs commit IF the session is still at gen — the stale-result guard.
// commit is called while the session's lock is held, so a concurrent job for the same
// session cannot also observe gen as current and commit on top of it. Returns false
// when the result was stale and therefore discarded.
//
// It also marks that this session has had a deferred compaction land, which is what
// lets the metrics distinguish savings a later turn got from replaying that work from
// savings the inline pass would have produced anyway. See Landed.
func (t *Tracker) CommitIfCurrent(session string, gen uint64, commit func()) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.get(session)
	// Stale: a later turn has already shipped, so this job was built from a transcript
	// the session has moved past.
	if s.gen != gen {
		return false
	}
	// Already satisfied: another job for this same generation committed first. The pool's
	// dedup makes this unreachable in production (one job per key), but the guard must be
	// exact on its own — a second commit at one generation would apply two independent
	// compactions of the same snapshot on top of each other.
	if s.committed >= gen {
		return false
	}
	if commit != nil {
		commit()
	}
	s.committed = gen
	s.landed = true
	return true
}

// Landed reports whether a deferred compaction has ever committed for this session.
// The metrics use it to gate "realized" savings: before anything has landed, whatever
// the inline pass saved was saved by deterministic components on the request path, not
// by deferred work, and crediting it to the deferral would be circular.
func (t *Tracker) Landed(session string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.get(session).landed
}

// Sessions reports how many sessions are tracked (test/telemetry aid).
func (t *Tracker) Sessions() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.m)
}
