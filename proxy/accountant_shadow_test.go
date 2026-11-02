package proxy

import (
	"bytes"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// A acc_fallback line is logged once for each event left on the legacy rules, with the reason. The first request of a thread has no previous request (penalty_unknown): unpriced.
func TestFallbackReasonIsLogged(t *testing.T) {
	var buf syncBuf
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	evs, _ := accRunUsage(t, 20, 0, 50)
	if fb := evs[0].AccountantFallback(); fb != "" {
		t.Fatalf("second event fell back: %s", fb)
	}
	if n := strings.Count(buf.String(), `"msg":"acc_fallback"`); n != 1 {
		t.Fatalf("acc_fallback lines = %d, want 1 (the first request):\n%s", n, buf.String())
	}
	if !strings.Contains(buf.String(), `"reason":"unpriced"`) {
		t.Fatalf("acc_fallback lacks the reason:\n%s", buf.String())
	}
}
