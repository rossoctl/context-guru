package proxy

import (
	"bytes"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/rossoctl/context-guru/dash"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// A acc_fallback line is logged once for each event left on the legacy rules, with the reason. The
// usage here bills far fewer tokens than the forwarded body holds, so the library's bytes-to-tokens
// ratio saves more than the request had and both events fall back (implausible_size).
func TestFallbackReasonIsLogged(t *testing.T) {
	var buf syncBuf
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	evs, _ := accRunUsage(t, 12, 9000, 1500)
	if fb := evs[0].AccountantFallback(); fb != dash.FallbackImplausibleSize {
		t.Fatalf("second event: fallback %q, want %q", fb, dash.FallbackImplausibleSize)
	}
	if n := strings.Count(buf.String(), `"msg":"acc_fallback"`); n != 2 {
		t.Fatalf("acc_fallback lines = %d, want 2 (one for each event):\n%s", n, buf.String())
	}
	if !strings.Contains(buf.String(), `"reason":"implausible_size"`) {
		t.Fatalf("acc_fallback lacks the reason:\n%s", buf.String())
	}
}
