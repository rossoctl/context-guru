package keepalive

import (
	"errors"
	"testing"
	"time"
)

// failedPingKeeper tracks one entry recorded at start, with resp answering every ping and now
// readable and settable by resp itself, so a test can model a failure that takes time.
func failedPingKeeper(t *testing.T, resp func(now *time.Time, n int) (Usage, int, error)) (*Keeper, *fakeDispatcher, Key, time.Time, *time.Time) {
	t.Helper()
	start := time.Now()
	now := start
	calls := 0
	fd := newFake(func(DispatchRequest) (Usage, int, error) {
		calls++
		return resp(&now, calls)
	})
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	key := Key{Tenant: "t", Session: "s"}
	k.Record(trackedRecord(fd, key, start)) // turn 0: counted, not tracked
	k.Record(trackedRecord(fd, key, start)) // turn 1: tracked, Held
	return k, fd, key, start, &now
}

// #703. A ping that fails in transport refreshed nothing. Before this fix the keeper still counted
// it and restarted its clock from it, so the next ping came a full Idle later, past the cache's
// lifetime, and re-wrote the prefix ($4.52 on a 903k-token thread). A failure that comes back
// while the cache is still alive must be retried at the next sweep tick, and must not use a ping.
func TestAFastTransportFailureIsRetriedWhileTheCacheLives(t *testing.T) {
	k, fd, key, start, now := failedPingKeeper(t, func(_ *time.Time, n int) (Usage, int, error) {
		if n == 1 {
			return Usage{}, 0, errors.New("EOF")
		}
		return Usage{CacheRead: 50_000, Output: 1}, 200, nil
	})
	*now = start.Add(281 * time.Second)
	k.Sweep(*now)
	if e, _ := k.Entry(key); e.Pings != 0 {
		t.Fatalf("after a failed ping the entry shows %d pings used, want 0: the failure must be given back", e.Pings)
	}
	*now = start.Add(283 * time.Second) // the next sweep tick, still inside the 300 s lifetime
	k.Sweep(*now)
	if got := fd.n(); got != 2 {
		t.Fatalf("%d pings sent, want 2: the failed ping was not retried while the cache was alive", got)
	}
	e, _ := k.Entry(key)
	if e.Pings != 1 || e.Refreshed != 50_000 {
		t.Fatalf("after the retry: pings %d refreshed %d, want 1 and 50000", e.Pings, e.Refreshed)
	}
}

// A ping that times out returns after the cache entry has expired. A retry then can only re-write
// the prefix, so it is not given back: the old schedule stands.
func TestAFailureAfterTheCacheExpiredIsNotRetried(t *testing.T) {
	k, fd, key, start, now := failedPingKeeper(t, func(now *time.Time, n int) (Usage, int, error) {
		*now = now.Add(pingTimeout) // the call ran to its own timeout
		return Usage{}, 0, errors.New("context deadline exceeded")
	})
	*now = start.Add(281 * time.Second)
	k.Sweep(*now)
	if e, _ := k.Entry(key); e.Pings != 1 {
		t.Fatalf("a failure past the cache's lifetime was given back (pings %d, want 1)", e.Pings)
	}
	k.Sweep(now.Add(2 * time.Second))
	if got := fd.n(); got != 1 {
		t.Fatalf("%d pings sent, want 1: a retry after expiry can only re-write the prefix", got)
	}
}

// A route that keeps failing gets ONE retry, not a ping on every 2 s sweep tick.
func TestARunOfTransportFailuresIsRetriedOnce(t *testing.T) {
	k, fd, _, start, now := failedPingKeeper(t, func(*time.Time, int) (Usage, int, error) {
		return Usage{}, 0, errors.New("connection reset by peer")
	})
	for s := 281; s <= 299; s += 2 {
		*now = start.Add(time.Duration(s) * time.Second)
		k.Sweep(*now)
	}
	if got := fd.n(); got != 2 {
		t.Fatalf("%d pings sent inside one lifetime, want 2 (the ping and one retry)", got)
	}
}
