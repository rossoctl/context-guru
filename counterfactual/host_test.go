package counterfactual

import (
	"testing"
	"time"
)

func TestPingOps_OneSpendWithWindowChangeID(t *testing.T) {
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	win := at.Add(-4 * time.Minute)
	ops, facts := PingOps(Ping{
		Session: "s1", ThreadID: "", Model: "m", Host: "h",
		Usage: CallUsage{Usage: Usage{CacheRead: 1000, Input: 10}, Output: 1},
		At:    at, Window: win,
	}, BillingFrom(BillingAPI))

	if ops.Thread.ID != "main" {
		t.Fatalf("thread = %q, want main", ops.Thread.ID)
	}
	if len(ops.Ops) != 1 || ops.Ops[0].Kind != KindSpend || ops.Ops[0].Call == nil {
		t.Fatalf("want one spend with a call, got %+v", ops.Ops)
	}
	wantID := "ka:main:" + win.UTC().Format(time.RFC3339)
	if ops.Ops[0].ChangeID != wantID || facts.PingWindow != wantID {
		t.Fatalf("ChangeID = %q, PingWindow = %q, want %q", ops.Ops[0].ChangeID, facts.PingWindow, wantID)
	}
	if !facts.LastPing.Equal(at) {
		t.Fatalf("LastPing = %v, want %v", facts.LastPing, at)
	}
}

func TestIsActive_FalseForUnknownThread(t *testing.T) {
	ts := NewTrackers()
	if ts.IsActive("s", Thread{ID: "main"}, "cg:x") {
		t.Fatal("an unseen thread has no active change")
	}
}

func seedThread(ts *Trackers, session string, th Thread, st ThreadState) {
	ts.tracker(ThreadKey(session, th)).state = st
}

func TestFork_CopiesParentStateIndependently(t *testing.T) {
	ts := NewTrackers()
	parent, child := Thread{ID: "main"}, Thread{ID: "side", Parent: "main"}
	fp := &Fingerprint{Thread: "main"}
	seedThread(ts, "s", parent, ThreadState{Active: []Operation{{ChangeID: "c1"}}, PrevPrint: fp, Version: 7})
	ts.Fork("s", parent, child)
	if !ts.IsActive("s", child, "c1") {
		t.Fatal("child did not inherit the active change")
	}
	ct := ts.tracker(ThreadKey("s", child))
	if ct.state.Version != 0 {
		t.Errorf("child version = %d, want 0", ct.state.Version)
	}
	ct.state.Active[0].ChangeID = "changed"
	ct.state.PrevPrint.Thread = "changed"
	if !ts.IsActive("s", parent, "c1") || fp.Thread != "main" {
		t.Fatal("mutating the child changed the parent")
	}
}

func TestFork_NoOpCases(t *testing.T) {
	ts := NewTrackers()
	parent, child := Thread{ID: "main"}, Thread{ID: "side"}
	ts.Fork("s", parent, child) // parent has no state
	if _, ok := ts.m[ThreadKey("s", child)]; ok {
		t.Fatal("forked from a parent with no state")
	}
	seedThread(ts, "s", parent, ThreadState{Active: []Operation{{ChangeID: "c1"}}})
	seedThread(ts, "s", child, ThreadState{Active: []Operation{{ChangeID: "own"}}})
	ts.Fork("s", parent, child) // child already has state
	if ts.IsActive("s", child, "c1") || !ts.IsActive("s", child, "own") {
		t.Fatal("Fork overwrote a child that had state")
	}
}

func TestAddUnlessActive(t *testing.T) {
	ts := NewTrackers()
	th := Thread{ID: "main"}
	seedThread(ts, "s", th, ThreadState{Active: []Operation{{ChangeID: "c1"}}})
	req := ts.BeginRequest(RequestMeta{Session: "s"}, Body{}, nil)
	cases := []struct {
		name string
		op   Operation
		want bool
	}{
		{"active rewrite is skipped", Operation{Kind: KindRewrite, ChangeID: "c1"}, false},
		{"inactive rewrite is added", Operation{Kind: KindRewrite, ChangeID: "c2"}, true},
		{"dry run is added", Operation{Kind: KindRewrite, ChangeID: "c1", DryRun: true}, true},
		{"spend is added", Operation{Kind: KindSpend, ChangeID: "c1"}, true},
		{"empty change id is added", Operation{Kind: KindRewrite}, true},
	}
	for _, c := range cases {
		n := len(req.Ops)
		got := req.AddUnlessActive(th, c.op)
		if got != c.want || (len(req.Ops) == n+1) != c.want {
			t.Errorf("%s: returned %v, ops %d -> %d", c.name, got, n, len(req.Ops))
		}
	}
}
