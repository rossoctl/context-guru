package proxy

import (
	"log/slog"
	"sync"
	"time"

	"github.com/rossoctl/accountant"
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
}

// NewPingStore returns an empty store holding at most pingStoreMax threads.
func NewPingStore() *PingStore { return &PingStore{m: map[string]accountant.PingFacts{}} }

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

// pingFromReport maps a keeper report to the library's Ping. r.CostUSD is deliberately not
// carried: the host prices the ping with its own rates.
func pingFromReport(r components.KeepAliveReport) accountant.Ping {
	return accountant.Ping{
		Session: r.Session, ThreadID: r.Thread, Model: r.Model, Host: r.Route,
		Usage: accountant.CallUsage{Usage: accountant.Usage{Input: int(r.FreshInput), CacheRead: int(r.CacheRead),
			CacheWrite: int(r.CacheWrite), CacheWrite1h: int(r.CacheWrite1h)}, Output: int(r.Output)},
		At: pingStart(r), Window: r.Window,
	}
}

// pingStart is when the ping began. The report's TS is when it was recorded, after the response, and the library
// measures the entry gap from the START of a request or ping, as the provider measures the TTL (accountant design
// §5.2; PingFacts.At is "when the ping started"). A ping that streams for minutes would otherwise shorten the gap.
func pingStart(r components.KeepAliveReport) time.Time {
	return time.UnixMilli(r.TS).Add(-time.Duration(r.DurationMs * float64(time.Millisecond)))
}

// PingSavings prices one keep-alive ping as a spend operation and stores its facts in st for the
// thread's next real request. ok is false, and nothing is stored, for a ping that is not
// converted: no Window (its ChangeID would be shared by every such ping), a status outside
// 200-299, or no usage (it billed nothing). Manual and sweep-substitute pings are converted.
//
// A MANUAL ping is priced, so its cost is on its row, but its facts are not stored. The write it avoided
// is already booked on its own row as ManualSavedUSD, from the user's estimate. Storing the facts too
// would let the next real request credit the same write again as a library rescue, and the total would
// count it twice (context-guru#768 B3: $0.0200 booked, about $0.0096 true).
//
// A panic in the library is recovered and logged, and the ping is reported as not converted. The keeper
// runs this in its own goroutine, so an unrecovered panic would end the process (issue #918).
func PingSavings(r components.KeepAliveReport, st *PingStore) (res accountant.Result, ok bool) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("accountant: ping pricing panicked; the ping is recorded with no saving", "panic", p)
			res, ok = accountant.Result{}, false
		}
	}()
	if r.Window.IsZero() || r.Status < 200 || r.Status > 299 ||
		r.FreshInput+r.CacheRead+r.CacheWrite+r.CacheWrite1h+r.Output == 0 {
		return accountant.Result{}, false
	}
	ops, facts := pingOps(pingFromReport(r), accountant.Billing{Mode: accountant.BillingAPI})
	res = accountantStandalone(ops)
	if !r.Manual {
		st.put(accPingKey(r.Session, accThreadID(r.Thread)), facts)
	}
	return res, true
}

// pingOps and accountantStandalone are the two library calls PingSavings makes. Tests replace them to make them panic.
var (
	pingOps              = accountant.PingOps
	accountantStandalone = accountant.ComputeStandalone
)
