package keepalive

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// timedOutKeeper tracks one priced entry that reports through a sink, then fires its first ping,
// which resp answers.
func timedOutKeeper(t *testing.T, resp func(now *time.Time) (Usage, int, error)) (*Keeper, *reportSink, EntryView, EntryView) {
	t.Helper()
	start := time.Now()
	now := start
	fd := newFake(func(DispatchRequest) (Usage, int, error) { return resp(&now) })
	fd.setPrices(proxyPricer{fixedPrice{modelinfo.Price{Input: 3e-6, Output: 15e-6, CacheRead: 3e-7, CacheWrite: 3.75e-6}}})
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	sink := &reportSink{}
	key := Key{Tenant: "t", Session: "s"}
	r := trackedRecord(fd, key, start)
	r.Emitter = sink
	k.Record(r) // turn 0: counted, not tracked
	k.Record(r) // turn 1: tracked
	before, _ := k.Entry(key)
	now = start.Add(281 * time.Second)
	k.Sweep(now)
	if fd.n() != 1 {
		t.Fatalf("sent %d pings, want 1", fd.n())
	}
	after, _ := k.Entry(key)
	return k, sink, before, after
}

// #754. A ping that runs to pingTimeout was sent and is probably billed (#532), so it books one row
// at the keeper's projected cost, marked TimedOut, and counts in timed_out as well as failed. It is
// not a refresh: the entry's refreshed figure does not move.
func TestATimedOutPingBooksItsProjectedCost(t *testing.T) {
	k, sink, before, after := timedOutKeeper(t, func(now *time.Time) (Usage, int, error) {
		*now = now.Add(pingTimeout) // the call ran to its own timeout
		return Usage{}, 0, fmt.Errorf("Post upstream: %w", context.DeadlineExceeded)
	})
	if before.PingUSD <= 0 {
		t.Fatalf("fixture: projected ping cost %v, want > 0", before.PingUSD)
	}
	reps := sink.all()
	if len(reps) != 1 {
		t.Fatalf("a timed-out ping booked %d rows, want 1", len(reps))
	}
	r := reps[0]
	if !r.TimedOut || r.CostUSD != before.PingUSD || r.CacheRead != 0 || r.Status != 0 {
		t.Errorf("row = timed_out %v cost %v cache_read %d status %d, want true, %v, 0, 0",
			r.TimedOut, r.CostUSD, r.CacheRead, r.Status, before.PingUSD)
	}
	st := k.Stats()
	if st.TimedOut != 1 || st.Failed != 1 {
		t.Errorf("stats timed_out %d failed %d, want 1 and 1", st.TimedOut, st.Failed)
	}
	if math.Abs(st.SpentUSD-before.PingUSD) > 1e-12 {
		t.Errorf("spend_usd %v, want the projected %v", st.SpentUSD, before.PingUSD)
	}
	if after.Refreshed != before.Refreshed {
		t.Errorf("a timed-out ping moved Refreshed from %d to %d: nobody knows it refreshed anything",
			before.Refreshed, after.Refreshed)
	}
}

// A ping that fails fast (EOF, reset, refused) probably never reached the provider: no row, no
// cost, and not a timeout. This is the behavior before #754, kept.
func TestAFastFailedPingBooksNothing(t *testing.T) {
	k, sink, _, _ := timedOutKeeper(t, func(*time.Time) (Usage, int, error) {
		return Usage{}, 0, errors.New("EOF")
	})
	if n := len(sink.all()); n != 0 {
		t.Errorf("a fast failure booked %d rows, want 0", n)
	}
	st := k.Stats()
	if st.TimedOut != 0 || st.Failed != 1 || st.SpentUSD != 0 {
		t.Errorf("stats timed_out %d failed %d spend %v, want 0, 1, 0", st.TimedOut, st.Failed, st.SpentUSD)
	}
}
