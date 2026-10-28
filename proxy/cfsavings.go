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

// end records the operations, then the thread and the forwarded body. A rewrite whose ChangeID
// the thread still applies is not added: the library applies it again by itself (§4.6). One that
// ended since is added again, which is how a change after a compaction is priced.
func (s *savingsTracker) end(req *cf.Request, th cf.Thread, forwarded []byte, ops []cf.Operation, facts cf.EntryFacts) {
	for _, op := range ops {
		if op.Kind == cf.KindRewrite && op.ChangeID != "" && !op.DryRun &&
			s.trackers.IsActive(req.Meta.Session, th, op.ChangeID) {
			continue
		}
		req.Add(op)
	}
	req.EndRequest(th, cfBodies(forwarded), cfBreakpoints(forwarded), facts)
}

// compute prices the request. It stores the new ThreadState (so a shadow run still advances it).
func (s *savingsTracker) compute(req *cf.Request, u Usage) cf.Result {
	return req.Compute(cf.Usage{Input: int(u.FreshInput), CacheRead: int(u.CacheRead),
		CacheWrite: int(u.CacheWrite), CacheWrite1h: int(u.CacheWrite1h)})
}
