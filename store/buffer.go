package store

import "sync"

// Buffer is a copy-on-write overlay over another Store: reads fall through to the
// base, writes are held locally until Commit flushes them (or are thrown away if
// Commit is never called).
//
// It exists for async mode (#31). An off-path compaction writes frozen decisions,
// stashed originals and sticky ids as it runs, so "discard a stale result" cannot be
// done after the fact — by then the writes have landed. Running the deferred job
// against a Buffer makes the whole result a single atomic, discardable unit: the
// worker re-checks the session's compaction generation and calls Commit only if no
// newer turn has superseded the snapshot the job was built from.
//
// Safe for concurrent use, like every Store.
type Buffer struct {
	Base Store

	mu     sync.Mutex
	writes map[string][]byte
	order  []string // Commit replays in write order so a later Put wins
	sticky map[string][]string
}

// NewBuffer wraps base. A nil base behaves like Nop.
func NewBuffer(base Store) *Buffer {
	if base == nil {
		base = Nop{}
	}
	return &Buffer{Base: base}
}

func (b *Buffer) Put(key string, payload []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.writes == nil {
		b.writes = map[string][]byte{}
	}
	if _, seen := b.writes[key]; !seen {
		b.order = append(b.order, key)
	}
	b.writes[key] = payload
}

func (b *Buffer) Get(key string) ([]byte, bool) {
	b.mu.Lock()
	if v, ok := b.writes[key]; ok {
		b.mu.Unlock()
		return v, true
	}
	b.mu.Unlock()
	return b.Base.Get(key)
}

func (b *Buffer) Sticky(session string) map[string]struct{} {
	out := b.Base.Sticky(session)
	if out == nil {
		out = map[string]struct{}{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, id := range b.sticky[session] {
		out[id] = struct{}{}
	}
	return out
}

func (b *Buffer) MarkSticky(session, id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sticky == nil {
		b.sticky = map[string][]string{}
	}
	b.sticky[session] = append(b.sticky[session], id)
}

// Persists mirrors the base: a component decides whether an offload can be made
// reversible from this, and the answer must be the base store's answer because that
// is where the stash ends up after Commit.
func (b *Buffer) Persists() bool { return b.Base.Persists() }

// Commit flushes every buffered write into the base store and empties the buffer.
// Not calling it discards the whole result.
func (b *Buffer) Commit() {
	b.mu.Lock()
	writes, order, sticky := b.writes, b.order, b.sticky
	b.writes, b.order, b.sticky = nil, nil, nil
	b.mu.Unlock()
	for _, k := range order {
		b.Base.Put(k, writes[k])
	}
	for s, ids := range sticky {
		for _, id := range ids {
			b.Base.MarkSticky(s, id)
		}
	}
}

// Writes reports how many distinct keys are buffered (test/telemetry aid).
func (b *Buffer) Writes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.writes)
}

// FrozenLost forwards the optional FrozenLoser capability (#40) to the base store, so
// wrapping a store in a Buffer does not silently disable it. A key the buffer itself
// holds is present, not lost, whatever the base thinks.
//
// Type-asserting on the wrapper is why this is needed: a component checks
// `c.Store.(store.FrozenLoser)`, and without this method an off-path async run — which
// always sees a Buffer — would take the degraded path and re-derive nothing, exactly the
// case #40 added the signal for.
func (b *Buffer) FrozenLost(key string) bool {
	b.mu.Lock()
	_, held := b.writes[key]
	b.mu.Unlock()
	if held {
		return false
	}
	// Asserted on a locally-declared shape rather than store.FrozenLoser, because that
	// type arrives with #40 and this branch must compile before it. Structural matching
	// means it binds to the real interface the moment #40 lands, with no edit here.
	fl, ok := b.Base.(interface{ FrozenLost(string) bool })
	return ok && fl.FrozenLost(key)
}
