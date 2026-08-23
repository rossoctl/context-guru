package proxy

import "sync"

// Which sessions have been OFFERED the expand tool.
//
// The model may call a tool it saw on an earlier turn even when the current request does not list it,
// so interception cannot be gated on the current request alone -- see the comment at `advertised` in
// serve. This is the minimum state needed to keep the fast path: a session that has never been
// offered the tool is never inspected, and no response of its is buffered.
//
// Bounded deliberately. An unbounded map keyed by session id grows with traffic on a long-lived
// proxy, so past the cap the set stops admitting new sessions rather than growing without limit. The
// consequence of falling off is only that a session reverts to advertise-gated interception, which is
// the previous behaviour, so the failure mode is a return to the old bug for the coldest sessions
// rather than unbounded memory.
const maxExpandOfferedSessions = 50_000

type expandOfferedSet struct {
	mu   sync.RWMutex
	seen map[string]struct{}
}

func (h *Handler) noteExpandOffered(sess string) {
	if h.expandSeen == nil {
		return
	}
	h.expandSeen.mu.Lock()
	defer h.expandSeen.mu.Unlock()
	if h.expandSeen.seen == nil {
		h.expandSeen.seen = make(map[string]struct{}, 1024)
	}
	if len(h.expandSeen.seen) >= maxExpandOfferedSessions {
		return
	}
	h.expandSeen.seen[sess] = struct{}{}
}

func (h *Handler) expandOffered(sess string) bool {
	if h.expandSeen == nil {
		return false
	}
	h.expandSeen.mu.RLock()
	defer h.expandSeen.mu.RUnlock()
	_, ok := h.expandSeen.seen[sess]
	return ok
}
