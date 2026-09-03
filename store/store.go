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
	pinnedN     int              // live pinned (frozen) entries, capped at max/2
	stashN      int              // live rewind payloads, capped at stashCap()
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
	MaxSessions int      `yaml:"max_sessions"`
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
// Entry count is a PROXY for memory, and a poor one for this namespace: pinned decisions and
// the bookkeeping flags are tiny, a stashed payload is a whole tool output. An operator
// running large payloads should set max_entries against bytes they can afford, not entries.
const DefaultMaxEntries = 5000

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
		ll: list.New(), items: map[string]*list.Element{},
		sticky:     map[string]map[string]struct{}{},
		lostFrozen: map[string]struct{}{},
		now:        time.Now,
	}
}

func (*Memory) Persists() bool { return true }

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

// stashCap bounds the rewind reserve at half the entry cap, the same share the pins get.
// Half and not more because the two halves are the two things reversibility needs at once:
// the payload to serve, and the decision that keeps the message it came out of byte-stable.
func (m *Memory) stashCap() int { return m.max / 2 }

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
		e.payload = payload
		e.expires = m.now().Add(m.ttl)
		// Claim a reserve slot if one has freed, exactly as Put re-claims a pin slot. An
		// entry already present is retained whatever the reserve says: refusing a REFRESH
		// would make a component decline to replay a marker it has already stamped, which
		// flips the message inside the provider's cached prefix — the cache-destructive
		// direction, and for content that is sitting right there.
		if !e.stash && m.stashN < m.stashCap() {
			e.stash = true
			m.stashN++
		}
		m.ll.MoveToFront(el)
		return true
	}
	// The TTL is the only thing that releases a reserve slot now, so collect what it has
	// released before declaring the reserve full — otherwise a long-lived process refuses
	// forever on the strength of payloads that expired hours ago.
	for m.stashN >= m.stashCap() && m.reclaimExpired() {
	}
	if m.stashN >= m.stashCap() {
		m.stashRefusedN++
		return false
	}
	e := &entry{key: key, payload: payload, expires: m.now().Add(m.ttl), stash: true}
	m.stashN++
	m.items[key] = m.ll.PushFront(e)
	for m.ll.Len() > m.max {
		if !m.evictOldest() {
			break // everything left is pinned or stashed
		}
	}
	return true
}

// StashStats reports the rewind reserve: payloads live now, the reserve's size, how many
// payloads PutStash REFUSED (removals declined, not removals made irreversibly), and how many
// the TTL reclaimed.
//
// refused is the operator-facing number, and it is deliberately upstream of
// expand_unresolved_missing: that counter only moves when the AGENT asks for content and is
// told it is gone, so a run could exhaust the reserve for an hour and read as healthy until
// something happened to request an expand. This one moves at the moment the budget binds.
func (m *Memory) StashStats() (live, capacity int, refused, expired int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stashN, m.stashCap(), m.stashRefusedN, m.stashExpiredN
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
		if !e.pinned && m.isPinPrefix(key) && !m.noSlide && m.pinnedN < m.max/2 {
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
	if m.isPinPrefix(key) && !m.noSlide && m.pinnedN < m.max/2 {
		e.pinned = true
		m.pinnedN++
	}
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

// reclaimExpired removes one already-EXPIRED entry, pinned or stashed included, and reports
// whether it found one. Without it an exempt entry is immortal — the TTL is otherwise only
// enforced in Get, and a dead session is never read again — so pinnedN/stashN would ratchet
// to their caps and stay there, leaking the reserve and silently disabling the exemption for
// every later session.
func (m *Memory) reclaimExpired() bool {
	now := m.now()
	for el := m.ll.Back(); el != nil; {
		prev := el.Prev()
		if e := el.Value.(*entry); now.After(e.expires) {
			m.remove(el)
			return true
		}
		el = prev
	}
	return false
}

// evictOldest drops the least-recently-used entry that is neither pinned nor stashed,
// walking back over the exempt ones. Reports false when nothing is evictable.
func (m *Memory) evictOldest() bool {
	if m.reclaimExpired() {
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
		// A stash only reaches here via reclaimExpired (LRU pressure cannot take one), so
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
