package proxy

import (
	"sync"
	"time"

	"github.com/rossoctl/context-guru/components"
	cf "github.com/rossoctl/context-guru/counterfactual"
)

// pingStoreMax bounds PingStore. It is keyed by session and thread, so without a bound a
// long-running proxy would hold one entry per thread that was ever pinged and never asked again.
const pingStoreMax = 4096

// PingStore remembers, per session and thread, the facts of the latest keep-alive ping until the
// thread's next real request takes them. The same instance is given to the ping emitter (writer)
// and to the Handler (reader) through Options.Pings. A nil *PingStore is a no-op.
type PingStore struct {
	mu sync.Mutex
	m  map[string]cf.PingFacts
}

// NewPingStore returns an empty store holding at most pingStoreMax threads.
func NewPingStore() *PingStore { return &PingStore{m: map[string]cf.PingFacts{}} }

// put keeps the latest facts for key; a second ping overwrites the first. When the store is full
// and key is new, an arbitrary other entry is evicted (map order), which only costs the evicted
// thread its LastPing gap on its next request.
func (s *PingStore) put(key string, f cf.PingFacts) {
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

// take returns the facts for key and deletes them: they belong to the next real request only.
func (s *PingStore) take(key string) (cf.PingFacts, bool) {
	if s == nil {
		return cf.PingFacts{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.m[key]
	delete(s.m, key)
	return f, ok
}

// pingFromReport maps a keeper report to the library's Ping. r.CostUSD is deliberately not
// carried: the host prices the ping with its own rates.
func pingFromReport(r components.KeepAliveReport) cf.Ping {
	return cf.Ping{
		Session: r.Session, ThreadID: r.Thread, Model: r.Model, Host: r.Route,
		Usage: cf.CallUsage{Usage: cf.Usage{Input: int(r.FreshInput), CacheRead: int(r.CacheRead),
			CacheWrite: int(r.CacheWrite), CacheWrite1h: int(r.CacheWrite1h)}, Output: int(r.Output)},
		At: time.UnixMilli(r.TS), Window: r.Window,
	}
}

// PingSavings prices one keep-alive ping as a spend operation and stores its facts in st for the
// thread's next real request. ok is false, and nothing is stored, for a ping that is not
// converted: no Window (its ChangeID would be shared by every such ping), a status outside
// 200-299, or no usage (it billed nothing). Manual and sweep-substitute pings are converted.
func PingSavings(r components.KeepAliveReport, st *PingStore) (cf.Result, bool) {
	if r.Window.IsZero() || r.Status < 200 || r.Status > 299 ||
		r.FreshInput+r.CacheRead+r.CacheWrite+r.CacheWrite1h+r.Output == 0 {
		return cf.Result{}, false
	}
	ops, facts := cf.PingOps(pingFromReport(r), cf.BillingFrom(cf.BillingAPI))
	res := cf.ComputeStandalone(ops)
	st.put(cf.ThreadKey(r.Session, cf.Thread{ID: cfThreadID(r.Thread)}), facts)
	return res, true
}
