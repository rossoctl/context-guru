package predictive

import "testing"

func chainLen(a *Arrival) int {
	n := 0
	for ; a != nil; a = a.prev {
		n++
	}
	return n
}

// Requests that are dropped before their response (errors, a session that ended) must not make a thread's history grow without bound, and neither may a campaign that
// never takes features.
func TestPredecessorChainsStayBounded(t *testing.T) {
	tr := NewTracker()
	var last *Arrival
	for i := 0; i < 100_000; i++ {
		last = tr.Arrive(Request{Tenant: "t", Session: "s", Thread: "never-finished", TsMs: int64(i) * 1000})
	}
	if n := chainLen(last); n > maxChain+2 {
		t.Errorf("unfinished requests chain %d deep", n)
	}
	tr2 := NewTracker()
	for i := 0; i < 100_000; i++ {
		a := tr2.Arrive(Request{Tenant: "t", Session: "s", Thread: "plan-only", TsMs: int64(i) * 1000})
		tr2.Release(a, &Response{Request: a.req, EndMs: int64(i)*1000 + 500})
		last = a
	}
	if n := chainLen(last); n > 2 {
		t.Errorf("released requests chain %d deep", n)
	}
}

// Two requests that overlap on one thread are two requests: the second is turn 1 whatever order the responses end in.
func TestOverlappingRequestsAreCountedOnce(t *testing.T) {
	tr := NewTracker()
	r1 := Request{Tenant: "t", Session: "s", Thread: "th", Model: "m", TsMs: 1_000_000}
	r2 := r1
	r2.TsMs = 1_002_000
	a1, a2 := tr.Arrive(r1), tr.Arrive(r2)
	resp := func(r Request, end int64) *Response {
		return &Response{Request: r, EndMs: end, DecMs: end, Prefix: 1000, Fresh: 5, Output: 10}
	}
	row2, row1 := NewRow(), NewRow()
	tr.Ended(a2, resp(r2, 1_004_000))
	tr.Features(a2, resp(r2, 1_004_000), row2) // the later request finishes first
	tr.Ended(a1, resp(r1, 1_006_000))
	tr.Features(a1, resp(r1, 1_006_000), row1)
	if got := row2.F[GBSBase+indexOfGBS("turn_in_thread")]; got != 1 {
		t.Errorf("second request is turn %v, want 1", got)
	}
	if got := row1.F[GBSBase+indexOfGBS("turn_in_thread")]; got != 0 {
		t.Errorf("first request is turn %v, want 0", got)
	}
	if got := row1.F[GBSBase+indexOfGBS("session_turn")]; got != 0 {
		t.Errorf("first request session turn %v, want 0", got)
	}
	if !row2.Skipped {
		t.Error("the request whose predecessor was still running is not flagged")
	}
}
