package proxy_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/components"
)

// syncBuf is a Writer a slog handler can share with the test goroutine.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) records(t *testing.T) []map[string]any {
	t.Helper()
	s.mu.Lock()
	raw := s.b.Bytes()
	out := make([]byte, len(raw))
	copy(out, raw)
	s.mu.Unlock()
	var recs []map[string]any
	for _, line := range bytes.Split(out, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		m := map[string]any{}
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("log line is not JSON: %s (%v)", line, err)
		}
		recs = append(recs, m)
	}
	return recs
}

// captureDebugLogs points the process logger at a buffer at DEBUG level, which is the only
// level at which the pipeline's per-decision lines exist at all.
func captureDebugLogs(t *testing.T) *syncBuf {
	t.Helper()
	buf := &syncBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// pipelineMsgs are the lines apply emits per run: the boundary decision, one per component,
// and the run summary. In observe mode ALL of them come from the off-path job, because the
// enforced path never calls apply at all.
var pipelineMsgs = map[string]bool{"cg.cache_boundary": true, "cg.component": true, "cg.run": true}

// TestObserveOffPathLinesCarryTenantAndMode: the off-path observation runs on the pool's
// own context, which is context.Background(), so logging.From falls through to the process
// default logger and every pipeline line the observation writes loses the per-request
// attributes — the tenant above all. Consequences, all of them silent:
//
//   - a Loki query for {tenant="X"} shows that tenant's requests and none of its pipeline
//     decisions, so with DEBUG on the reason a component declined is unreachable from the
//     request that triggered it;
//   - the ERROR-level panic recovery in apply.BodyOpts — whose own comment says a panic is
//     the line you most want attributed to a tenant — is unattributed on this path;
//   - and without a `mode` attr a panel summing cg.run `saved` mixes never-enforced observe
//     projections into enforced totals, which is the potential_* namespace's whole reason
//     to exist, defeated in the logs.
func TestObserveOffPathLinesCarryTenantAndMode(t *testing.T) {
	buf := captureDebugLogs(t)

	up, _ := captureUpstream(t)
	h, _ := modeHandler(t, modePipeline, up.URL, components.ModeObserve)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	post(t, srv, dupBody())

	// The observation is off-path by construction, so the response says nothing about it.
	// Wait for its run summary, which is the last line it writes.
	var recs []map[string]any
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		recs = buf.records(t)
		done := false
		for _, r := range recs {
			if r["msg"] == "cg.run" {
				done = true
			}
		}
		if done {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	var seen int
	for _, r := range recs {
		msg, _ := r["msg"].(string)
		if !pipelineMsgs[msg] {
			continue
		}
		seen++
		if got, _ := r["tenant"].(string); got != "local" {
			t.Errorf("%s: tenant = %q, want %q — the off-path job lost the request logger",
				msg, got, "local")
		}
		if got, _ := r["mode"].(string); got != string(components.ModeObserve) {
			t.Errorf("%s: mode = %q, want %q — an observe projection must not be summable "+
				"together with enforced runs", msg, got, components.ModeObserve)
		}
	}
	if seen < 4 {
		t.Fatalf("saw %d pipeline lines, want at least 4 (boundary, 2 components, run); "+
			"the observation did not run", seen)
	}
}

// TestSyncPipelineLinesCarryTheirMode is the other half: `mode` has to be present on the
// ENFORCED path too, or "no mode attr" is indistinguishable from "old line" and the only
// way to exclude observe from a sum is a negative match on an absent field.
func TestSyncPipelineLinesCarryTheirMode(t *testing.T) {
	buf := captureDebugLogs(t)

	up, _ := captureUpstream(t)
	h, _ := modeHandler(t, modePipeline, up.URL, components.ModeSync)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	post(t, srv, dupBody())

	var seen int
	for _, r := range buf.records(t) {
		msg, _ := r["msg"].(string)
		if !pipelineMsgs[msg] {
			continue
		}
		seen++
		if got, _ := r["mode"].(string); got != string(components.ModeSync) {
			t.Errorf("%s: mode = %q, want %q", msg, got, components.ModeSync)
		}
		if got, _ := r["tenant"].(string); got != "local" {
			t.Errorf("%s: tenant = %q, want %q", msg, got, "local")
		}
	}
	if seen < 4 {
		t.Fatalf("saw %d pipeline lines, want at least 4", seen)
	}
}
