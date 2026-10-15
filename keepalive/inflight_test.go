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

// Record must depart on every early-return path, not only its ordinary one — an empty Body, a
// zero StartedAt, a refused Status, and a turn-0 Record all return before Record reaches its
// pingable() check, and each must still end the Arrive it is closing out.
//
// A single later Record on the same thread cannot by itself prove this: Record's own defer
// departs unconditionally, so one subsequent Record silently absorbs a single leaked count
// regardless of whether the broken call departed correctly. To actually surface a leak, a
// SECOND Arrive has to open on top of the (possibly still-open) first one, so that only a
// correctly-departed broken call leaves the thread's count at exactly zero once the final,
// tracking Record departs its own Arrive. If the broken call leaked instead, the count is left
// at one, and Sweep must not fire — this is the shape of the bug the review found: an unpaired
// Arrive stops that thread's pings for good.
func TestRecordDepartsOnEveryEarlyReturnPath(t *testing.T) {
	start := time.Now()
	cases := []struct {
		name string
		// turnZero, when true, means the broken call IS the session's very first Record (turn
		// 0: counted, not tracked, and no setup call precedes it). When false, an ordinary
		// turn-0 Record runs first so the broken call is a later turn instead.
		turnZero bool
		broken   func(fd *fakeDispatcher, key Key) Record
	}{
		{name: "empty body", broken: func(fd *fakeDispatcher, key Key) Record {
			r := trackedRecord(fd, key, start)
			r.Body = nil
			return r
		}},
		{name: "zero StartedAt", broken: func(fd *fakeDispatcher, key Key) Record {
			r := trackedRecord(fd, key, start)
			r.StartedAt = time.Time{}
			return r
		}},
		{name: "refused status", broken: func(fd *fakeDispatcher, key Key) Record {
			r := trackedRecord(fd, key, start)
			r.Status = 500
			return r
		}},
		{name: "turn-0 Record", turnZero: true, broken: func(fd *fakeDispatcher, key Key) Record {
			return trackedRecord(fd, key, start) // turn 0: counted, not tracked
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			now := start
			fd := newFake(okPing)
			k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
			key := Key{Tenant: "t", Session: "s"}
			if !c.turnZero {
				k.Record(trackedRecord(fd, key, start)) // turn 0: counted, not tracked
			}
			k.Arrive(key) // request A opens
			k.Record(c.broken(fd, key))
			k.Arrive(key)                           // request B opens on the same thread
			k.Record(trackedRecord(fd, key, start)) // B finishes: tracked, and departs its own open
			now = start.Add(281 * time.Second)
			if n := k.Sweep(now); n != 1 {
				t.Fatalf("Sweep started %d pings after a %s Record, want 1: A's open count leaked", n, c.name)
			}
		})
	}
}
