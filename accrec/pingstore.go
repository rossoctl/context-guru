package accrec

import (
	"sync"
	"sync/atomic"

	"github.com/rossoctl/accountant"
)

// PingStoreMax bounds PingStore. It is keyed by session and thread, so without a bound a
// long-running proxy would hold one entry per thread that was ever pinged and never asked again.
const PingStoreMax = 4096

// PingStore remembers, per session and thread, the facts of the latest keep-alive ping until the
// thread's next real request takes them. A host gives the same instance to its ping emitter (writer)
// and to its request path (reader). A nil *PingStore is a no-op.
type PingStore struct {
	mu sync.Mutex
	m  map[string]accountant.PingFacts
	// priced counts the pings PingSavings priced (it returned ok), manual ones too.
	priced atomic.Int64
}

// NewPingStore returns an empty store holding at most PingStoreMax threads.
func NewPingStore() *PingStore { return &PingStore{m: map[string]accountant.PingFacts{}} }

// NotePriced counts one priced ping. Nil-safe.
func (s *PingStore) NotePriced() {
	if s != nil {
		s.priced.Add(1)
	}
}

// Priced is how many pings PingSavings priced. Nil-safe: a host with no store reads 0.
func (s *PingStore) Priced() int64 {
	if s == nil {
		return 0
	}
	return s.priced.Load()
}

// Put keeps the latest facts for key; a second ping overwrites the first. When the store is full
// and key is new, an arbitrary other entry is evicted (map order), which only costs the evicted
// thread its LastPing gap on its next request.
func (s *PingStore) Put(key string, f accountant.PingFacts) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[key]; !ok && len(s.m) >= PingStoreMax {
		for k := range s.m {
			delete(s.m, k)
			break
		}
	}
	s.m[key] = f
}

// Peek returns the facts for key and keeps them. A request reads them before it is sent; it takes
// them (Drop) only once it is priced, so a request that books nothing leaves them for the next one.
func (s *PingStore) Peek(key string) (accountant.PingFacts, bool) {
	if s == nil {
		return accountant.PingFacts{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.m[key]
	return f, ok
}

// Take is Peek and Drop in one step.
func (s *PingStore) Take(key string) (accountant.PingFacts, bool) {
	f, ok := s.Peek(key)
	s.Drop(key, f.PingWindow)
	return f, ok
}

// Drop deletes the facts for key: they belong to the next priced request only. window is the
// PingWindow the request read; facts with another window are a newer ping, put after the request
// read, and stay for the next request. An empty window drops whatever is there.
func (s *PingStore) Drop(key, window string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if f, ok := s.m[key]; ok && (window == "" || f.PingWindow == window) {
		delete(s.m, key)
	}
}

// Len is how many threads the store holds. Nil-safe.
func (s *PingStore) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}
