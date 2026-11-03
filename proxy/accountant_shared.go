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
// then records this request as the thread's last. A prefix counts as warm up to the last breakpoint
// the sibling sent inside the prefix both bodies share: the provider caches at breakpoints, so a
// shared prefix with no breakpoint of the sibling's in it was not kept warm. Nil when no sibling did.
func (s *siblingStore) sharedWarm(session, thread string, at time.Time, ttl time.Duration,
	fwd accountant.Body, bps []accountant.Breakpoint) *accountant.Position {
	if s == nil || session == "" {
		return nil
	}
	ss := s.session(session)
	ss.mu.Lock()
	defer ss.mu.Unlock()
	best := -1
	for id, e := range ss.threads {
		if id == thread || at.Sub(e.at) > ttl || e.at.After(at) {
			continue
		}
		common := commonPrefix(e.fwd, fwd)
		for _, b := range bps2flat(e.fwd, e.bps) {
			if b <= common && b > best {
				best = b
			}
		}
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
	if best < 0 {
		return nil
	}
	p := positionOf(fwd, best)
	return &p
}

// commonPrefix is the last index at which a and b hold the same prefix, or -1. The hashes are
// cumulative, so equal hashes at an index mean equal content up to it.
func commonPrefix(a, b accountant.Body) int {
	i := -1
	for i+1 < len(a.Pos) && i+1 < len(b.Pos) && a.Pos[i+1].Hash == b.Pos[i+1].Hash {
		i++
	}
	return i
}

// bps2flat maps breakpoints to flat indexes of b.
func bps2flat(b accountant.Body, bps []accountant.Breakpoint) []int {
	var out []int
	for _, bp := range bps {
		if i, ok := flatOf(b, bp.Pos); ok {
			out = append(out, i)
		}
	}
	return out
}

var sectionOrder = [3]accountant.Section{accountant.SectionTools, accountant.SectionSystem, accountant.SectionMessages}

func flatOf(b accountant.Body, p accountant.Position) (int, bool) {
	for s, sec := range sectionOrder {
		if sec != p.Section {
			continue
		}
		end := len(b.Pos)
		if s < 2 {
			end = b.Start[s+1]
		}
		i := b.Start[s] + p.Index
		return i, p.Index >= 0 && i < end
	}
	return 0, false
}

func positionOf(b accountant.Body, i int) accountant.Position {
	s := 2
	for s > 0 && i < b.Start[s] {
		s--
	}
	return accountant.Position{Section: sectionOrder[s], Index: i - b.Start[s]}
}
