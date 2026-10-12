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
	k := New(Options{Dispatcher: fd, Now: clock}) // default Go: a real goroutine
	key := Key{Tenant: "t", Session: "s"}
	k.Record(trackedRecord(fd, key, start))
	k.Record(trackedRecord(fd, key, start))
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
	deadline := time.Now().Add(5 * time.Second)
	for k.Stats().Pings == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if _, ok := k.Entry(key); ok {
		t.Fatal("the entry survived Arrive")
	}
	if len(fd.released) != 1 {
		t.Fatalf("Release calls = %d, want exactly 1", len(fd.released))
	}
}
