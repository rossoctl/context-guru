package proxy

import (
	"github.com/rossoctl/accountant"
)

// savingsTracker runs the counterfactual calls around one request. It holds the one Trackers
// of the proxy: a thread's state lives there between requests.
type savingsTracker struct {
	trackers *accountant.Trackers
	// onEnd, when set, is called with each record after EndRequest. Tests only.
	onEnd func(*accountant.Request)
}

func newSavingsTracker() *savingsTracker {
	return &savingsTracker{trackers: accountant.NewTrackers()}
}

// begin opens the request record with the client body, before any component ran. The Trackers bound
// their own memory (accountant design §6.1, Memory), so there is nothing to sweep here.
func (s *savingsTracker) begin(meta accountant.RequestMeta, client []byte) *accountant.Request {
	return s.trackers.BeginRequest(meta, cfBodies(client), cfBreakpoints(client))
}

// end records the operations, then the thread and the forwarded body. Every op is added, replays
// included: the library replaces an active change by the reported op with the same ChangeID
// (accountant internal/engine/compute.go, items()), so a change is priced once and unique_change never fires.
// Skipping a replay here would lose the op whenever Compute ends the change on this request.
func (s *savingsTracker) end(req *accountant.Request, th accountant.Thread, forwarded []byte, ops []accountant.Operation, facts accountant.EntryFacts) {
	for _, op := range ops {
		req.Add(op)
	}
	req.EndRequest(th, cfBodies(forwarded), cfBreakpoints(forwarded), facts)
	if s.onEnd != nil {
		s.onEnd(req)
	}
}

// compute prices the request. It stores the new ThreadState (so a shadow run still advances it).
func (s *savingsTracker) compute(req *accountant.Request, u Usage) accountant.Result {
	return req.Compute(accountant.Usage{Input: int(u.FreshInput), CacheRead: int(u.CacheRead),
		CacheWrite: int(u.CacheWrite), CacheWrite1h: int(u.CacheWrite1h)})
}
