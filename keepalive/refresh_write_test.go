package keepalive

import (
	"testing"
	"time"
)

// A ping that finds the entry gone and re-creates it starts a fresh cache lifetime at that ping,
// so the clock the next real request is judged against must move to it (#580). Left at the real
// request's start, the request is aged by the whole gap and a live entry reads as ttl_expiry.
// A ping that failed, or that billed neither a read nor a write, left the entry alone.
func TestAPingMovesTheRefreshClockOnlyWhenItTouchedTheEntry(t *testing.T) {
	const pingAt = 281 * time.Second
	for _, tc := range []struct {
		name    string
		usage   Usage
		status  int
		moved   bool
		comment string
	}{
		{"read", Usage{CacheRead: 40_000, Output: 1}, 200, true, "a hit refreshes the entry"},
		{"miss that wrote", Usage{CacheWrite: 40_000, Output: 1}, 200, true, "a miss re-creates the entry (#580)"},
		{"read and wrote", Usage{CacheRead: 30_000, CacheWrite: 10_000, Output: 1}, 200, true, "a partial read still touched it"},
		{"neither", Usage{FreshInput: 40_000, Output: 1}, 200, false, "nothing cached was billed"},
		{"write on a failed ping", Usage{CacheWrite: 40_000}, 500, false, "a non-2xx wrote no entry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			now := start
			fd := newFake(func(DispatchRequest) (Usage, int, error) { return tc.usage, tc.status, nil })
			k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
			key := Key{Tenant: "t", Session: "s"}
			k.Record(trackedRecord(fd, key, start)) // turn 0: counted, not tracked
			k.Record(trackedRecord(fd, key, start)) // turn 1: tracked
			now = start.Add(pingAt)
			k.Sweep(now)
			if len(fd.sent) != 1 {
				t.Fatalf("pings sent = %d, want 1", len(fd.sent))
			}
			now = start.Add(400 * time.Second)
			_, _, _, got := k.ArriveAt(key)
			want := start
			if tc.moved {
				want = start.Add(pingAt)
			}
			if !got.Equal(want) {
				t.Fatalf("refreshedAt = start+%v, want start+%v (%s)", got.Sub(start), want.Sub(start), tc.comment)
			}
		})
	}
}

// The later of two refreshing pings wins: a write re-created the entry, a read then refreshed it.
func TestALaterReadingPingMovesTheClockPastAnEarlierWritingOne(t *testing.T) {
	start := time.Now()
	now := start
	calls := 0
	fd := newFake(func(DispatchRequest) (Usage, int, error) {
		calls++
		if calls == 1 {
			return Usage{CacheWrite: 40_000, Output: 1}, 200, nil
		}
		return Usage{CacheRead: 40_000, Output: 1}, 200, nil
	})
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	key := Key{Tenant: "t", Session: "s"}
	k.Record(trackedRecord(fd, key, start))
	k.Record(trackedRecord(fd, key, start))
	now = start.Add(281 * time.Second)
	k.Sweep(now)
	second := now.Add(280 * time.Second)
	now = second
	k.Sweep(now)
	if len(fd.sent) != 2 {
		t.Fatalf("pings sent = %d, want 2", len(fd.sent))
	}
	if _, _, _, got := k.ArriveAt(key); !got.Equal(second) {
		t.Fatalf("refreshedAt = start+%v, want the second ping's start+%v", got.Sub(start), second.Sub(start))
	}
}
