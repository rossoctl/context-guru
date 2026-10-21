package dash

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// seedManyThreads writes one session with `threads` threads (thread 0 is the main one) of `turns`
// rows each, plus `noise` rows of other sessions. Each thread wrote its cache once, on its first
// turn, and read it after: the shape of a long Claude Code session with hundreds of sub-agents.
func seedManyThreads(tb testing.TB, db *DB, session string, threads, turns, noise int) {
	tb.Helper()
	now := time.Now().UnixMilli()
	var evs []*Event
	flush := func() {
		if err := db.insertBatch(evs); err != nil {
			tb.Fatal(err)
		}
		evs = evs[:0]
	}
	for i := 0; i < threads*turns; i++ {
		th, turn := i%threads, i/threads
		e := mkEvent(now-int64(threads*turns-i)*1000, session, "aws/claude-sonnet-5", 1000, 900)
		e.Status, e.Components = 200, nil
		if th > 0 {
			e.ThreadID = fmt.Sprintf("a:%04d", th)
		}
		if turn == 0 {
			e.CacheWrite, e.CacheRead = 5000, 0
		} else {
			e.CacheWrite, e.CacheRead = 0, 5000
		}
		evs = append(evs, e)
		if len(evs) == 500 {
			flush()
		}
	}
	for i := 0; i < noise; i++ {
		e := mkEvent(now-int64(i)*100, fmt.Sprintf("other-%d", i%50), "aws/claude-sonnet-5", 1000, 900)
		e.Status, e.Components = 200, nil
		evs = append(evs, e)
		if len(evs) == 500 {
			flush()
		}
	}
	flush()
}

// BenchmarkCacheStateManyThreads times one /api/cachestate read of a session with 300 threads.
func BenchmarkCacheStateManyThreads(b *testing.B) {
	rec, err := NewRecorder(Options{DBPath: filepath.Join(b.TempDir(), "d.db")})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { rec.Close() })
	seedManyThreads(b, rec.DB(), "s-big", 300, 30, 20000)
	m := http.NewServeMux()
	NewAPI(rec).Mount(m)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		m.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/cachestate?session=s-big", nil))
		if w.Code != 200 {
			b.Fatalf("code %d: %s", w.Code, w.Body)
		}
	}
}
