// Package store holds context-guru's cross-call state behind one interface so
// both hosts (bifrost proxy, AuthBridge plugin) share it. v1 ships an in-memory
// TTL+LRU backend; SQLite/Redis slot in behind the same interface when a
// durable or multi-replica deployment is real (see the design doc, D5).
//
// It carries three things keyed by session:
//   - Rewind: cache_key -> original bytes, so Offload components are reversible
//     (the expand(id) tool loop resolves originals from here).
//   - Sticky: the set of content ids already reduced on prior turns, so a
//     component can keep its output byte-stable across turns (cache stability).
//   - per-session token/metric rollups (added with metrics in P0/P5).
package store

import (
	"container/list"
	"strings"
	"sync"
	"time"
)

// Store is the interface components and adapters depend on. Implementations
// must be safe for concurrent use — one instance serves all requests.
type Store interface {
	// Put stashes an original payload under key with the store's default TTL.
	Put(key string, payload []byte)
	// Get returns a stashed payload; ok=false if absent or expired.
	Get(key string) (payload []byte, ok bool)
	// Sticky returns the per-session set of already-reduced content ids.
	Sticky(session string) map[string]struct{}
	// MarkSticky records that id was reduced in this session.
	MarkSticky(session, id string)
	// Persists reports whether Put actually retains payloads. false (the Nop
	// store) means offloads cannot be made reversible, so a full marker_mode must
	// degrade to an irreversible drop rather than leave an unresolvable marker.
	Persists() bool
}

// Stasher is an OPTIONAL Store capability: writing a REWIND PAYLOAD — the original bytes
// behind a <<cg:HASH>> marker — under its own reserved budget, and reporting whether the
// payload was actually retained.
//
// It exists because a rewind payload cannot be recognised from its key. The pin namespaces
// below all carry a "cg:" prefix, but a stash key IS the marker id: a bare lowercase-hex
// content hash the model reads out of the request and hands back to the expand tool (see
// expand.Marker). Prefixing it would rewrite every marker's bytes, and marker bytes sit
// inside the provider's cached prefix — so the only way for the store to tell a payload from
// any other entry is for the writer to say so. That is this method.
//
// The RETURN VALUE is the load-bearing half. Before it, a payload was written with Put and
// then evicted as the least-recently-used entry of a cache whose pinned half held the
// DECISIONS referring to it, so a removal advertised as reversible (a marker was stamped)
// became irreversible with nothing reported — issue #187. false now means "this payload is
// NOT stored", and the caller's contract is to not advertise reversibility: refuse the
// removal and leave the content verbatim. See components/offload.commitMark.
//
// Stores that do not implement it degrade to the legacy behavior (Put, always "retained"),
// which is why writers go through the PutStash helper rather than asserting at each site.
type Stasher interface {
	// PutStash stores a rewind payload under key and reports whether it is now retained.
	// Refreshing a payload already present always succeeds — a live entry must never be
	// refused, or a replay of an already-stamped marker would flip that message's bytes.
	PutStash(key string, payload []byte) bool
	// StashRoom reports whether a NEW payload of size bytes would be admitted right now.
	// It is a probe, not a reservation: it claims nothing, and a PutStash after it can
	// still refuse (another goroutine took the slot). It exists for the one caller shape
	// that cannot recover from a refusal cheaply — a component that must PAY A MODEL CALL
	// to produce the text whose marker the payload backs. summarize's call was measured at
	// ~57k prompt tokens, and under a saturated reserve it was paid and thrown away on
	// every turn. Asking first turns that into a skip.
	StashRoom(size int) bool
}

// PutStash writes a rewind payload through the Stasher capability when the store has it,
// and reports whether the payload is retained. A store without the capability keeps the
// legacy behavior: an ordinary Put, reported as retained.
func PutStash(s Store, key string, payload []byte) bool {
	if st, ok := s.(Stasher); ok {
		return st.PutStash(key, payload)
	}
	s.Put(key, payload)
	return true
}

// StashRoom reports whether a new payload of size bytes would be admitted. A store without
// the Stasher capability has no reserve to exhaust, so it always has room — the same
// direction PutStash degrades in.
func StashRoom(s Store, size int) bool {
	if st, ok := s.(Stasher); ok {
		return st.StashRoom(size)
	}
	return true
}

// FrozenLoser is an OPTIONAL Store capability: reporting that a frozen decision
// under key was dropped (TTL expiry / pin cap) rather than never taken. A bare Get
// miss cannot tell those apart, and they call for opposite behavior — "never frozen"
// means obey the tail gate, "was frozen, now lost" means re-derive the same bytes so
// the cached prefix does not flip. Stores that don't implement it degrade to the
// legacy indistinguishable behavior.
type FrozenLoser interface {
	// FrozenLost reports whether a frozen entry under key existed and was dropped.
	FrozenLost(key string) bool
}

// Key namespaces whose entries are a component's FROZEN decision — the replacement
// text it must replay on every later turn to keep an already-cached message
// byte-identical (see components/offload/state.go), plus the small per-session trackers
// the cache-safety machinery itself depends on.
//
// Entries under these prefixes are PINNED: exempt from LRU eviction, because losing one
// is not a cache miss, it is a cache-DESTRUCTIVE event — the message flips representation
// inside the provider's cached prefix and the whole suffix is re-written at 11.5x the
// read price. They are small (a marker line, a compacted projection, an integer), still
// honor the sliding TTL, and the exemption is capped at half the entry cap so a
// pathological session can never pin the whole cache.
//
// The prefixes are declared by their OWNERS (components/offload, apply) and passed in via
// Options.PinPrefixes — the store must not know what a component names its keys.
//
// The rewind stashes are NOT in this list and never can be (their keys are bare marker ids —
// see Stasher), which used to mean they were "fully evictable". That was the bug in #187:
// this cache kept "that output was dropped" while evicting "here is what it was", so the
// reversibility guarantee degraded exactly as the pipeline removed more. They now have their
// own reserve, claimed through PutStash and bounded by stashCap.
const (
	FrozenPrefix = "cg:frz:" // mask / failed_run freeze decisions
	ResultPrefix = "cg:res:" // extract_llm's replayed result (projection + summary, one key)
	LenPrefix    = "cg:len:" // apply's prev-turn message count (the MaxCachedIdx boundary)
	// XResultPrefix is extract_llm's CROSS-session result namespace. Pinned for the same
	// reason ResultPrefix is: every entry is a model call already paid for, and the store's
	// default cap is 1,000 entries shared with the unpinned expand stashes — which are the
	// large payloads, so they evict the cheap keys that would have avoided a call.
	XResultPrefix = "cg:xres:"
	// TTLPrefix and SeenPrefix are apply's two cold-decision records. Losing either makes a
	// WARM prefix read cold, which is the cache-destructive direction: TTLPrefix narrows a
	// session that asked for the 1h tier back to 5m, and SeenPrefix drops the evidence that
	// another session id touched the same content-keyed entry. Both are refreshed on every
	// turn, so the sliding TTL keeps an active session's records alive; the pin is only
	// against LRU pressure from a busy proxy.
	TTLPrefix  = "cg:ttl:"  // longest cache lifetime this session has ever asked for
	SeenPrefix = "cg:seen:" // last activity under a content-derived session id
)

// DefaultPinPrefixes is the shipped set of key namespaces whose loss is cache-destructive.
// Callers that build their own Store may pass a different set; the zero value means "none",
// so a host that opts out simply gets plain TTL+LRU.
var DefaultPinPrefixes = []string{FrozenPrefix, ResultPrefix, LenPrefix, XResultPrefix, TTLPrefix, SeenPrefix}

// pinned reports whether key belongs to one of the configured pin namespaces.
func (m *Memory) isPinPrefix(key string) bool {
	for _, p := range m.pinPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

type entry struct {
	key     string
	payload []byte
	expires time.Time
	pinned  bool // exempt from LRU eviction (frozen decision); TTL still applies
	// stash marks a REWIND PAYLOAD written through PutStash. Also exempt from LRU
	// eviction, and — unlike a pin, which silently degrades to an evictable entry once
	// over its cap — a stash that cannot be admitted is REFUSED, so the caller declines
	// the removal instead of stamping a marker nothing can resolve. TTL still applies.
	stash bool
}

// Memory is an in-memory Store: a TTL+LRU cache for rewind payloads plus a
// bounded per-session sticky-id set. The TTL is SLIDING (refreshed on Get), and
// the default (DefaultTTL) is sized past a long-horizon agent task rather than
// mirroring headroom's 1800s CCR store: a frozen compaction that dies mid-task is
// a cache-destructive event, not a saving.
type Memory struct {
	mu          sync.Mutex
	ttl         time.Duration
	max         int
	ll          *list.List               // LRU, front = most recent
	items       map[string]*list.Element // key -> element(*entry)
	sticky      map[string]map[string]struct{}
	maxStick    int
	pinPrefixes []string
	now         func() time.Time // injectable for tests
	pinnedN     int              // live pinned (frozen) entries, capped at pinCap()
	stashN      int              // live rewind payloads, capped at stashCap()
	// stashBytes is the reserve's REAL cost, and stashMaxBytes is what bounds it. Entries
	// are the wrong unit for this one namespace: every other exempt entry is a marker line,
	// a compacted projection or an integer, while a payload is a whole tool output — so
	// "5,000 entries" names a memory figure anywhere between a few megabytes and a few
	// gigabytes depending on nothing the operator chose. The entry caps below still apply
	// (list and map slots cost something, and the evictable floor is an entry property);
	// this is the budget that binds first when payloads are large.
	stashBytes    int64
	stashMaxBytes int64
	// nextExpiry is a LOWER BOUND on the earliest expires among live entries, so a sweep
	// can be skipped outright when nothing can possibly have expired. Without it every
	// refused PutStash walked the whole list under this mutex looking for reclaimable
	// entries — O(max) per refusal, on precisely the saturated path where refusals are
	// continuous. A lower bound is enough: too low costs one wasted sweep, and it can never
	// be too high because every write sets its entry's expiry to now+ttl, the latest of any.
	nextExpiry time.Time
	// stashRefusedN counts payloads PutStash DECLINED because the reserve was full — i.e.
	// removals that were not made, rather than removals made irreversibly. stashExpiredN
	// counts payloads the TTL reclaimed, which is the only way a stash leaves now that LRU
	// pressure cannot evict one.
	stashRefusedN int64
	stashExpiredN int64
	// lostFrozen remembers keys whose FROZEN entry was dropped anyway (TTL expiry, or
	// the pin cap). It is the "was frozen, now LOST" signal a caller cannot otherwise
	// distinguish from "never frozen" — see FrozenLost. Bounded like sticky.
	lostFrozen map[string]struct{}
	lostOrder  []string // insertion order, so the OLDEST mark is evicted first
	lostN      int64
	repairedN  int64
	noSlide    bool // tests only: restore the old write-only expiry (see DisableSlidingTTLForTest)
}

// Options configures a Memory store; the zero value yields sane defaults.
// yaml tags let it drop straight into the config file's store: block.
type Options struct {
	// Enabled toggles the state store. nil/absent => on (backward-compatible).
	// false => no store: reversibility is off, so offload components must run
	// marker_mode: off (a full-marker offload would leave dangling markers).
	Enabled    *bool `yaml:"enabled"`
	TTLSeconds int   `yaml:"ttl_seconds"`
	// PinPrefixes are key namespaces whose entries are exempt from LRU eviction because
	// losing one is cache-destructive rather than merely a miss (see FrozenPrefix). nil =>
	// DefaultPinPrefixes. Not a yaml knob: it is a code-level property of the key layout,
	// not something an operator should be tuning.
	PinPrefixes []string `yaml:"-"`
	MaxEntries  int      `yaml:"max_entries"`
	// StashMaxBytes caps the rewind reserve in BYTES. Zero => DefaultStashMaxBytes. See
	// Memory.stashBytes for why this namespace is budgeted in bytes and the rest in entries.
	StashMaxBytes int64 `yaml:"stash_max_bytes"`
	MaxSessions   int   `yaml:"max_sessions"`
}

// Nop is a Store that persists nothing: Put discards, Get/Sticky always miss.
// Used when the store is disabled — the expand loop resolves nothing and lossy
// offloads become irreversible, which is why they must use marker_mode: off.
type Nop struct{}

func (Nop) Put(string, []byte) {}

// PutStash retains nothing, so it reports nothing retained. Reachable only if a caller
// skips effectiveMode (which already degrades a full marker to off when !Persists()), and
// the answer is the same either way: do not advertise reversibility.
func (Nop) PutStash(string, []byte) bool { return false }

// StashRoom is false for the same reason PutStash is: there is nowhere to put a payload, so
// a caller about to pay a model call for content this store would have to back should not.
func (Nop) StashRoom(int) bool { return false }

func (Nop) Get(string) ([]byte, bool)         { return nil, false }
func (Nop) Sticky(string) map[string]struct{} { return nil }
func (Nop) MarkSticky(string, string)         {}
func (Nop) Persists() bool                    { return false }

// DefaultTTL is the store's default (sliding) entry lifetime. Terminal-Bench tasks
// averaged 1975s of wall clock and run up to 4h, so the old 1800s default expired
// live frozen decisions mid-task; ~2.8h covers a long-horizon task's idle gaps
// (test suites, training runs) with the sliding refresh doing the rest.
const DefaultTTL = 10000 * time.Second

// DefaultMaxEntries is the store's default entry cap.
//
// It was 1,000, and that was the quantity #187 was measured against: ONE process-wide store
// serves every concurrent session (cmd/context-guru-proxy builds it once), and each
// reversible removal writes FIVE entries — the payload, cg:own:, cg:xseen:, and two pinned
// decision records (cg:res:, cg:xres:). So a 1,000-entry cache held roughly 1/5 of half its
// slots' worth of payloads while the decisions naming them were pinned, and iteration 024's
// arm B — hundreds of removals across eight concurrent workers — overran it by an order of
// magnitude. 5,000 puts the cap in the same order as the observed volume; PutStash's refusal
// is what makes overrunning it visible rather than silent, so this number does not have to be
// right, only sane.
//
// Entry count is a PROXY for memory, and a poor one for the payloads: pinned decisions and
// the bookkeeping flags are tiny, a stashed payload is a whole tool output. That is why the
// reserve carries its own BYTE budget (DefaultStashMaxBytes) rather than leaving max_entries
// to mean two unrelated things — this constant now bounds only how many entries the cache
// tracks, and stash_max_bytes bounds what they cost.
const DefaultMaxEntries = 5000

// DefaultStashMaxBytes bounds the rewind reserve's memory: 256 MiB.
//
// The arithmetic it comes from, so an operator can redo it with their own numbers: the reserve
// holds at most stashCap() = max_entries/2 = 2,500 payloads, and a payload is one tool output.
// Real agent traffic in this repo's own corpus runs tens of kilobytes per output (the measured
// examples in extract_llm's comments are a 26k-token access log and a ~7k-token file read), so
// 2,500 × ~100 KB ≈ 250 MB is the entry cap's worth at the large end of ordinary. 256 MiB is
// therefore the point where the two budgets bind at roughly the same time for ordinary
// payloads — and the byte budget binds FIRST, as it should, for a workload whose outputs are
// bigger than that.
//
// It is sane rather than measured, exactly like max_entries, and for the same reason: what
// makes a wrong value safe is that overrunning it REFUSES removals and says so
// (stash_refused), instead of quietly making them irreversible.
const DefaultStashMaxBytes = 256 << 20

// NewMemory builds an in-memory store. Zero/negative option fields fall back to
// defaults (DefaultTTL, DefaultMaxEntries, 100 sessions of sticky sets).
func NewMemory(o Options) *Memory {
	ttl := time.Duration(o.TTLSeconds) * time.Second
	if o.TTLSeconds <= 0 {
		ttl = DefaultTTL
	}
	max := o.MaxEntries
	if max <= 0 {
		max = DefaultMaxEntries
	}
	stashMax := o.StashMaxBytes
	if stashMax <= 0 {
		stashMax = DefaultStashMaxBytes
	}
	stick := o.MaxSessions
	if stick <= 0 {
		stick = 100
	}
	pins := o.PinPrefixes
	if pins == nil {
		pins = DefaultPinPrefixes
	}
	return &Memory{
		ttl: ttl, max: max, maxStick: stick, pinPrefixes: pins,
		stashMaxBytes: stashMax,
		ll:            list.New(), items: map[string]*list.Element{},
		sticky:     map[string]map[string]struct{}{},
		lostFrozen: map[string]struct{}{},
		now:        time.Now,
	}
}

func (*Memory) Persists() bool { return true }

// evictableEntriesForTest counts entries the LRU may take. For TESTS only: the evictable floor
// is a property of the whole cache rather than of any one write, so asserting it needs the
// count, and a test that inferred it from "one Put survived" would pass with a floor of one.
func (m *Memory) evictableEntriesForTest() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for el := m.ll.Back(); el != nil; el = el.Prev() {
		if e := el.Value.(*entry); !e.pinned && !e.stash {
			n++
		}
	}
	return n
}

// StashedKeysForTest lists the keys the reserve currently holds. For TESTS only: a test that
// needs to act on a specific payload cannot recompute its key without duplicating the component's
// hashing, and a duplicate would pass while the real key drifted.
func (m *Memory) StashedKeysForTest() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for el := m.ll.Back(); el != nil; el = el.Prev() {
		if e := el.Value.(*entry); e.stash {
			out = append(out, e.key)
		}
	}
	return out
}

// SetClock replaces the store's time source. For TESTS only — TTL behavior over a
// multi-hour agent session is not testable in real time, and the freeze lifetime is
// exactly what this store gets wrong when it's wrong.
func (m *Memory) SetClock(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = now
}

// DisableSlidingTTLForTest restores the old write-only expiry (and un-pins frozen
// entries) so a test can measure what the previous behavior cost. For TESTS only.
func (m *Memory) DisableSlidingTTLForTest() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.noSlide = true
}

// The three entry budgets, and why there are three.
//
// pinCap and stashCap are each half the entry cap: those are the two things reversibility
// needs at once — the payload to serve, and the decision that keeps the message it came out
// of byte-stable — so neither may starve the other.
//
// evictableFloor is what stops them from adding up. Each cap alone is half, so together they
// could reach the WHOLE entry cap, and then evictOldest walks the list, finds every entry
// exempt, and returns false. A cache with nothing evictable does not fail loudly: the next
// plain Put pushes its entry to the front, the eviction loop finds that same just-inserted
// entry as the only non-exempt one and removes it, so writes to the UNPINNED namespaces
// become silent no-ops — and each of those namespaces is load-bearing:
//
//   - cg:keep: (offload.MarkKeptVerbatim) — isKeptVerbatim goes permanently false, so content
//     the agent just expanded is re-compacted, which is the expand loop the flag exists to stop.
//   - cg:sum:  (offload.saveCheckpoint) — summarize can never checkpoint, so it re-pays its
//     model call every turn and never reuses.
//   - cg:own:  (offload.recordOwner) — GET /expand refuses a key the session really does own.
//   - cg:xseen: — the economic gate misprices recurrence.
//
// Worse, that state is reached by exactly the workload this reserve was built for: a
// reversible removal writes two pinned decisions and one stash, so pin pressure and reserve
// pressure saturate together. So a quarter of the entry cap is held back from BOTH exemptions
// and stays evictable, unconditionally.
//
// Which exemption loses when the joint budget binds is whoever asks last, and each keeps its
// own existing over-cap behavior: a pin degrades to an ordinary evictable entry (it is still
// readable, and its loss is reported where losses happen), a stash is REFUSED (so the caller
// declines the removal rather than promising what it cannot deliver). No new failure shape.
func (m *Memory) pinCap() int   { return m.max / 2 }
func (m *Memory) stashCap() int { return m.max / 2 }

// evictableFloor is at least ONE entry. max/4 alone is 0 for max_entries of 2 or 3, and the
// exemptions (max/2 each) could then reach the whole cap — so the guarantee this comment and
// docs/reference/config.md both describe as unconditional had a hole at the bottom of the range.
// Only absurd configs reach it, but "unconditional" is either true or it is not.
func (m *Memory) evictableFloor() int {
	if f := m.max / 4; f > 0 {
		return f
	}
	return 1
}

// exemptRoom reports whether one more exempt entry can be admitted without eating into the
// evictable floor. Consulted by both exemptions, which is what makes the floor a floor.
func (m *Memory) exemptRoom() bool {
	return m.pinnedN+m.stashN < m.max-m.evictableFloor()
}

// StashRoom reports whether a NEW payload of size bytes would be admitted right now: a free
// entry slot under both the reserve cap and the shared exempt budget, and enough of the byte
// budget left. It reclaims what the TTL has released first, exactly as PutStash does, so the
// answer is not stale — a probe that said "full" on the strength of payloads that expired
// hours ago would skip work the store would in fact have taken.
//
// It claims nothing. See Stasher.StashRoom for why a probe exists at all.
func (m *Memory) StashRoom(size int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stashN >= m.stashCap() || !m.exemptRoom() || !m.byteRoom(size) {
		m.sweepExpired()
	}
	return m.stashN < m.stashCap() && m.exemptRoom() && m.byteRoom(size)
}

// byteRoom reports whether size more bytes fit in the reserve's byte budget. A payload larger
// than the whole budget is refused rather than admitted-and-over: the budget is the promise
// the operator sized, and one outsized output must not be the thing that breaks it.
func (m *Memory) byteRoom(size int) bool {
	return m.stashBytes+int64(size) <= m.stashMaxBytes
}

// PutStash stores a rewind payload under its marker key and reports whether it is retained.
// See Stasher for why the store cannot recognise these keys on its own, and why the boolean
// is the point.
//
// The policy, and the property it is chosen for: a live stash is NEVER evicted to make room
// for a new one. Under the old plain-Put behavior the guarantee decayed as the pipeline
// succeeded — every extra removal both consumed a slot and added a pinned decision, so more
// compaction meant more BROKEN promises. Refusing instead moves that pressure onto the
// removals not yet made: promises already outstanding stay good however long the run gets,
// and the mechanism declines new work rather than doing it irreversibly. Capacity becomes a
// configured quantity (max_entries) and exhaustion becomes a counter, in exchange for
// reversibility no longer being load-dependent.
func (m *Memory) PutStash(key string, payload []byte) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.items[key]; ok {
		e := el.Value.(*entry)
		// Byte accounting follows the payload, not the write: a refresh replaces the bytes,
		// so the reserve must be charged the difference. In practice it is zero — a stash key
		// IS the hash of its payload, so a refresh under the same key carries the same bytes —
		// but the accounting must not depend on that being true of every caller.
		if e.stash {
			m.stashBytes += int64(len(payload)) - int64(len(e.payload))
		}
		e.payload = payload
		e.expires = m.now().Add(m.ttl)
		// Claim a reserve slot if one has freed, exactly as Put re-claims a pin slot. An
		// entry already present is retained whatever the reserve says: refusing a REFRESH
		// would make a component decline to replay a marker it has already stamped, which
		// flips the message inside the provider's cached prefix — the cache-destructive
		// direction, and for content that is sitting right there. That is also why the byte
		// budget is not consulted here: a refresh is never refused, so an over-budget refresh
		// is charged honestly and the refusals it causes land on the NEXT new payload.
		if !e.stash && m.stashN < m.stashCap() && m.exemptRoom() {
			e.stash = true
			m.stashN++
			m.stashBytes += int64(len(payload))
		}
		m.ll.MoveToFront(el)
		return true
	}
	// The TTL is the only thing that releases a reserve slot now, so collect what it has
	// released before declaring the reserve full — otherwise a long-lived process refuses
	// forever on the strength of payloads that expired hours ago. One sweep, not one per
	// reclaimed entry: this is the saturated path, so it runs on every refusal.
	if m.stashN >= m.stashCap() || !m.exemptRoom() || !m.byteRoom(len(payload)) {
		m.sweepExpired()
	}
	if m.stashN >= m.stashCap() || !m.exemptRoom() || !m.byteRoom(len(payload)) {
		m.stashRefusedN++
		return false
	}
	e := &entry{key: key, payload: payload, expires: m.now().Add(m.ttl), stash: true}
	m.stashN++
	m.stashBytes += int64(len(payload))
	m.noteExpiry(e.expires)
	m.items[key] = m.ll.PushFront(e)
	for m.ll.Len() > m.max {
		if !m.evictOldest() {
			break // everything left is pinned or stashed
		}
	}
	return true
}

// StashStat is the rewind reserve's state, reported as a struct because the reserve now has
// two budgets and returning six positional values invites the caller to mix them up.
type StashStat struct {
	Live     int   // payloads held right now
	Capacity int   // the reserve's entry cap (stashCap)
	Bytes    int64 // what those payloads cost
	MaxBytes int64 // the reserve's byte budget
	// Refused counts payloads PutStash DECLINED — removals not made, rather than removals
	// made irreversibly. Expired counts payloads the TTL reclaimed, which is the only way a
	// stash leaves now that LRU pressure cannot evict one.
	Refused int64
	Expired int64
}

// StashStats reports the rewind reserve against both of its budgets.
//
// Refused is the operator-facing number, and it is deliberately upstream of
// expand_unresolved_missing: that counter only moves when the AGENT asks for content and is
// told it is gone, so a run could exhaust the reserve for an hour and read as healthy until
// something happened to request an expand. This one moves at the moment the budget binds.
//
// Reading Live/Capacity against Bytes/MaxBytes is what tells an operator WHICH budget bound,
// and so which knob to turn: entries near capacity with bytes far from the budget means raise
// max_entries; the reverse means raise stash_max_bytes (or reduce what the pipeline removes).
func (m *Memory) StashStats() StashStat {
	m.mu.Lock()
	defer m.mu.Unlock()
	return StashStat{
		Live: m.stashN, Capacity: m.stashCap(),
		Bytes: m.stashBytes, MaxBytes: m.stashMaxBytes,
		Refused: m.stashRefusedN, Expired: m.stashExpiredN,
	}
}

func (m *Memory) Put(key string, payload []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Writing a key previously recorded as lost IS the repair — the decision is present
	// again. Counted here, before either branch, so it is not missed when the entry still
	// exists (over the pin cap it stays in the map, unpinned) and so a decision that is
	// immediately unprotected again is not ALSO scored as repaired: repaired must never
	// exceed dropped, or frozen_flips reads 0 while messages are in fact flipping.
	if _, wasLost := m.lostFrozen[key]; wasLost {
		delete(m.lostFrozen, key)
		for i, k := range m.lostOrder {
			if k == key {
				m.lostOrder = append(m.lostOrder[:i], m.lostOrder[i+1:]...)
				break
			}
		}
		m.repairedN++
	}
	if el, ok := m.items[key]; ok {
		e := el.Value.(*entry)
		e.payload = payload
		e.expires = m.now().Add(m.ttl)
		// Claim a pin slot if one has since freed (an earlier session's decisions expired):
		// the cap is a live-entry budget, not a lifetime quota, so re-freezing every turn
		// eventually protects this decision instead of leaving it permanently second-class.
		if !e.pinned && m.isPinPrefix(key) && !m.noSlide && m.pinnedN < m.pinCap() && m.exemptRoom() {
			e.pinned = true
			m.pinnedN++
		}
		m.ll.MoveToFront(el)
		return
	}
	e := &entry{key: key, payload: payload, expires: m.now().Add(m.ttl)}
	// Pin frozen decisions, but never more than half the cache: past that the marginal
	// pin protects one message while starving the rewind stashes the expand loop needs.
	// Over the cap the entry is simply evictable — NOT recorded as lost: it is present and
	// readable right now, and calling it "dropped" at write time both inflates the drop
	// count with live entries and makes the very next re-freeze look like a repair. Its
	// loss, if it comes, is recorded where losses actually happen (remove).
	if m.isPinPrefix(key) && !m.noSlide && m.pinnedN < m.pinCap() && m.exemptRoom() {
		e.pinned = true
		m.pinnedN++
	}
	m.noteExpiry(e.expires)
	m.items[key] = m.ll.PushFront(e)
	for m.ll.Len() > m.max {
		if !m.evictOldest() {
			break // everything left is pinned
		}
	}
}

// noteLost records that a frozen decision under key is gone, so a later Get miss is
// distinguishable from "never frozen". Bounded by the entry cap, evicting the OLDEST mark
// first: dropping an arbitrary one let a busy session delete another session's fresh mark,
// so that session's next turn saw a plain miss and flipped its message unrepaired.
// ponytail: FIFO over one shared budget, not a per-session quota — the marks are
// session-scoped keys and short-lived (cleared by the next re-freeze), so age is a good
// enough proxy. Revisit if one session's churn is ever shown to starve another's.
func (m *Memory) noteLost(key string) {
	if _, dup := m.lostFrozen[key]; dup {
		return // already marked; don't double-count or re-queue
	}
	for len(m.lostFrozen) >= m.max && len(m.lostOrder) > 0 {
		oldest := m.lostOrder[0]
		m.lostOrder = m.lostOrder[1:]
		delete(m.lostFrozen, oldest)
	}
	m.lostFrozen[key] = struct{}{}
	m.lostOrder = append(m.lostOrder, key)
	m.lostN++
}

// FrozenLost reports whether a frozen entry under key existed and was dropped (TTL
// expiry or the pin cap) — the "was frozen, now lost" signal. See FrozenLoser.
func (m *Memory) FrozenLost(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.lostFrozen[key]
	return ok
}

// FrozenLossStats returns how many frozen decisions this store has DROPPED since start
// (TTL expiry, or eviction) and how many of those were later re-Put — restored to the
// store, so a replay can land again instead of the message flipping.
//
// Counted per DROP EVENT, not per distinct key: one key that expires, is re-frozen, and
// expires again contributes 2 drops and 1 repair. That is the intended reading — each
// event is a separate opportunity for a flip — but it means dropped−repaired is a running
// balance (marks still outstanding), not a total of distinct broken keys. A repeat drop of
// an already-marked key is not double-counted while its mark is outstanding.
func (m *Memory) FrozenLossStats() (dropped, repaired int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lostN, m.repairedN
}

func (m *Memory) Get(key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	el, ok := m.items[key]
	if !ok {
		return nil, false
	}
	e := el.Value.(*entry)
	if m.now().After(e.expires) {
		m.remove(el)
		return nil, false
	}
	// Sliding TTL: an entry still being read is still live. The TTL exists to reclaim
	// state for FINISHED sessions, not to kill a decision an ongoing session replays
	// every turn — expiring a frozen compaction mid-task flips an already-cached
	// message's representation and forces the provider to re-write the whole suffix
	// (one cache-write costs 11.5 cache-reads). Recency and lifetime refresh together.
	if !m.noSlide {
		e.expires = m.now().Add(m.ttl)
	}
	m.ll.MoveToFront(el)
	return e.payload, true
}

func (m *Memory) Sticky(session string) map[string]struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	src := m.sticky[session]
	out := make(map[string]struct{}, len(src))
	for k := range src {
		out[k] = struct{}{}
	}
	return out
}

func (m *Memory) MarkSticky(session, id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sticky[session]
	if s == nil {
		if len(m.sticky) >= m.maxStick {
			// drop an arbitrary session to stay bounded; ponytail: good enough
			// until a real eviction policy is warranted.
			for k := range m.sticky {
				delete(m.sticky, k)
				break
			}
		}
		s = map[string]struct{}{}
		m.sticky[session] = s
	}
	s[id] = struct{}{}
}

// noteExpiry keeps nextExpiry a valid LOWER BOUND on the earliest expires in the store. Every
// write sets its entry's expiry to now+ttl — the latest of any live entry — so the bound only
// ever needs seeding, never raising: the first write after a sweep supplies it, and a sweep
// recomputes it exactly.
func (m *Memory) noteExpiry(t time.Time) {
	if m.nextExpiry.IsZero() || t.Before(m.nextExpiry) {
		m.nextExpiry = t
	}
}

// sweepExpired removes EVERY already-expired entry, pinned and stashed included, and reports
// how many it took. Without it an exempt entry is immortal — the TTL is otherwise only
// enforced in Get, and a dead session is never read again — so pinnedN/stashN would ratchet
// to their caps and stay there, leaking the reserve and silently disabling the exemption for
// every later session.
//
// One pass over the list, and skipped outright when nextExpiry proves nothing can have
// expired. The predecessor reclaimed a single entry per call, so a saturated reserve walked
// the whole list under the global mutex on every refused PutStash, and again per entry when
// several had expired.
func (m *Memory) sweepExpired() int {
	now := m.now()
	if !m.nextExpiry.IsZero() && !now.After(m.nextExpiry) {
		return 0 // the earliest expiry is still in the future; nothing to find
	}
	n := 0
	var soonest time.Time
	for el := m.ll.Back(); el != nil; {
		prev := el.Prev()
		e := el.Value.(*entry)
		if now.After(e.expires) {
			m.remove(el)
			n++
		} else if soonest.IsZero() || e.expires.Before(soonest) {
			soonest = e.expires
		}
		el = prev
	}
	// Recomputed from what survived, so the bound is exact here and only decays (safely,
	// downward) as later writes seed it.
	m.nextExpiry = soonest
	return n
}

// evictOldest drops the least-recently-used entry that is neither pinned nor stashed,
// walking back over the exempt ones. Reports false when nothing is evictable.
func (m *Memory) evictOldest() bool {
	if m.sweepExpired() > 0 {
		return true
	}
	// Nothing expired, so take the LRU entry that is not exempt. Stashes are exempt for a
	// DIFFERENT reason from pins: a pin protects the bytes of an already-cached message, a
	// stash protects a promise this proxy made to the model in a marker it has already sent.
	// Evicting one here is what #187 was — the cheap decision record survived, the payload it
	// pointed at did not, and the removal became irreversible with nothing reported.
	for el := m.ll.Back(); el != nil; el = el.Prev() {
		if e := el.Value.(*entry); !e.pinned && !e.stash {
			m.remove(el)
			return true
		}
	}
	return false
}

func (m *Memory) remove(el *list.Element) {
	e := el.Value.(*entry)
	if e.pinned {
		m.pinnedN--
	}
	if e.stash {
		m.stashN--
		m.stashBytes -= int64(len(e.payload))
		// A stash only reaches here via sweepExpired (LRU pressure cannot take one), so
		// this counts TTL reclamation. Counted rather than silent because it is the one
		// remaining way an outstanding marker can stop resolving, and an operator seeing
		// expand_unresolved_missing needs to know whether the answer is "raise max_entries"
		// (refused) or "raise ttl_seconds" (expired).
		m.stashExpiredN++
	}
	// Any replay decision disappearing must be detectable — keyed on the NAMESPACE, not on
	// the pin flag. An entry that missed the pin cap is exactly the one most likely to be
	// dropped, and gating this on e.pinned would let it vanish silently: unreported, and so
	// never repaired. (noSlide reproduces the OLD store, which had no loss signal at all.)
	if m.isPinPrefix(e.key) && !m.noSlide {
		m.noteLost(e.key)
	}
	m.ll.Remove(el)
	delete(m.items, e.key)
}
