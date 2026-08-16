package dash

import (
	"fmt"
	"testing"
	"time"
)

// BenchmarkRecord measures the ENTIRE cost the dashboard adds to a request
// goroutine: stamp the timestamp and one non-blocking channel send. This is the
// number the issue demands be reported — a tool that sells latency awareness
// cannot pay measurable latency for its own dashboard.
//
// The writer goroutine (redaction, gzip, SQLite insert, SSE fan-out) runs
// concurrently and is deliberately NOT in this measurement, because it is not on
// the request path. BenchmarkWriterThroughput covers that instead.
func BenchmarkRecord(b *testing.B) {
	r, err := NewRecorder(Options{DBPath: ":memory:", QueueSize: 1 << 16})
	if err != nil {
		b.Fatal(err)
	}
	defer r.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Record(&Event{SessionID: "s", Model: "m", TokensBefore: 1000, TokensAfter: 900})
	}
}

// BenchmarkRecordFullQueue is the pathological case: the queue is full, so every
// call takes the drop branch. This path MUST stay O(1) — if a full queue could
// block, an overloaded dashboard would become a latency incident.
func BenchmarkRecordFullQueue(b *testing.B) {
	r, err := NewRecorder(Options{DBPath: ":memory:", QueueSize: 1})
	if err != nil {
		b.Fatal(err)
	}
	defer r.Close()
	for i := 0; i < 64; i++ {
		r.Record(&Event{}) // fill it
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Record(&Event{SessionID: "s"})
	}
}

// BenchmarkObserve measures the cache-attribution bookkeeping, the one other thing
// the request goroutine does (a map lookup under a short mutex).
func BenchmarkObserve(b *testing.B) {
	r, err := NewRecorder(Options{DBPath: ":memory:"})
	if err != nil {
		b.Fatal(err)
	}
	defer r.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Observe("", "session", "model", int64(i))
	}
}

// TestCaptureOverheadIsNegligible asserts the measured per-request cost stays
// inside a budget generous enough not to flake on a loaded CI box but far below
// anything a caller could perceive. It is a REGRESSION guard: if someone later
// moves I/O onto the capture path, this fails.
func TestCaptureOverheadIsNegligible(t *testing.T) {
	r, err := NewRecorder(Options{DBPath: ":memory:", QueueSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	const n = 20000
	evs := make([]*Event, n)
	for i := range evs {
		evs[i] = &Event{SessionID: "s", Model: "m", TokensBefore: 1000, TokensAfter: 900}
	}
	start := time.Now()
	for _, e := range evs {
		r.Record(e)
	}
	per := time.Since(start) / n

	// 50us is orders of magnitude above what the operation actually costs and a
	// rounding error against this workload's multi-second upstream round trip, so the
	// assertion catches a design regression rather than scheduler noise.
	if per > 50*time.Microsecond {
		t.Errorf("Record() costs %v per request; a capture on the hot path must be negligible", per)
	}
	t.Logf("capture overhead: %v per request (%d events)", per, n)
}

// BenchmarkWriterThroughput measures how fast the writer drains the queue —
// off-path, but it must outrun realistic traffic or the queue fills and drops.
func BenchmarkWriterThroughput(b *testing.B) {
	r, err := NewRecorder(Options{DBPath: ":memory:", QueueSize: 1 << 16, BatchSize: 256,
		FlushInterval: 5 * time.Millisecond})
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e := &Event{SessionID: fmt.Sprintf("s%d", i%64), Model: "m", TokensBefore: 1000, TokensAfter: 900}
		e.Components = []CompRow{{Component: "extract", SavedGross: 100, SavedUnique: 100}}
		r.Record(e)
	}
	r.Close() // drains
	b.StopTimer()
	if d := r.Stats().Dropped; d > 0 {
		b.Logf("dropped %d of %d (the queue could not keep up at this rate)", d, b.N)
	}
}
