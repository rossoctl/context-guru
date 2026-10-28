package proxy

import cf "github.com/rossoctl/context-guru/counterfactual"

// savingsTracker runs the counterfactual calls around one request. It holds the one Trackers
// of the proxy: a thread's state lives there between requests.
type savingsTracker struct{ trackers *cf.Trackers }

func newSavingsTracker() *savingsTracker { return &savingsTracker{trackers: cf.NewTrackers()} }

// begin opens the request record with the client body, before any component ran.
func (s *savingsTracker) begin(meta cf.RequestMeta, client []byte) *cf.Request {
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
}

// compute prices the request. It stores the new ThreadState (so a shadow run still advances it).
func (s *savingsTracker) compute(req *cf.Request, u Usage) cf.Result {
	return req.Compute(cf.Usage{Input: int(u.FreshInput), CacheRead: int(u.CacheRead),
		CacheWrite: int(u.CacheWrite), CacheWrite1h: int(u.CacheWrite1h)})
}
