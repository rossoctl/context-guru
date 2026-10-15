package keepalive

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

// pingOnce records a tracked entry and sweeps once at Idle+1s, with resp answering the ping.
func pingOnce(t *testing.T, resp func(DispatchRequest) (Usage, int, error)) (*Keeper, *fakeDispatcher, Key, time.Time) {
	t.Helper()
	start := time.Now()
	now := start
	fd := newFake(resp)
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	key := Key{Tenant: "t", Session: "s"}
	// A session's first request is gated out of tracking (kaEntry.pingable: turn < 1), so a
	// tracked entry needs two Records — see recordOne in keeper_test.go and the same pattern in
	// shutdown_test.go / inflight_test.go.
	k.Record(trackedRecord(fd, key, start)) // turn 0: counted, not tracked
	k.Record(trackedRecord(fd, key, start)) // turn 1: tracked, Held
	now = start.Add(281 * time.Second)
	k.Sweep(now)
	return k, fd, key, now
}

func TestARealMissIsNotAFailedPing(t *testing.T) {
	k, _, key, _ := pingOnce(t, func(DispatchRequest) (Usage, int, error) {
		return Usage{FreshInput: 40_000, Output: 1}, http.StatusOK, nil
	})
	if s := k.Stats(); s.Missed != 1 || s.Failed != 0 || s.Wrote != 0 {
		t.Fatalf("stats = %+v, want Missed=1 Failed=0 Wrote=0", s)
	}
	if e, ok := k.Entry(key); !ok || e.Stopped {
		t.Fatalf("a first miss must keep pinging: entry=%+v ok=%v", e, ok)
	}
}

func TestASecondMissInARowStops(t *testing.T) {
	start := time.Now()
	now := start
	fd := newFake(func(DispatchRequest) (Usage, int, error) { return Usage{FreshInput: 40_000, Output: 1}, 200, nil })
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	key := Key{Tenant: "t", Session: "s"}
	r := trackedRecord(fd, key, start)
	r.Policy.MaxPings = 3
	k.Record(r) // turn 0: counted, not tracked
	k.Record(r) // turn 1: tracked, Held
	now = start.Add(281 * time.Second)
	k.Sweep(now)
	now = now.Add(281 * time.Second)
	k.Sweep(now)
	if e, ok := k.Entry(key); ok && !e.Stopped {
		t.Fatal("a second miss in a row must stop the entry")
	}
	if s := k.Stats(); s.Missed != 1 || s.Wrote != 1 {
		t.Fatalf("stats = %+v, want Missed=1 Wrote=1", s)
	}
}

func TestPingFailureNeedsNoUsageOrAnError(t *testing.T) {
	k, _, key, _ := pingOnce(t, func(DispatchRequest) (Usage, int, error) { return Usage{}, http.StatusOK, nil })
	if s := k.Stats(); s.Failed != 1 {
		t.Fatalf("an all-zero 2xx is a failure: stats = %+v", s)
	}
	if e, ok := k.Entry(key); ok && !e.Stopped {
		t.Fatal("an all-zero 2xx must stop the entry")
	}
}

func TestOverloadedPingsAreTransient(t *testing.T) {
	for _, st := range []int{429, 500, 529} {
		k, _, key, _ := pingOnce(t, func(DispatchRequest) (Usage, int, error) { return Usage{}, st, nil })
		// Skipped is 2, not 1: pingOnce's own warm-up turn (see its comment) costs one skip
		// before the ping this test is about ever fires; the second is this status's own
		// outcomeTransient.
		if s := k.Stats(); s.Skipped != 2 || s.Failed != 0 {
			t.Fatalf("status %d: stats = %+v, want Skipped=2 Failed=0", st, s)
		}
		if e, ok := k.Entry(key); !ok || e.Stopped {
			t.Fatalf("status %d must not stop the entry", st)
		}
	}
}

func TestAnyOther4xxStops(t *testing.T) {
	k, _, key, _ := pingOnce(t, func(DispatchRequest) (Usage, int, error) { return Usage{}, http.StatusBadRequest, nil })
	if s := k.Stats(); s.Failed != 1 {
		t.Fatalf("stats = %+v, want Failed=1", s)
	}
	if e, ok := k.Entry(key); ok && !e.Stopped {
		t.Fatal("a 400 must stop the entry")
	}
}

func TestATransportErrorIsNotAFailure(t *testing.T) {
	k, _, key, _ := pingOnce(t, func(DispatchRequest) (Usage, int, error) { return Usage{}, 0, errors.New("dial tcp: refused") })
	if e, ok := k.Entry(key); !ok || e.Stopped {
		t.Fatal("a transport error must not stop the entry")
	}
	// restored in Task 2
	// if s := k.Stats(); s.RouteTrips != 0 {
	// 	t.Fatalf("a transport error must not count toward the breaker: %+v", s)
	// }
}
