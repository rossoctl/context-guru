package counterfactual

import (
	"testing"
	"time"
)

func TestRequestRecordsTheCallsInOrder(t *testing.T) {
	ts := NewTrackers()
	r := ts.BeginRequest(RequestMeta{RequestID: "r1", Session: "s1", At: time.Unix(0, 0)},
		NewBody(nil, [][]byte{[]byte("sys")}, [][]byte{[]byte("hello")}), nil)
	r.Add(Operation{ID: "a", Kind: KindRewrite, Operator: "mask"})
	r.Add(Operation{ID: "b", Kind: KindSetting, Operator: "router"})
	r.EndRequest(Thread{ID: "main"}, Body{}, []Breakpoint{{TTL: "5m"}}, EntryFacts{})

	if len(r.Ops) != 2 || r.Ops[0].ID != "a" || r.Ops[1].ID != "b" {
		t.Fatalf("Ops = %+v, want a then b", r.Ops)
	}
	if !r.ended || len(r.Breakpoints) != 1 || r.Thread.ID != "main" {
		t.Fatalf("EndRequest did not record: %+v", r)
	}
}

func TestComputeWithNoUsagePricesNothing(t *testing.T) {
	ts := NewTrackers()
	r := ts.BeginRequest(RequestMeta{Session: "s1"}, Body{}, nil)
	r.Add(Operation{ID: "a", Kind: KindRewrite})
	r.EndRequest(Thread{ID: "main"}, Body{}, nil, EntryFacts{})

	res := r.Compute(Usage{})
	if len(res.Ops) != 1 || !hasUnpriced(res, ReasonNoUsage) {
		t.Fatalf("result = %+v, want the op unpriced with no_usage", res)
	}
}

func TestComputeWithoutEndRequestPricesNothing(t *testing.T) {
	ts := NewTrackers()
	r := ts.BeginRequest(RequestMeta{Session: "s1"}, Body{}, nil)
	r.Add(Operation{ID: "a", Kind: KindRewrite})
	res := r.Compute(Usage{Input: 10})
	if !hasUnpriced(res, ReasonEndRequestMissing) {
		t.Fatalf("result = %+v, want end_request_missing", res)
	}
}

func TestEndRequestCopiesTheState(t *testing.T) {
	ts := NewTrackers()
	tr := ts.tracker(ThreadKey("s1", Thread{ID: "main"}))
	tr.state.Active = []Operation{{ID: "x"}}
	r := ts.BeginRequest(RequestMeta{Session: "s1"}, Body{}, nil)
	r.EndRequest(Thread{ID: "main"}, Body{}, nil, EntryFacts{})
	r.start.Active[0].ID = "changed"
	if tr.state.Active[0].ID != "x" {
		t.Fatal("the record shares the Tracker's Active slice")
	}
}

func TestNewBodyHashesThePrefix(t *testing.T) {
	a := NewBody(nil, [][]byte{[]byte("s")}, [][]byte{[]byte("m0"), []byte("m1")})
	b := NewBody(nil, [][]byte{[]byte("s")}, [][]byte{[]byte("m0"), []byte("mX")})
	if a.Pos[0] != b.Pos[0] || a.Pos[1] != b.Pos[1] {
		t.Fatal("equal prefixes must have equal hashes")
	}
	if a.Pos[2].Hash == b.Pos[2].Hash {
		t.Fatal("a different position must change the hash")
	}
	if a.Start != [3]int{0, 0, 1} {
		t.Fatalf("Start = %v", a.Start)
	}
}
