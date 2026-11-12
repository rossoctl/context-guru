package proxy

import (
	"sync"
	"time"

	"github.com/rossoctl/accountant"
)

// siblingsMax bounds siblingStore by session, and siblingThreadsMax by thread within a session. A
// session past the thread cap drops its oldest thread: a sibling that old is past any TTL anyway.
const (
	siblingsMax       = 4096
	siblingThreadsMax = 64
)

// siblingStore remembers, per session, the last forwarded body each thread sent and when. It gives
// EntryFacts.SharedWarm: the prefix of a request's forwarded body that another thread of the session
// kept warm inside the TTL, which a keep-alive rescue must not credit (accountant R54). It holds the
// library's Body (hashes and sizes only, R2) and the breakpoints, never content.
//
// The store lock is held only to find or create a session; each session has its own lock, so
// requests of different sessions do not wait on each other's scans.
type siblingStore struct {
	mu   sync.Mutex
	m    map[string]*siblingSession
	tick uint64 // a use counter, for least-recently-used eviction of sessions
}

type siblingSession struct {
	mu      sync.Mutex
	used    uint64 // the store's tick at the last use; read and written under the store lock
	threads map[string]siblingEntry
}

type siblingEntry struct {
	at  time.Time
	fwd accountant.Body
	bps []accountant.Breakpoint
}

func newSiblingStore() *siblingStore { return &siblingStore{m: map[string]*siblingSession{}} }

// session returns the session's entry, creating it; at the cap it drops the least recently used
// session, which only loses that session's sibling facts.
func (s *siblingStore) session(id string) *siblingSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tick++
	if ss, ok := s.m[id]; ok {
		ss.used = s.tick
		return ss
	}
	if len(s.m) >= siblingsMax {
		oldest, at := "", uint64(0)
		for k, v := range s.m {
			if oldest == "" || v.used < at {
				oldest, at = k, v.used
			}
		}
		delete(s.m, oldest)
	}
	ss := &siblingSession{used: s.tick, threads: map[string]siblingEntry{}}
	s.m[id] = ss
	return ss
}

// sharedWarm returns the last position of fwd that a sibling of thread kept warm inside ttl before at,
// then records this request as the thread's last. The host chooses the siblings: another thread of the
// session, last seen inside the TTL and not after this request. The library decides where the shared
// prefix ends (accountant.SharedWarmFrom, R54). Nil when no sibling did.
func (s *siblingStore) sharedWarm(session, thread string, at time.Time, ttl time.Duration,
	fwd accountant.Body, bps []accountant.Breakpoint) *accountant.Position {
	if s == nil || session == "" {
		return nil
	}
	ss := s.session(session)
	ss.mu.Lock()
	defer ss.mu.Unlock()
	var sibs []accountant.Sibling
	for id, e := range ss.threads {
		if id == thread || at.Sub(e.at) > ttl || e.at.After(at) {
			continue
		}
		sibs = append(sibs, accountant.Sibling{Body: e.fwd, Breakpoints: e.bps})
	}
	if _, ok := ss.threads[thread]; !ok && len(ss.threads) >= siblingThreadsMax {
		oldest := ""
		for id, e := range ss.threads {
			if oldest == "" || e.at.Before(ss.threads[oldest].at) {
				oldest = id
			}
		}
		delete(ss.threads, oldest)
	}
	ss.threads[thread] = siblingEntry{at: at, fwd: fwd, bps: bps}
	return accountant.SharedWarmFrom(fwd, sibs)
}
