package dash

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/internal/thread"
)

const twoHours = int64(2 * 60 * 60 * 1000)

// waitForRowCount blocks until the writer goroutine has persisted n rows, so a test that
// seeds a SECOND recorder from the database knows the first one's rows are on disk.
func waitForRowCount(t *testing.T, rec *Recorder, want int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var n int64
		_ = rec.DB().sql.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&n)
		if n == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("rows never reached %d", want)
}

// A restart recovers the grace clock per thread, and a keep-alive ping that READ the cache
// counts on it (#766): the thread was idle two hours, but its ping one minute before the
// restart kept the provider's entry warm, so the grace must answer the ping's time. A ping
// that did NOT read the cache kept nothing warm and must not count — the thread stays
// expired, two hours cold.
func TestARestartRecoversTheGraceClockIncludingCacheReadingPings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	rec, err := NewRecorder(Options{DBPath: path, BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	const now = int64(1_700_000_000_000)
	ping := &Event{TS: now - 60_000, TenantID: "t1", Model: "m", TokensBefore: 10,
		KeepAlive: true, CacheRead: 900}
	// s1: real request two hours ago, then a cache-reading ping one minute ago.
	rec.Record(&Event{TS: now - twoHours, TenantID: "t1", SessionID: "s1", Model: "m", TokensBefore: 10})
	s1ping := *ping
	s1ping.SessionID = "s1"
	rec.Record(&s1ping)
	// s2: the same real request, but its ping read nothing — the entry was already gone.
	rec.Record(&Event{TS: now - twoHours, TenantID: "t1", SessionID: "s2", Model: "m", TokensBefore: 10})
	s2ping := *ping
	s2ping.SessionID = "s2"
	s2ping.CacheRead = 0
	rec.Record(&s2ping)
	waitForRowCount(t, rec, 4)
	rec.Close()

	rec2, err := NewRecorder(Options{DBPath: path, BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer rec2.Close()
	if n, err := rec2.SeedGraceSessions(now); err != nil || n != 2 {
		t.Fatalf("SeedGraceSessions recovered %d threads (err %v), want 2", n, err)
	}
	if at, ok := rec2.ThreadRefreshedAt(thread.Key("s1", "")); !ok || at != now-60_000 {
		t.Errorf("pinged thread's refresh = (%d, %v), want the ping's time (%d, true)", at, ok, now-60_000)
	}
	if at, ok := rec2.ThreadRefreshedAt(thread.Key("s2", "")); !ok || at != now-twoHours {
		t.Errorf("cache-blind pinged thread's refresh = (%d, %v), want the real request's time (%d, true)", at, ok, now-twoHours)
	}
	if _, ok := rec2.ThreadRefreshedAt("s3"); ok {
		t.Errorf("a thread with no rows reads as known to the grace clock")
	}
	// Attribution stays ping-free: the recency seed answers the real request's gap, not the
	// ping's, or a ping would re-date the thread across the restart (SeedSessions' own rule).
	if n, err := rec2.SeedSessions(now); err != nil || n != 2 {
		t.Fatalf("SeedSessions recovered %d threads (err %v), want 2", n, err)
	}
	if seen, _, since, _ := rec2.ObserveThread("t1", "s1", "", "m", now, 0); !seen || since != twoHours {
		t.Errorf("pinged thread's attributed gap = %d ms (seen %v), want %d — the gap since its own last request",
			since, seen, twoHours)
	}
}

// The LIVE clock counts the same events as the seed: a real request (ObserveThread) and a
// cache-reading ping (Record) move it, a ping that read nothing does not, and a timestamp
// only ever moves forward.
func TestALiveRecorderCountsRequestsAndPingsOnTheGraceClock(t *testing.T) {
	rec, err := NewRecorder(Options{DBPath: filepath.Join(t.TempDir(), "d.db"),
		BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	const now = int64(1_700_000_000_000)
	key := thread.Key("s1", "")
	// A real request starts the clock.
	rec.ObserveThread("t1", "s1", "", "m", now-twoHours, 0)
	if at, ok := rec.ThreadRefreshedAt(key); !ok || at != now-twoHours {
		t.Fatalf("real request did not start the grace clock: (%d, %v)", at, ok)
	}
	// A cache-reading ping refreshes it.
	rec.Record(&Event{TS: now - 60_000, TenantID: "t1", SessionID: "s1", Model: "m",
		TokensBefore: 10, KeepAlive: true, CacheRead: 900})
	if at, _ := rec.ThreadRefreshedAt(key); at != now-60_000 {
		t.Fatalf("cache-reading ping did not refresh the grace clock: %d, want %d", at, now-60_000)
	}
	// A ping that read nothing does not, and neither does an older timestamp move it back.
	rec.Record(&Event{TS: now - 120_000, TenantID: "t1", SessionID: "s1", Model: "m",
		TokensBefore: 10, KeepAlive: true, CacheRead: 0})
	rec.Record(&Event{TS: now - 90_000, TenantID: "t1", SessionID: "s1", Model: "m",
		TokensBefore: 10, KeepAlive: true, CacheRead: 900})
	if at, _ := rec.ThreadRefreshedAt(key); at != now-60_000 {
		t.Fatalf("grace clock moved to %d, want it still at %d (never backwards, never on a blind ping)",
			at, now-60_000)
	}
}
