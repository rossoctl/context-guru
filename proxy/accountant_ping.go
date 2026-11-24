package proxy

import (
	"sync"
	"sync/atomic"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/accrec"
	"github.com/rossoctl/context-guru/components"
)

// pingStoreMax bounds PingStore. It is keyed by session and thread, so without a bound a
// long-running proxy would hold one entry per thread that was ever pinged and never asked again.
const pingStoreMax = 4096

// PingStore remembers, per session and thread, the facts of the latest keep-alive ping until the
// thread's next real request takes them. The same instance is given to the ping emitter (writer)
// and to the Handler (reader) through Options.Pings. A nil *PingStore is a no-op.
type PingStore struct {
	mu sync.Mutex
	m  map[string]accountant.PingFacts
	// priced counts the pings PingSavings priced (it returned ok), manual ones too.
	priced atomic.Int64
}

// NewPingStore returns an empty store holding at most pingStoreMax threads.
func NewPingStore() *PingStore { return &PingStore{m: map[string]accountant.PingFacts{}} }

// notePriced counts one priced ping. Nil-safe.
func (s *PingStore) notePriced() {
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

// put keeps the latest facts for key; a second ping overwrites the first. When the store is full
// and key is new, an arbitrary other entry is evicted (map order), which only costs the evicted
// thread its LastPing gap on its next request.
func (s *PingStore) put(key string, f accountant.PingFacts) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[key]; !ok && len(s.m) >= pingStoreMax {
		for k := range s.m {
			delete(s.m, k)
			break
		}
	}
	s.m[key] = f
}

// peek returns the facts for key and keeps them. A request reads them before it is sent; it takes
// them (drop) only once it is priced, so a request that books nothing leaves them for the next one.
func (s *PingStore) peek(key string) (accountant.PingFacts, bool) {
	if s == nil {
		return accountant.PingFacts{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.m[key]
	return f, ok
}

// take is peek and drop in one step.
func (s *PingStore) take(key string) (accountant.PingFacts, bool) {
	f, ok := s.peek(key)
	s.drop(key, f.PingWindow)
	return f, ok
}

// drop deletes the facts for key: they belong to the next priced request only. window is the
// PingWindow the request read; facts with another window are a newer ping, put after the request
// read, and stay for the next request. An empty window drops whatever is there.
func (s *PingStore) drop(key, window string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if f, ok := s.m[key]; ok && (window == "" || f.PingWindow == window) {
		delete(s.m, key)
	}
}

// pingSink lets accrec.PingSavings use the store. A nil store is a no-op, as the store's methods are.
type pingSink struct{ s *PingStore }

func (p pingSink) Put(key string, f accountant.PingFacts) { p.s.put(key, f) }
func (p pingSink) NotePriced()                            { p.s.notePriced() }

// Peek lets accrec.EntryFacts read the store.
func (s *PingStore) Peek(key string) (accountant.PingFacts, bool) { return s.peek(key) }

// PingSavings prices one keep-alive ping and stores its facts in st. The rules are accrec.PingSavings's.
func PingSavings(r components.KeepAliveReport, st *PingStore) (accountant.Result, bool) {
	return accrec.PingSavings(r, pingSink{st}, accrec.PingCalls{Ops: pingOps, Standalone: accountantStandalone})
}

// pingOps and accountantStandalone are the two library calls PingSavings makes. Tests replace them to make them panic.
var (
	pingOps              = accountant.PingOps
	accountantStandalone = accountant.ComputeStandalone
)
