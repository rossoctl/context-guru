package keepalive

import (
	"sync"
	"testing"
	"time"
)

// A real request arriving while that thread's ping is in flight releases the held credential;
// the ping must finish on its own copy, and the entry must not be pinged again.
func TestArriveDuringAnInFlightPing(t *testing.T) {
	start := time.Now()
	now := start
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	inFlight, finish := make(chan struct{}), make(chan struct{})
	fd := newFake(func(DispatchRequest) (Usage, int, error) {
		close(inFlight)
		<-finish
		return Usage{CacheRead: 50_000}, 200, nil
	})
	// A real goroutine, as the default Go runs it, but one that signals when the whole fire
	// (Dispatch, then the keeper's bookkeeping) has returned.
	fired := make(chan struct{})
	goFire := func(f func()) { go func() { defer close(fired); f() }() }
	k := New(Options{Dispatcher: fd, Now: clock, Go: goFire})
	key := Key{Tenant: "t", Session: "s"}
	k.Record(trackedRecord(fd, key, start)) // turn 0: counted, not tracked
	k.Record(trackedRecord(fd, key, start)) // turn 1: tracked, Held
	mu.Lock()
	now = start.Add(281 * time.Second)
	mu.Unlock()
	if n := k.Sweep(clock()); n != 1 {
		t.Fatalf("Sweep started %d pings, want 1", n)
	}
	<-inFlight
	k.Arrive(key) // the real request: releases while the ping is still on the wire
	if n := fd.holds(key); n != 0 {
		t.Fatalf("balance %d after Arrive, want 0", n)
	}
	close(finish)
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("the fire goroutine did not finish")
	}
	if p := k.Stats().Pings; p != 1 {
		t.Fatalf("Pings = %d after the fire returned, want 1", p)
	}
	if _, ok := k.Entry(key); ok {
		t.Fatal("the entry survived Arrive")
	}
	if len(fd.released) != 1 {
		t.Fatalf("Release calls = %d, want exactly 1", len(fd.released))
	}
}

func TestAnOpenRequestOnTheSameThreadPausesItsPing(t *testing.T) {
	start := time.Now()
	now := start
	fd := newFake(okPing)
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	key := Key{Tenant: "t", Session: "s"}
	k.Record(trackedRecord(fd, key, start)) // turn 0: counted, not tracked
	k.Arrive(key)                           // request A opens
	k.Arrive(key)                           // request B opens, in parallel on the same thread
	k.Record(trackedRecord(fd, key, start)) // A finishes: tracked, and departs one; B is still open
	now = start.Add(281 * time.Second)
	if n := k.Sweep(now); n != 0 {
		t.Fatalf("Sweep started %d pings while B is open, want 0", n)
	}
	k.Depart(key) // B ends without a Record (an error)
	if n := k.Sweep(now); n != 1 {
		t.Fatalf("Sweep started %d pings after B departed, want 1", n)
	}
}

func TestASlowSubagentRequestDoesNotDelayTheMainThreadsPing(t *testing.T) {
	start := time.Now()
	now := start
	fd := newFake(okPing)
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	main := Key{Tenant: "t", Session: "s"}
	sub := Key{Tenant: "t", Session: "s", Thread: "a:x"}
	k.Record(trackedRecord(fd, main, start)) // turn 0: counted, not tracked
	k.Record(trackedRecord(fd, main, start)) // turn 1: tracked
	k.Arrive(sub)                            // a subagent request stays open
	now = start.Add(281 * time.Second)
	if n := k.Sweep(now); n != 1 {
		t.Fatalf("Sweep started %d pings, want 1: another thread's request must not pause the main thread", n)
	}
}

func TestDepartWithNoArrivalIsANoOp(t *testing.T) {
	fd := newFake(okPing)
	k := New(Options{Dispatcher: fd})
	k.Depart(Key{Tenant: "t", Session: "s"}) // must not panic
	var nilK *Keeper
	nilK.Depart(Key{Session: "s"}) // nil-safe, like Arrive
}
