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
