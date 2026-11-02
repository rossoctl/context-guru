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
	ping := &Event{TS: now - 60_000, TenantID: "t1", Status: 200, Model: "m", TokensBefore: 10,
		KeepAlive: true, CacheRead: 900}
	// s1: real request two hours ago, then a cache-reading ping one minute ago. Status 200
	// because the seed counts only rows the upstream accepted.
	rec.Record(&Event{TS: now - twoHours, TenantID: "t1", SessionID: "s1", Status: 200, Model: "m", TokensBefore: 10})
	s1ping := *ping
	s1ping.SessionID = "s1"
	rec.Record(&s1ping)
	// s2: the same real request, but its ping read nothing — the entry was already gone.
	rec.Record(&Event{TS: now - twoHours, TenantID: "t1", SessionID: "s2", Status: 200, Model: "m", TokensBefore: 10})
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

// A request under an EXPLICIT header id also refreshes the provider entry its conversation's
// HEADERLESS turns read, so the seed must cover the alias key a row stored (#766, round 3):
// after a restart, a headerless turn asks for the alias key alone — its own derived id IS
// the alias — and the header id's warmth cannot reach it any other way. Two documented limits
// land here too: a row with no stored alias (written before the column existed, or a
// keep-alive ping, which carries no body to derive one from) seeds only its own key, and a
// row the upstream rejected seeds nothing — it wrote no cache entry.
func TestTheGraceSeedCoversTheAliasSessionIdToo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	rec, err := NewRecorder(Options{DBPath: path, BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	const now = int64(1_700_000_000_000)
	// The seesaw conversation's last request: explicit header id, alias stored, 2xx.
	rec.Record(&Event{TS: now - 60_000, TenantID: "t1", SessionID: "explicit-a", Status: 200,
		SessionAlias: "alias-b", Model: "m", TokensBefore: 10})
	// A row from before the alias column existed: only its own key can be recovered.
	rec.Record(&Event{TS: now - 60_000, TenantID: "t1", SessionID: "old-row", Status: 200,
		Model: "m", TokensBefore: 10})
	// A request the upstream rejected: no cache entry, so no warmth to recover.
	rec.Record(&Event{TS: now - 60_000, TenantID: "t1", SessionID: "failed", Status: 500,
		SessionAlias: "alias-f", Model: "m", TokensBefore: 10})
	waitForRowCount(t, rec, 3)
	rec.Close()

	rec2, err := NewRecorder(Options{DBPath: path, BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer rec2.Close()
	// Three keys: the explicit id's, its alias's, and the old row's own.
	if n, err := rec2.SeedGraceSessions(now); err != nil || n != 3 {
		t.Fatalf("SeedGraceSessions recovered %d keys (err %v), want 3", n, err)
	}
	if at, ok := rec2.ThreadRefreshedAt(thread.Key("explicit-a", "")); !ok || at != now-60_000 {
		t.Errorf("explicit id's refresh = (%d, %v), want (%d, true)", at, ok, now-60_000)
	}
	if at, ok := rec2.ThreadRefreshedAt(thread.Key("alias-b", "")); !ok || at != now-60_000 {
		t.Errorf("alias id's refresh = (%d, %v), want (%d, true) — the headerless turns' key",
			at, ok, now-60_000)
	}
	if _, ok := rec2.ThreadRefreshedAt(thread.Key("failed", "")); ok {
		t.Errorf("a rejected request's thread reads as warm to the grace clock")
	}
	if _, ok := rec2.ThreadRefreshedAt(thread.Key("alias-f", "")); ok {
		t.Errorf("a rejected request's alias reads as warm to the grace clock")
	}
}

// The LIVE clock learns the alias key the same way (#766, round 3): the writer goroutine
// derives the alias off the request body (Event.AliasFn, the same lazy normalization
// WaitCause uses), touches the grace clock under it, and stores it on the row for the next
// restart's seed. A row whose alias is its own id (a headerless request) touches nothing
// extra — observe() already covered that key.
func TestALiveRecorderCountsTheAliasKeyOnTheGraceClock(t *testing.T) {
	rec, err := NewRecorder(Options{DBPath: filepath.Join(t.TempDir(), "d.db"),
		BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	const now = int64(1_700_000_000_000)
	rec.Record(&Event{TS: now - 60_000, TenantID: "t1", SessionID: "explicit-a", Status: 200,
		ThreadID: "th1", Model: "m", TokensBefore: 10,
		AliasFn: func() string { return "alias-b" }})
	rec.Record(&Event{TS: now - 60_000, TenantID: "t1", SessionID: "derived-c", Status: 200,
		ThreadID: "th2", Model: "m", TokensBefore: 10,
		AliasFn: func() string { return "" }})
	waitForRowCount(t, rec, 2)

	if at, ok := rec.ThreadRefreshedAt(thread.Key("alias-b", "th1")); !ok || at != now-60_000 {
		t.Fatalf("alias key's refresh = (%d, %v), want (%d, true)", at, ok, now-60_000)
	}
	if _, ok := rec.ThreadRefreshedAt(thread.Key("alias-b", "th2")); ok {
		t.Fatalf("a row with no separate alias warmed another thread's alias key")
	}
	// The alias reached the row, so the next restart's seed can recover it.
	page, err := rec.DB().Requests(Filter{}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range page.Requests {
		switch e.SessionID {
		case "explicit-a":
			if e.SessionAlias != "alias-b" {
				t.Errorf("explicit row's stored alias = %q, want %q", e.SessionAlias, "alias-b")
			}
		case "derived-c":
			if e.SessionAlias != "" {
				t.Errorf("headerless row's stored alias = %q, want ''", e.SessionAlias)
			}
		}
	}
}
