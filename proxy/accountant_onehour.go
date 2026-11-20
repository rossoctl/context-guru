package proxy

import (
	"strconv"
	"sync"
	"time"

	"github.com/rossoctl/accountant"
)

// The rescue of a 1-hour-on-wait ping (#1243).
//
// A 1-hour ping moves the entry of a 5-minute thread to 1 hour. When the thread returns more than 5
// minutes after its last real request, its own 5-minute request READS that entry. Without the ping it
// would have rebuilt the prefix at 1.25x. That is a saving, and the ping's own read is the cost (booked
// as any ping).
//
// The library cannot see it. It judges the entry by the TTL of the breakpoints of the previous REAL
// request (5 minutes) and counts a ping only when the return is within that TTL of the ping, so a read
// 20 minutes after a 1-hour ping looks like an ordinary hit and books no rescue (the gap
// TestRelabelOfAnEntryAlreadyCachedAt5mBooksNoPremium found). The host knows what the library does not: the
// ping asked 1 hour and the gateway read it. So the host books the rescue itself, as the library books
// its own: Saved is the tokens written at 5m that were not, Caused is the same tokens read.

// oneHourPing is what the proxy keeps of a 1-hour ping until the thread's next real request is priced.
type oneHourPing struct {
	window string    // the ping's keep-alive window, the ChangeID the ping's cost is booked on
	at     time.Time // when the ping started
	read   int       // tokens the ping read
}

// ohMax bounds the two small stores below, like pingStoreMax.
const ohMax = 4096

// putOneHour keeps the latest 1-hour ping of a thread. Nil-safe.
func (s *PingStore) putOneHour(key string, p oneHourPing) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.oh == nil || len(s.oh) >= ohMax {
		s.oh = map[string]oneHourPing{}
	}
	s.oh[key] = p
}

// peekOneHour returns the thread's 1-hour ping and keeps it. Nil-safe.
func (s *PingStore) peekOneHour(key string) (oneHourPing, bool) {
	if s == nil {
		return oneHourPing{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.oh[key]
	return p, ok
}

// startStore remembers when each thread's last real request started: the time and nothing else.
type startStore struct {
	mu sync.Mutex
	m  map[string]time.Time
}

func newStartStore() *startStore { return &startStore{m: map[string]time.Time{}} }

// swap stores at as the thread's last start and returns the one before. Nil-safe.
func (s *startStore) swap(key string, at time.Time) (prev time.Time, ok bool) {
	if s == nil {
		return time.Time{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok = s.m[key]
	if !ok && len(s.m) >= ohMax {
		s.m = map[string]time.Time{}
	}
	s.m[key] = at
	return prev, ok
}

// accOneHourRescue books the rescue of a 1-hour ping for this request (see oneHourRescue). It also notes
// this request's start for the thread's next return. A request with no ping facts changes nothing.
func (c *capture) accOneHourRescue(res accountant.Result, usage Usage) accountant.Result {
	key := accPingKey(c.accMeta.Session, c.accThread)
	prev, had := c.savings.starts.swap(key, c.accMeta.At)
	ping, ok := c.pings.peekOneHour(key)
	if !ok {
		return res
	}
	return oneHourRescue(res, ping, prev, had, c.accMeta.At, int(usage.CacheRead))
}

// oneHourRescue adds the 1-hour ping's rescue to res and returns it. It books nothing unless all hold:
//   - the thread's last ping was a 1-hour ping, started inside the hour before this request;
//   - this request started MORE than 5 minutes after the thread's previous real request, so a 5-minute
//     entry would be gone without the ping (a return inside 5 minutes is hot either way: no rescue);
//   - this request read cache;
//   - the library booked no rescue of its own for this ping window.
//
// The tokens are what both the ping and this request read, whichever is smaller.
func oneHourRescue(res accountant.Result, ping oneHourPing, prevStart time.Time, hadPrev bool, at time.Time, cacheRead int) accountant.Result {
	if !hadPrev || cacheRead <= 0 || ping.read <= 0 || at.Sub(ping.at) > time.Hour || at.Sub(prevStart) <= 5*time.Minute {
		return res
	}
	for _, e := range res.CacheEffects {
		if e.Kind == accountant.EffectRescue {
			return res
		}
	}
	n := min(ping.read, cacheRead)
	e := accountant.CacheEffect{ID: "ce:" + strconv.Itoa(len(res.CacheEffects)+1), Kind: accountant.EffectRescue,
		ChangeID: ping.window, Estimated: true}
	e.Saved.CacheWrite5m = n
	e.Caused.CacheRead = n
	res.CacheEffects = append(res.CacheEffects, e)
	return res
}
