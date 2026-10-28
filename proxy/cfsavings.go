package proxy

import (
	"sync/atomic"
	"time"

	cf "github.com/rossoctl/context-guru/counterfactual"
)

// Bounds on the thread state the savings tracker keeps (cortex-wiring W5.3). Each Tracker holds a
// thread's previous fingerprint (two Bodies, ~12 bytes per position each) and its active changes.
const (
	// savingsEvictEvery is how many requests pass between two idle sweeps.
	savingsEvictEvery = 1000
	// savingsIdleAfter is how long a thread must be unseen to be evicted. Twice the longest cache TTL
	// (1h): past the TTL the library can only find the phase unknown, so the evicted state could not
	// have produced a figure anyway, and nothing correct is lost.
	savingsIdleAfter = 2 * time.Hour
	// savingsMaxTrackers is the hard cap. Over it, the threads seen longest ago are evicted down to
	// savingsMaxTrackers - savingsMaxTrackers/10, so the sort runs once per tenth of the cap and not
	// on every request. An evicted thread's next request falls back to legacy (no previous request).
	savingsMaxTrackers = 10000
)

// savingsTracker runs the counterfactual calls around one request. It holds the one Trackers
// of the proxy: a thread's state lives there between requests.
type savingsTracker struct {
	trackers *cf.Trackers
	// seen counts begin calls, for the idle sweep.
	seen atomic.Uint64
	// now is time.Now; a test replaces it.
	now func() time.Time
	// onEnd, when set, is called with each record after EndRequest. Tests only.
	onEnd func(*cf.Request)
}

func newSavingsTracker() *savingsTracker {
	return &savingsTracker{trackers: cf.NewTrackers(), now: time.Now}
}

// begin opens the request record with the client body, before any component ran. It also bounds
// the thread state: an idle sweep every savingsEvictEvery requests, and the hard cap.
func (s *savingsTracker) begin(meta cf.RequestMeta, client []byte) *cf.Request {
	if s.seen.Add(1)%savingsEvictEvery == 0 {
		s.trackers.EvictIdle(s.now().Add(-savingsIdleAfter))
	}
	if s.trackers.Len() > savingsMaxTrackers {
		s.trackers.EvictOldest(savingsMaxTrackers - savingsMaxTrackers/10)
	}
	return s.trackers.BeginRequest(meta, cfBodies(client), cfBreakpoints(client))
}

// end records the operations, then the thread and the forwarded body. Every op is added, replays
// included: the library replaces an active change by the reported op with the same ChangeID
// (counterfactual/compute.go, items()), so a change is priced once and unique_change never fires.
// Skipping a replay here would lose the op whenever Compute ends the change on this request.
func (s *savingsTracker) end(req *cf.Request, th cf.Thread, forwarded []byte, ops []cf.Operation, facts cf.EntryFacts) {
	for _, op := range ops {
		req.Add(op)
	}
	req.EndRequest(th, cfBodies(forwarded), cfBreakpoints(forwarded), facts)
	if s.onEnd != nil {
		s.onEnd(req)
	}
}

// compute prices the request. It stores the new ThreadState (so a shadow run still advances it).
func (s *savingsTracker) compute(req *cf.Request, u Usage) cf.Result {
	return req.Compute(cf.Usage{Input: int(u.FreshInput), CacheRead: int(u.CacheRead),
		CacheWrite: int(u.CacheWrite), CacheWrite1h: int(u.CacheWrite1h)})
}
