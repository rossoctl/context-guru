package proxy

import (
	"sync"
	"time"

	"github.com/rossoctl/accountant"
)

// siblingsMax bounds siblingStore by session. A session keeps one entry per thread, and the proxy
// sees few threads per session, so the bound is on sessions.
const siblingsMax = 4096

// siblingStore remembers, per session, the last forwarded body each thread sent and when. It gives
// EntryFacts.SharedWarm: the prefix of a request's forwarded body that another thread of the session
// kept warm inside the TTL, which a keep-alive rescue must not credit (accountant R54). It holds the
// library's Body (hashes and sizes only, R2) and the breakpoints, never content.
type siblingStore struct {
	mu sync.Mutex
	m  map[string]map[string]siblingEntry // session -> thread -> last request
}

type siblingEntry struct {
	at  time.Time
	fwd accountant.Body
	bps []accountant.Breakpoint
}

func newSiblingStore() *siblingStore { return &siblingStore{m: map[string]map[string]siblingEntry{}} }

// sharedWarm returns the last position of fwd that a sibling of thread kept warm inside ttl before at,
// then records this request as the thread's last. A prefix counts as warm up to the last breakpoint
// the sibling sent inside the prefix both bodies share: the provider caches at breakpoints, so a
// shared prefix with no breakpoint of the sibling's in it was not kept warm. Nil when no sibling did.
func (s *siblingStore) sharedWarm(session, thread string, at time.Time, ttl time.Duration,
	fwd accountant.Body, bps []accountant.Breakpoint) *accountant.Position {
	if s == nil || session == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	threads := s.m[session]
	best := -1
	for id, e := range threads {
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
	if threads == nil {
		if len(s.m) >= siblingsMax {
			for k := range s.m { // an arbitrary session: it only loses its sibling facts
				delete(s.m, k)
				break
			}
		}
		threads = map[string]siblingEntry{}
		s.m[session] = threads
	}
	threads[thread] = siblingEntry{at: at, fwd: fwd, bps: bps}
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
