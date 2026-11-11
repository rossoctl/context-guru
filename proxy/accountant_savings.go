package proxy

import (
	"bytes"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/rossoctl/accountant"
)

// savingsTracker runs the accountant calls around one request. It holds the one Trackers
// of the proxy: a thread's state lives there between requests.
type savingsTracker struct {
	trackers *accountant.Trackers
	siblings *siblingStore // each thread's last forwarded body, for EntryFacts.SharedWarm
	// clients holds the hash of each open request's client body, by request, from begin to end.
	clients sync.Map // *accountant.Request -> clientHash
	// onEnd, when set, is called with each record after EndRequest. Tests only.
	onEnd func(*accountant.Request)
	// written is the last state file's bytes, for save to skip an unchanged write.
	written lastSaved
	// rescueFactsMissing counts priced requests that followed keep-alive pings but found no ping facts.
	rescueFactsMissing atomic.Int64
}

// noteRescueFactsMissing counts one priced request that followed pings and found no ping facts. Nil-safe.
func (s *savingsTracker) noteRescueFactsMissing() {
	if s != nil {
		s.rescueFactsMissing.Add(1)
	}
}

// RescueFactsMissing is the count noteRescueFactsMissing keeps. Nil-safe.
func (s *savingsTracker) RescueFactsMissing() int64 {
	if s == nil {
		return 0
	}
	return s.rescueFactsMissing.Load()
}

// clientHash is what begin built from the client body, kept until end so a request that was forwarded unchanged does
// not hash the same bytes again. Hashing a 600 KB body costs 3 to 4 ms.
type clientHash struct {
	raw  []byte
	body accountant.Body
	bps  []accountant.Breakpoint
}

// drop forgets the client hash of a request that will never end: a panic between accBegin and accEnd makes the proxy
// send the original body, and end is the only other place that removes the entry. Nil-safe.
func (s *savingsTracker) drop(req *accountant.Request) {
	if s != nil && req != nil {
		s.clients.Delete(req)
	}
}

func newSavingsTracker() *savingsTracker {
	return &savingsTracker{trackers: accountant.NewTrackers(), siblings: newSiblingStore()}
}

// begin opens the request record with the client body, before any component ran. The Trackers bound
// their own memory (accountant design §6.1, Memory), so there is nothing to sweep here.
func (s *savingsTracker) begin(meta accountant.RequestMeta, client []byte) *accountant.Request {
	body, bps := accParse(client)
	req := s.trackers.BeginRequest(meta, body, bps)
	s.clients.Store(req, clientHash{raw: client, body: body, bps: bps})
	return req
}

// end records the operations, then the thread and the forwarded body. Every op is added, replays
// included: the library replaces an active change by the reported op with the same ChangeID
// (accountant internal/engine/compute.go, items()), so a change is priced once and unique_change never fires.
// Skipping a replay here would lose the op whenever Compute ends the change on this request.
func (s *savingsTracker) end(req *accountant.Request, th accountant.Thread, forwarded []byte, ops []accountant.Operation, facts accountant.EntryFacts) {
	for _, op := range ops {
		req.Add(op)
	}
	var fwd accountant.Body
	var bps []accountant.Breakpoint
	if v, ok := s.clients.LoadAndDelete(req); ok && bytes.Equal(v.(clientHash).raw, forwarded) {
		fwd, bps = v.(clientHash).body, v.(clientHash).bps
	} else {
		fwd, bps = accParse(forwarded)
	}
	facts.SharedWarm = s.siblings.sharedWarm(req.Meta.Session, th.ID, req.Meta.At, facts.TTL, fwd, bps)
	req.EndRequest(th, fwd, bps, facts)
	if s.onEnd != nil {
		s.onEnd(req)
	}
}

// computeSafe is compute for the request path. The library asks the host to wrap Compute, and this repo's rule is
// to fail open: a panic here would unwind through finish and lose the cg.request log line and the dashboard row
// of a request that was already forwarded. It reports ok=false instead, the request is recorded with no saving
// (PriceMode falls back with no_result), and the panic is logged. Tests call compute, so a panic still fails them.
func (s *savingsTracker) computeSafe(req *accountant.Request, u Usage) (res accountant.Result, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("accountant: Compute panicked; the request is recorded with no saving", "panic", r)
			res, ok = accountant.Result{}, false
		}
	}()
	return s.compute(req, u), true
}

// compute prices the request. It stores the new ThreadState (so a shadow run still advances it).
func (s *savingsTracker) compute(req *accountant.Request, u Usage) accountant.Result {
	return req.Compute(accountant.Usage{Input: int(u.FreshInput), CacheRead: int(u.CacheRead),
		CacheWrite: int(u.CacheWrite), CacheWrite1h: int(u.CacheWrite1h)})
}
