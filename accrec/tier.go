package accrec

import "sync"

// tierStoreMax bounds TierStore. Past it the store starts over: a thread that lost its entry meets
// the asked tier on its next read-only turn, which is the behavior before the entry existed.
const tierStoreMax = 8192

// TierStore remembers, per thread, whether its last write was billed at the one-hour tier. It is
// keyed like the ping store (accPingKey) and holds one bool per thread: no content.
type TierStore struct {
	mu sync.Mutex
	m  map[string]bool
}

func NewTierStore() *TierStore { return &TierStore{m: map[string]bool{}} }

// get returns the remembered tier and whether the thread has one. Nil-safe.
func (s *TierStore) Get(key string) (oneHour, ok bool) {
	if s == nil {
		return false, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	oneHour, ok = s.m[key]
	return
}

// set remembers the tier. Nil-safe.
func (s *TierStore) Set(key string, oneHour bool) {
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
