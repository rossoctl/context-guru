package counterfactual

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func seenAt(ts *Trackers, session, thread string, at time.Time) {
	seedThread(ts, session, Thread{ID: thread}, ThreadState{PrevPrint: &Fingerprint{At: at}})
}

func TestEvictIdle_RemovesOnlyIdleTrackers(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	ts := NewTrackers()
	seenAt(ts, "s", "old", now.Add(-3*time.Hour))
	seenAt(ts, "s", "recent", now.Add(-time.Minute))
	ts.tracker(ThreadKey("s", Thread{ID: "fresh"})) // no PrevPrint: aged by creation (now)
	if n := ts.EvictIdle(now.Add(-2 * time.Hour)); n != 1 {
		t.Fatalf("evicted %d, want 1", n)
	}
	if ts.Len() != 2 {
		t.Fatalf("Len = %d, want 2", ts.Len())
	}
	for _, id := range []string{"recent", "fresh"} {
		if _, ok := ts.m[ThreadKey("s", Thread{ID: id})]; !ok {
			t.Fatalf("%s was evicted", id)
		}
	}
}

func TestEvictIdle_ForkChildSurvivesWhenRecent(t *testing.T) {
	now := time.Now()
	ts := NewTrackers()
	seenAt(ts, "s", "main", now.Add(-3*time.Hour))
	ts.Fork("s", Thread{ID: "main"}, Thread{ID: "sub"})
	// The child copies the parent's PrevPrint, so it ages with the parent until its own request.
	seenAt(ts, "s", "sub", now.Add(-time.Minute))
	ts.EvictIdle(now.Add(-2 * time.Hour))
	if _, ok := ts.m[ThreadKey("s", Thread{ID: "sub"})]; !ok || ts.Len() != 1 {
		t.Fatalf("recent fork child must survive and the idle parent go: Len %d", ts.Len())
	}
}

func TestEvictOldest_KeepsTheMostRecent(t *testing.T) {
	now := time.Now()
	ts := NewTrackers()
	for i := range 5 {
		seenAt(ts, "s", fmt.Sprint("t", i), now.Add(time.Duration(i)*time.Minute))
	}
	if n := ts.EvictOldest(2); n != 3 || ts.Len() != 2 {
		t.Fatalf("evicted %d, Len %d", n, ts.Len())
	}
	for _, id := range []string{"t3", "t4"} {
		if _, ok := ts.m[ThreadKey("s", Thread{ID: id})]; !ok {
			t.Fatalf("%s (one of the two most recent) was evicted", id)
		}
	}
	if ts.EvictOldest(10) != 0 {
		t.Fatal("under the cap nothing is evicted")
	}
}

// A Request in flight keeps its own snapshot. After its Tracker is evicted, Compute recreates one,
// prices against it (Recomputed: the version moved) and stores the next state there.
func TestEvictIdle_InFlightRequestStillComputes(t *testing.T) {
	ts := NewTrackers()
	b := mk(nil, nil, msgs(3, 100))
	r1 := ts.BeginRequest(RequestMeta{RequestID: "r1", Session: "s", At: time.Now(), Billing: BillingFrom(BillingAPI)}, b, nil)
	r1.EndRequest(Thread{ID: "main"}, b, nil, EntryFacts{TTL: 5 * time.Minute})
	r1.Compute(Usage{Input: 300})
	r2 := ts.BeginRequest(RequestMeta{RequestID: "r2", Session: "s", At: time.Now(), Billing: BillingFrom(BillingAPI)}, b, nil)
	r2.EndRequest(Thread{ID: "main"}, b, nil, EntryFacts{TTL: 5 * time.Minute})
	if ts.EvictIdle(time.Now().Add(time.Hour)) != 1 || ts.Len() != 0 {
		t.Fatal("precondition: the tracker must be evicted")
	}
	res := r2.Compute(Usage{Input: 300})
	if !res.Recomputed || res.Phase != PhaseUnknown {
		t.Fatalf("Recomputed %v phase %v, want a recompute against an empty state", res.Recomputed, res.Phase)
	}
	if ts.Len() != 1 || ts.m[ThreadKey("s", Thread{ID: "main"})].snapshot().PrevPrint == nil {
		t.Fatal("Compute must store the next state in a recreated tracker")
	}
}

func TestEviction_ConcurrentWithRequests(t *testing.T) {
	ts := NewTrackers()
	b := mk(nil, nil, msgs(3, 100))
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				th := Thread{ID: fmt.Sprint("t", g, "-", i%5)}
				r := ts.BeginRequest(RequestMeta{RequestID: "r", Session: "s", At: time.Now(), Billing: BillingFrom(BillingAPI)}, b, nil)
				r.EndRequest(th, b, nil, EntryFacts{TTL: 5 * time.Minute})
				ts.Fork("s", th, Thread{ID: th.ID + "-sub"})
				r.Compute(Usage{Input: 300})
				if i%17 == 0 {
					ts.EvictIdle(time.Now().Add(-time.Millisecond))
					ts.EvictOldest(10)
					_ = ts.Len()
				}
			}
		}()
	}
	wg.Wait()
}
