package dash

import (
	"path/filepath"
	"testing"
	"time"
)

const fiveMin = int64(5 * 60 * 1000)

// attribute runs one request through the same two calls proxy/dashcapture.go makes, and
// returns its miss label. Every request here re-wrote its prompt (cache_write > 0, no read),
// which is the shape of the 46 misses issue #423 was filed for.
func attribute(r *Recorder, session, threadID string, ts int64) string {
	seenSession, seenModel, since, _ := r.ObserveThread("t1", session, threadID, "m", ts, 0)
	e := &Event{CacheWrite: 400_000}
	e.AttributeCache(seenSession, seenModel, since, fiveMin, e.CacheWrite > 0)
	return e.CacheMissReason
}

// Test 1 of the issue: the main thread M is idle for 8 minutes while a subagent S sends a request
// every minute. M's next miss is a TTL expiry, not a prefix change.
func TestInterleavedThreadsAttributeTheMainThreadsExpiry(t *testing.T) {
	const t0 = int64(1_700_000_000_000)
	run := func(sub string, observe func(r *Recorder, thread string, ts int64) string) string {
		r := &Recorder{lastSeen: map[string]int64{}, lastTail: map[string]uint64{}, seenModel: map[string]bool{}}
		observe(r, "", t0)
		for i := int64(1); i <= 8; i++ {
			observe(r, sub, t0+i*60_000)
		}
		return observe(r, "", t0+8*60_000+30_000)
	}
	perThread := func(r *Recorder, thread string, ts int64) string { return attribute(r, "s1", thread, ts) }
	if got := run("a:S", perThread); got != CacheTTLExpiry {
		t.Errorf("main thread miss after 8.5 idle minutes = %q, want %q", got, CacheTTLExpiry)
	}
	// Control: the same traffic keyed on the session alone reproduces the defect. Without this,
	// the assertion above could pass because the fixture never reached the subagent branch.
	perSession := func(r *Recorder, _ string, ts int64) string { return attribute(r, "s1", "", ts) }
	if got := run("a:S", perSession); got != CachePrefixChange {
		t.Fatalf("control: session-keyed attribution = %q; the fixture no longer reproduces #423", got)
	}
}

// Test 2 of the issue: a subagent's first request is a cold start for a new thread.
func TestASubagentsFirstRequestIsAColdStart(t *testing.T) {
	r := &Recorder{lastSeen: map[string]int64{}, lastTail: map[string]uint64{}, seenModel: map[string]bool{}}
	const t0 = int64(1_700_000_000_000)
	attribute(r, "s1", "", t0)
	if got := attribute(r, "s1", "a:S", t0+1000); got != CacheColdStart {
		t.Errorf("subagent's first request = %q, want %q", got, CacheColdStart)
	}
	if got := attribute(r, "s1", "a:S", t0+2000); got != CachePrefixChange {
		t.Errorf("subagent's second request = %q; a re-write inside one thread is still a prefix change", got)
	}
}

// The primary thread keys exactly as the session did, so single-thread traffic is unchanged:
// ObserveSplit (the pre-#423 entry point) and ObserveThread with the primary id share state.
func TestThePrimaryThreadSharesTheSessionsState(t *testing.T) {
	r := &Recorder{lastSeen: map[string]int64{}, lastTail: map[string]uint64{}, seenModel: map[string]bool{}}
	r.ObserveSplit("t1", "s1", "m", 1000, 0)
	seen, _, since, _ := r.ObserveThread("t1", "s1", "", "m", 4000, 0)
	if !seen || since != 3000 {
		t.Fatalf("primary thread did not see the session's state: seen=%v since=%d", seen, since)
	}
}

// A restart recovers each THREAD's recency, not each session's: seeded from the session's latest
// row, the main thread would be re-dated with the subagent's last request and its expiry hidden.
func TestARestartRecoversRecencyPerThread(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	rec, err := NewRecorder(Options{DBPath: path, BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	const now = int64(1_700_000_000_000)
	rec.Record(&Event{TS: now, TenantID: "t1", SessionID: "s1", Model: "m", TokensBefore: 10})
	rec.Record(&Event{TS: now + 420_000, TenantID: "t1", SessionID: "s1", ThreadID: "a:S", Model: "m", TokensBefore: 10})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var n int64
		_ = rec.DB().sql.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&n)
		if n == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	rec.Close()

	rec2, err := NewRecorder(Options{DBPath: path, BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer rec2.Close()
	if n, err := rec2.SeedSessions(now + 480_000); err != nil || n != 2 {
		t.Fatalf("seeded %d threads (err %v), want 2", n, err)
	}
	seen, _, since, _ := rec2.ObserveThread("t1", "s1", "", "m", now+480_000, 0)
	if !seen || since != 480_000 {
		t.Errorf("main thread recovered gap = %d ms (seen %v), want 480000 — the gap since its own last request", since, seen)
	}
	seen, _, since, _ = rec2.ObserveThread("t1", "s1", "a:S", "m", now+480_000, 0)
	if !seen || since != 60_000 {
		t.Errorf("subagent recovered gap = %d ms (seen %v), want 60000", since, seen)
	}
	// And the column round-trips to the read path the dashboard serves.
	page, err := rec2.DB().Requests(Filter{Tenant: "t1"}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	threads := map[string]bool{}
	for _, e := range page.Requests {
		threads[e.ThreadID] = true
	}
	if !threads[""] || !threads["a:S"] {
		t.Errorf("thread ids read back: %v", threads)
	}
}
