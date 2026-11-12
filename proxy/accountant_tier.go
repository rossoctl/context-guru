package proxy

import "sync"

// tierStoreMax bounds tierStore. Past it the store starts over: a thread that lost its entry meets
// the asked tier on its next read-only turn, which is the behavior before the entry existed.
const tierStoreMax = 8192

// tierStore remembers, per thread, whether its last write was billed at the one-hour tier. It is
// keyed like the ping store (accPingKey) and holds one bool per thread: no content.
type tierStore struct {
	mu sync.Mutex
	m  map[string]bool
}

func newTierStore() *tierStore { return &tierStore{m: map[string]bool{}} }

// get returns the remembered tier and whether the thread has one. Nil-safe.
func (s *tierStore) get(key string) (oneHour, ok bool) {
	if s == nil {
		return false, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	oneHour, ok = s.m[key]
	return
}

// set remembers the tier. Nil-safe.
func (s *tierStore) set(key string, oneHour bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[key]; !ok && len(s.m) >= tierStoreMax {
		s.m = map[string]bool{}
	}
	s.m[key] = oneHour
}
