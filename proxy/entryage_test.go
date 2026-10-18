package proxy

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/dash"
)

// A keep-alive ping that READ the cache moved the provider's clock. Measured from the last real
// request, a 327 s gap with a ping at 296 s read as ttl_expiry on a turn that came 31 s after the
// refresh (requests 8621 / 8622 / 8623 in the local database). The cache did not lapse: the turn
// missed because the summary rewrote the prefix, which is prefix_change.
func TestAPingThatRefreshedTheEntryShortensItsAge(t *testing.T) {
	const ttlMs = 300_000
	base := time.UnixMilli(1_000_000)
	for _, tc := range []struct {
		name        string
		sinceMs     int64
		refreshedAt time.Time
		wantAge     int64
		wantReason  string
	}{
		{"no ping: the gap stands and the entry lapsed", 327_000, time.Time{}, 327_000, dash.CacheTTLExpiry},
		{"a ping 31 s before the turn: not an expiry", 327_000, base.Add(296 * time.Second), 31_000, dash.CachePrefixChange},
		{"a ping from before the last real request changes nothing", 327_000, base.Add(-5 * time.Second), 327_000, dash.CacheTTLExpiry},
		{"the last ping itself lapsed", 700_000, base.Add(100 * time.Second), 600_000, dash.CacheTTLExpiry},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := base.UnixMilli() + tc.sinceMs
			age := entryAgeMs(tc.sinceMs, ts, tc.refreshedAt)
			if age != tc.wantAge {
				t.Fatalf("entry age = %d ms, want %d", age, tc.wantAge)
			}
			e := &dash.Event{CacheWrite: 221_557}
			e.AttributeCache(true, true, age, ttlMs, e.CacheWrite > 0)
			if e.CacheMissReason != tc.wantReason {
				t.Errorf("verdict = %q, want %q", e.CacheMissReason, tc.wantReason)
			}
		})
	}
}

// The keeper hands the refresh time to the request that ends the idle span: the start of the last
// ping that read the cache, not the start of the last real request.
func TestArriveAtReportsWhenTheEntryWasLastRefreshed(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	start := clock.now()
	recordOne(t, k, kaPolicy(), kaBody, start, upstream{base: "http://up", path: "/v1/messages"})
	at := clock.advance(281 * time.Second)
	k.Sweep(at)
	waitPings(t, k, 1)

	a := k.arriveAt("t1", "sess-1", "")
	if a.pings != 1 {
		t.Fatalf("pings = %d, want 1", a.pings)
	}
	if !a.refreshedAt.After(start) || a.refreshedAt.After(at) {
		t.Errorf("refreshedAt = %v, want the ping's start inside (%v, %v]", a.refreshedAt, start, at)
	}
	if again := k.arriveAt("t1", "sess-1", ""); !again.refreshedAt.IsZero() || again.pings != 0 {
		t.Errorf("the entry must be cleared once a real request arrives: %+v", again)
	}
}

// A ping from AFTER this request (a clock skew, or an entry cleared out of order) gives a negative
// age. It must not shorten the gap into the past.
func TestAFutureRefreshTimeNeverShortensTheGap(t *testing.T) {
	base := time.UnixMilli(1_000_000)
	ts := base.UnixMilli() + 327_000
	if got := entryAgeMs(327_000, ts, time.UnixMilli(ts+5_000)); got != 327_000 {
		t.Errorf("age = %d, want the raw gap 327000 for a refresh time after the request", got)
	}
}

// The wiring, end to end: noteKeepAlive hands the refresh time to the capture and finish() uses it
// for the verdict that is STORED. entryAgeMs alone passes with the wiring cut, which is how a
// zero time in finish() went unnoticed.
func TestFinishUsesThePingRefreshTimeForTheStoredVerdict(t *testing.T) {
	const gap = 327 * time.Second
	for _, tc := range []struct {
		name        string
		refreshedAt func(first time.Time) time.Time
		want        string
	}{
		{"a ping refreshed the entry 31 s before the turn", func(f time.Time) time.Time { return f.Add(296 * time.Second) }, dash.CachePrefixChange},
		{"no ping: the entry lapsed", func(time.Time) time.Time { return time.Time{} }, dash.CacheTTLExpiry},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, err := dash.NewRecorder(dash.Options{
				DBPath: filepath.Join(t.TempDir(), "d.db"), BatchSize: 1, FlushInterval: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { rec.Close() })
			first := time.Now().Add(-time.Hour)
			mk := func(start time.Time) *capture {
				return &capture{rec: rec, tenant: "t1", provider: "anthropic", model: "claude-opus-5-5",
					route: "/v1/messages", start: start, status: 200,
					trace: apply.Trace{Session: "sess-1"}}
			}
			a := mk(first)
			a.finish(Usage{FreshInput: 10, CacheWrite: 1000}, true, false, 0, 0)
			b := mk(first.Add(gap))
			b.noteKeepAlive(kaArrival{pings: 1, refreshed: 400_000, refreshedAt: tc.refreshedAt(first)})
			// The turn re-created its prefix: a miss with a write.
			b.finish(Usage{FreshInput: 10, CacheWrite: 220_000}, true, false, 0, 0)

			var got string
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				pg, err := rec.DB().Requests(dash.Filter{TenantAll: true}, 0, 10)
				if err == nil && len(pg.Requests) == 2 {
					got = pg.Requests[0].CacheMissReason // newest first
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if got != tc.want {
				t.Errorf("stored verdict = %q, want %q", got, tc.want)
			}
		})
	}
}
