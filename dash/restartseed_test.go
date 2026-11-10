package dash

import (
	"path/filepath"
	"testing"
	"time"
)

// rsRow is one stored request of the restart-seed tests (#994).
type rsRow struct {
	ts          int64
	thread      string
	model       string
	read, write int64
	write1h     int64
	status      int
	keepalive   bool
}

// restartWithRows stores the rows, restarts the recorder on the same database and seeds it.
func restartWithRows(t *testing.T, seedAt int64, rows ...rsRow) *Recorder {
	t.Helper()
	path := filepath.Join(t.TempDir(), "d.db")
	rec, err := NewRecorder(Options{DBPath: path, BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range rows {
		if w.status == 0 {
			w.status = 200
		}
		rec.Record(&Event{TS: w.ts, TenantID: "t1", SessionID: "s", ThreadID: w.thread, Model: w.model, Status: w.status,
			KeepAlive: w.keepalive, CacheRead: w.read, CacheWrite: w.write, CacheWrite1h: w.write1h})
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var n int64
		_ = rec.DB().sql.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&n)
		if int(n) == len(rows) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	rec.Close()
	rec2, err := NewRecorder(Options{DBPath: path, BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec2.Close() })
	if _, err := rec2.SeedSessions(seedAt); err != nil {
		t.Fatal(err)
	}
	return rec2
}

// turnOn runs one request through the calls proxy/dashcapture.go makes, on a thread and model.
func turnOn(r *Recorder, thread, from string, hasFrom bool, model string, ts int64, read, write, write1h int64) *Event {
	ev := &Event{TS: ts, Model: model, CacheRead: read, CacheWrite: write, CacheWrite1h: write1h}
	f := base()
	f.Model = model
	d := FactsOf(ev)
	f.TS, f.OneHour, f.NoWrite, f.PromptTokens = ts, d.OneHour, d.NoWrite, d.PromptTokens
	var seenSession, seenModel bool
	var since int64
	if hasFrom {
		seenSession, seenModel, since, _ = r.ObserveThreadFrom("t1", "s", thread, from, model, ts, 0)
	} else {
		seenSession, seenModel, since, _ = r.ObserveThread("t1", "s", thread, model, ts, 0)
	}
	cause, modelGap, known, _, prevPrompt, prevOneHour := r.ObservePrefix("s", thread, from, hasFrom, f, true)
	if known {
		since = modelGap
	}
	ttl := int64(fiveMin)
	if prevOneHour {
		ttl = 3_600_000
	}
	e := &Event{CacheWrite: write, CacheRead: read, CacheWrite1h: write1h}
	e.AttributeCache(seenSession, seenModel, since, ttl, cause, prevPrompt)
	return e
}

const rsT0 = int64(1_700_000_000_000)
const rsMin = int64(60_000)

func TestRestartSeedsTheSeenModel(t *testing.T) {
	r := restartWithRows(t, rsT0+2*rsMin, rsRow{ts: rsT0, thread: "th", model: "A", write: 50_000})
	// A hit on a model seen before the restart is a hit.
	if got := turnOn(r, "th", "", false, "A", rsT0+2*rsMin, 50_000, 0, 0).CacheMissReason; got != CacheHit {
		t.Errorf("hit on a known model read %q, want %q", got, CacheHit)
	}
	// A miss on it is not a cold start either (a hit alone cannot tell: it wins first).
	if got := turnOn(r, "th", "", false, "A", rsT0+3*rsMin, 0, 50_000, 0).CacheMissReason; got == CacheColdStart {
		t.Errorf("miss on a known model read %q", got)
	}
	if e := turnOn(r, "th2", "", false, "B", rsT0+3*rsMin, 0, 50_000, 0); e.CacheMissReason != CacheColdStart {
		t.Errorf("model never seen: reason = %q, want %q", e.CacheMissReason, CacheColdStart)
	}
}

func TestRestartSeedTierIsPerModel(t *testing.T) {
	r := restartWithRows(t, rsT0+3*rsMin,
		rsRow{ts: rsT0, thread: "th", model: "B", write: 50_000},
		rsRow{ts: rsT0 + rsMin, thread: "th", model: "A", write: 50_000, write1h: 50_000})
	// B's own tier is 5m. A's later 1h write must not leak to B.
	turnOn(r, "th", "", false, "B", rsT0+3*rsMin, 50_000, 0, 0)
	e := turnOn(r, "th", "", false, "B", rsT0+3*rsMin+20*rsMin, 0, 50_000, 0)
	if e.CacheMissReason != CacheTTLExpiry {
		t.Errorf("B miss after 20 min: reason = %q, want %q", e.CacheMissReason, CacheTTLExpiry)
	}
}

func TestRestartSeedTierGoesToAFork(t *testing.T) {
	r := restartWithRows(t, rsT0+2*rsMin, rsRow{ts: rsT0, thread: "th", model: "A", write: 50_000, write1h: 50_000})
	// The fork's first request is a read-only turn: it keeps the parent's 1h tier.
	turnOn(r, "p:fork", "th", true, "A", rsT0+2*rsMin, 50_000, 0, 0)
	e := turnOn(r, "p:fork", "th", true, "A", rsT0+22*rsMin, 0, 50_000, 50_000)
	if e.CacheMissReason == CacheTTLExpiry {
		t.Errorf("fork at 20 min read %q; the parent's 1h tier was lost", e.CacheMissReason)
	}
}

func TestRestartSeedRecencyIgnoresAFailedRequest(t *testing.T) {
	r := restartWithRows(t, rsT0+6*rsMin,
		rsRow{ts: rsT0, thread: "th", model: "A", write: 50_000, status: 200},
		rsRow{ts: rsT0 + 5*rsMin, thread: "th", model: "A", status: 500})
	// The entry is 6 min old on a 5m tier. The failed request at 5 min must not make it younger.
	e := turnOn(r, "th", "", false, "A", rsT0+6*rsMin, 0, 50_000, 0)
	if e.CacheMissReason != CacheTTLExpiry {
		t.Errorf("reason = %q, want %q", e.CacheMissReason, CacheTTLExpiry)
	}
}

func TestRestartSeedsNoModelFromFailedRows(t *testing.T) {
	r := restartWithRows(t, rsT0+2*rsMin,
		rsRow{ts: rsT0, thread: "th", model: "B", write: 50_000},
		rsRow{ts: rsT0 + rsMin, thread: "th", model: "A", status: 500})
	if got := turnOn(r, "th", "", false, "A", rsT0+2*rsMin, 0, 50_000, 0).CacheMissReason; got != CacheColdStart {
		t.Errorf("model with only failed rows: reason = %q, want %q", got, CacheColdStart)
	}
}

func TestRestartSeedsNoModelFromAPing(t *testing.T) {
	r := restartWithRows(t, rsT0+2*rsMin,
		rsRow{ts: rsT0, thread: "th", model: "B", write: 50_000},
		rsRow{ts: rsT0 + rsMin, thread: "th", model: "A", read: 50_000, keepalive: true})
	if got := turnOn(r, "th", "", false, "A", rsT0+2*rsMin, 0, 50_000, 0).CacheMissReason; got != CacheColdStart {
		t.Errorf("model with only a ping row: reason = %q, want %q", got, CacheColdStart)
	}
}

func TestRestartFailedOneHourWriteSeedsNoTier(t *testing.T) {
	r := restartWithRows(t, rsT0+2*rsMin,
		rsRow{ts: rsT0, thread: "th", model: "A", write: 50_000},
		rsRow{ts: rsT0 + rsMin, thread: "th", model: "A", write: 50_000, write1h: 50_000, status: 500})
	turnOn(r, "th", "", false, "A", rsT0+2*rsMin, 50_000, 0, 0)
	if got := turnOn(r, "th", "", false, "A", rsT0+22*rsMin, 0, 50_000, 0).CacheMissReason; got != CacheTTLExpiry {
		t.Errorf("reason = %q, want %q (a failed 1h write must not seed 1h)", got, CacheTTLExpiry)
	}
}
