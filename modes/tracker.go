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
	"hash/fnv"
	"sync"
	"sync/atomic"
	"time"
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

// DefaultMaxKeys bounds the tracker so an unbounded stream of distinct keys cannot grow it
// without limit. The key is a THREAD (thread.Key), not a session (#484), so one busy session
// holds one entry per subagent and per side request. 4,096 entries of about 40 bytes plus the
// key is well under 1 MB. Exported so other per-thread maps (extract_llm's growth record) use
// the same bound.
const DefaultMaxKeys = 4096

// StaleAfter is how long a key may go without a request before the tracker may expire it. It is
// a MEMORY bound, not a cache verdict: a keep-alive can hold a provider entry warm with no
// request at all, so an expired key is remembered as dropped (see Tracker.dropped) and never
// read as "nothing cached". 6 hours is far past the longest provider TTL (1 hour) plus the
// default keep-alive span.
const StaleAfter = 6 * time.Hour

// Tracker holds the cached-prefix boundary per key (a session, or one thread of a session), each
// key's state guarded by one lock so concurrent turns cannot interleave a read and a write.
type Tracker struct {
	mu  sync.Mutex
	m   map[string]turn
	max int    // bound on tracked keys; 0 => default
	seq uint64 // use counter: each turn stamps its key, and the smallest stamp is evicted first
	// startedAtMs is when this tracker was created, in unix milliseconds: a proxy restart's
	// marker. An unknown key whose first request comes less than that request's cache lifetime
	// after the start reads as all-cached (#496). 0 (a Tracker built without NewTracker) gets
	// no grace, which is the old behaviour.
	startedAtMs int64
	// graceOn is the host's on/off switch for that rule. The length is not ours to choose: each
	// request carries its own cache lifetime, and the caller passes it to TurnFrom. A benchmark
	// that sends whole transcripts to a fresh proxy switches the rule off (false) so it measures
	// compaction, not the grace.
	graceOn bool
	// dropped holds a hash of every key the tracker evicted or expired, bounded at 4x max. A
	// later request on such a key gets the WHOLE request as its boundary (nothing may change),
	// not 0. 0 would let every component rewrite a prefix the provider may still hold — the
	// same cache write a proxy restart causes. 8 bytes a key, so it outlives the records.
	dropped map[uint64]struct{}
}

// turn is what one key's previous request left behind: how long the transcript was, and WHEN
// it happened.
//
// The timestamp is here rather than in a second map because the two are read together and
// under the same lock, and because this is already the one place per-key request state is
// bounded. It answers a question nothing on the request path could answer before: how long
// has this thread been idle — and therefore, given the provider's cache TTL, is the prefix we
// are about to protect still cached at all?
type turn struct {
	n    int
	at   int64  // unix milliseconds; 0 = no previous turn recorded
	used uint64 // Tracker.seq at the last turn on this key
}

// NewTracker returns an empty tracker. maxKeys <= 0 uses DefaultMaxKeys.
func NewTracker(maxKeys int) *Tracker {
	if maxKeys <= 0 {
		maxKeys = DefaultMaxKeys
	}
	return &Tracker{m: map[string]turn{}, max: maxKeys, startedAtMs: time.Now().UnixMilli(),
		graceOn: true, dropped: map[uint64]struct{}{}}
}

// SetStartedAt sets the tracker's start time (unix milliseconds). NewTracker stamps the real
// clock; a test can set a past time to build a tracker BEYOND its restart grace (#496) without
// waiting for the grace to pass.
func (t *Tracker) SetStartedAt(ms int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.startedAtMs = ms
}

// SetRestartGrace switches the restart grace on or off. The grace's LENGTH is per request (the
// caller passes the cache lifetime to TurnFrom); this is only the on/off switch, which the host
// passes its setting to (the proxy's --restart-grace). A benchmark that sends whole transcripts
// to a fresh proxy switches it off. Startup setting, not a live knob.
func (t *Tracker) SetRestartGrace(on bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.graceOn = on
}

// Turn records that this session's current turn carries n normalized messages and
// returns the PREVIOUS turn's count — the cached-prefix boundary the request must be
// built against. Read and write happen under one lock, which is what removes the race
// described in the package comment.
//
// A shorter transcript under the same session id is the agent's own compaction, and
// resets the boundary to 0 — see Boundary for the rule and why it is not "grow only".
func (t *Tracker) Turn(session string, n int) (prevLen int) {
	prevLen, _ = t.TurnAt(session, n, 0, 0)
	return prevLen
}

// TurnAt is Turn plus the previous turn's timestamp, so the caller can tell how long this
// session has been idle. nowMs of 0 means "do not record a time" (the legacy Turn path);
// the clock is the caller's so this stays a pure, testable function. grace is this request's
// cache lifetime; it matters only for a key with no record (see TurnFrom).
//
// prevAtMs is 0 when there is no previous turn recorded — a new session, or one evicted by
// the bound above. A caller MUST treat that as "unknown", never as "idle forever": acting
// on a fabricated idle time is how a warm cache gets invalidated after a proxy restart.
func (t *Tracker) TurnAt(session string, n int, nowMs int64, grace time.Duration) (prevLen int, prevAtMs int64) {
	return t.TurnFrom(session, "", n, nowMs, grace)
}

// TurnFrom is TurnAt for a key that may be a NEW thread continuing another thread's prefix.
//
// parent is the key of the thread this one inherits from ("" for none): Claude Code's side
// requests (title, quota check, end-of-turn note) and forks start with the main thread's whole
// transcript, which the provider already holds. When key has no record of its own, its boundary
// and idle clock come from parent's record, so the first request of such a thread protects the
// cached prefix it reads. parent's record is only READ, never written: the parent's next request
// must still be measured against its own last request (#484).
//
// The inherited boundary is min(parent's count, n): a fork shorter than its parent diverged
// somewhere inside the parent's prefix, and freezing all of it is the safe direction. It is never
// counted as a compaction.
//
// A key the tracker dropped (evicted or expired), or a new key whose parent was dropped, gets
// prevLen = n: the whole request is treated as cached, so nothing changes on this one turn, and
// the next turn has a real record again.
//
// The same answer serves a proxy restart (#496): a key with no record, no drop and no usable
// parent reads as all-cached while the tracker started less than THIS request's cache lifetime
// ago (the caller passes it as grace), because the process forgot the record, not the provider
// the prefix. The grace is per thread and ends at that first request, which writes the record:
// the thread's second request follows the normal rules. A brand-new thread's first request
// cannot be changed either, for the same lifetime. A first request past the lifetime is a new
// thread again, as before. The host can switch the rule off (SetRestartGrace).
func (t *Tracker) TurnFrom(key, parent string, n int, nowMs int64, grace time.Duration) (prevLen int, prevAtMs int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	prev, ok := t.m[key]
	switch {
	case ok:
		prevLen, prevAtMs = Boundary(prev.n, n), prev.at
	case t.wasDropped(key):
		prevLen = n
	case parent == "":
		prevLen = t.graceLocked(n, nowMs, grace)
	default:
		if p, pok := t.m[parent]; pok {
			prevLen, prevAtMs = min(p.n, n), p.at
		} else if t.droppedLocked(parent) {
			// The parent was evicted, but the provider may still hold the prefix this request
			// reads: all cached, as for a dropped key of its own. Peek only, so the parent's own
			// next request still reads as dropped too.
			prevLen = n
		} else {
			prevLen = t.graceLocked(n, nowMs, grace)
		}
	}
	if !ok && len(t.m) >= t.max {
		t.pruneLocked(nowMs)
	}
	// Always record THIS turn's length: on growth it is the max anyway, and on a shrink
	// it is the new prefix every later turn must be measured against.
	at := prev.at
	if nowMs > 0 {
		at = nowMs
	}
	t.seq++
	t.m[key] = turn{n: n, at: at, used: t.seq}
	return prevLen, prevAtMs
}

// graceLocked is the boundary for a key with no record, no drop and no usable parent: n when
// the tracker started less than grace — this request's cache lifetime, the longest a
// restart-forgotten entry of this thread can still be alive (#496) — ago, 0 as before once
// that window is past. prevAtMs stays 0 either way: the idle clock is unknown, so nothing may
// read cold off it.
//
// nowMs <= 0 (the legacy Turn path, which records no time) gets no grace: there is no clock to
// measure the restart against, and the request path always supplies one. A caller clock BEHIND
// the start (a test's fixed past timestamp) is not a restart either: it reads as past the grace,
// which keeps fake-clock callers on the old behaviour.
func (t *Tracker) graceLocked(n int, nowMs int64, grace time.Duration) int {
	if !t.graceOn || t.startedAtMs == 0 || nowMs <= 0 || grace <= 0 {
		return 0
	}
	if d := nowMs - t.startedAtMs; d >= 0 && d < grace.Milliseconds() {
		return n
	}
	return 0
}

// pruneLocked makes room for one new key. It first expires every key idle past StaleAfter
// (side-request keys are used once and land here), and only if none is stale does it evict the
// least recently used key. Either way the key is remembered in dropped, so its next request
// reads as "all cached", never as "nothing cached". The scans are O(n), and they run only when a
// NEW key arrives at a full tracker.
func (t *Tracker) pruneLocked(nowMs int64) {
	if nowMs > 0 {
		for k, v := range t.m {
			if v.at > 0 && nowMs-v.at > StaleAfter.Milliseconds() {
				t.dropLocked(k)
			}
		}
		if len(t.m) < t.max {
			return
		}
	}
	var oldest string
	var least uint64
	first := true
	for k, v := range t.m {
		if first || v.used < least {
			oldest, least, first = k, v.used, false
		}
	}
	if !first {
		t.dropLocked(oldest)
	}
}

func (t *Tracker) dropLocked(key string) {
	delete(t.m, key)
	if len(t.dropped) >= 4*t.max {
		// ponytail: arbitrary eviction. A forgotten drop reads as a new key (boundary 0), which
		// is the old behaviour. Add order only if drops outrun 4x the live bound in practice.
		for h := range t.dropped {
			delete(t.dropped, h)
			break
		}
	}
	t.dropped[keyHash(key)] = struct{}{}
}

// wasDropped reports, and forgets, whether key was evicted or expired. A hash collision makes
// a new key read as dropped, which only freezes its first request: the safe direction.
func (t *Tracker) wasDropped(key string) bool {
	h := keyHash(key)
	if _, ok := t.dropped[h]; ok {
		delete(t.dropped, h)
		return true
	}
	return false
}

// droppedLocked reports whether key was evicted or expired, without forgetting it.
func (t *Tracker) droppedLocked(key string) bool {
	_, ok := t.dropped[keyHash(key)]
	return ok
}

func keyHash(key string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(key))
	return h.Sum64()
}

// Sessions reports how many sessions are tracked (test/telemetry aid).
func (t *Tracker) Sessions() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.m)
}
