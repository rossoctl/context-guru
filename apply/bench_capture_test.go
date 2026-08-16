package apply_test

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/store"
)

// benchPipeline is the DETERMINISTIC hosted default: codesmart minus the LLM
// component (extract_llm is off by default), which is what the latency work targets.
const benchPipeline = `pipeline: [format, dedup, failed_run, cmdfilter, extract, cachesplit]
components:
  extract:
    min_tokens: 400
`

type capRec struct {
	Provider string          `json:"provider"`
	Body     json.RawMessage `json:"body"`
}

// loadCapture reads a CONTEXT_GURU_CAPTURE jsonl of real requests. Never committed:
// captures live outside the repo (they contain real traffic).
func loadCapture(tb testing.TB, max int) []capRec {
	tb.Helper()
	path := os.Getenv("CONTEXT_GURU_CAPTURE")
	if path == "" {
		tb.Skip("set CONTEXT_GURU_CAPTURE to a captured-traffic jsonl")
	}
	f, err := os.Open(path)
	if err != nil {
		tb.Skipf("cannot read capture %q: %v", path, err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	var out []capRec
	for sc.Scan() && (max <= 0 || len(out) < max) {
		var r capRec
		if json.Unmarshal(sc.Bytes(), &r) != nil || len(r.Body) == 0 {
			continue
		}
		if r.Provider == "" {
			r.Provider = string(bschemas.Anthropic)
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		tb.Skipf("capture %q held no usable requests", path)
	}
	return out
}

func benchPipe(tb testing.TB) *components.Pipeline {
	tb.Helper()
	c, err := config.LoadBytes([]byte(benchPipeline))
	if err != nil {
		tb.Fatal(err)
	}
	p, _ := c.Build(nil)
	return p
}

// BenchmarkDeterministicPipelineOnCapture runs the default deterministic pipeline over
// every captured request once per iteration.
func BenchmarkDeterministicPipelineOnCapture(b *testing.B) {
	recs := loadCapture(b, 24)
	p := benchPipe(b)
	st := store.NewMemory(store.Options{})
	ctx := context.Background()
	b.ResetTimer()
	for b.Loop() {
		for _, r := range recs {
			apply.Body(ctx, p, st, bschemas.ModelProvider(r.Provider), r.Body, "", false)
		}
	}
	b.ReportMetric(float64(len(recs)), "requests/op")
}

// BenchmarkDeterministicPipelineOneRequest isolates a single median-size real request.
func BenchmarkDeterministicPipelineOneRequest(b *testing.B) {
	recs := loadCapture(b, 0)
	r := recs[len(recs)/2]
	p := benchPipe(b)
	st := store.NewMemory(store.Options{})
	ctx := context.Background()
	b.ResetTimer()
	for b.Loop() {
		apply.Body(ctx, p, st, bschemas.ModelProvider(r.Provider), r.Body, "", false)
	}
}

// TestCaptureOutputGolden records/compares the pipeline's output over real captured
// bodies, so a performance change that alters even one byte fails loudly. The golden
// file is a per-request sha256 of the output (never the body itself — captures carry
// real traffic). Written on first run to $CONTEXT_GURU_GOLDEN, compared afterwards.
func TestCaptureOutputGolden(t *testing.T) {
	golden := os.Getenv("CONTEXT_GURU_GOLDEN")
	if golden == "" {
		t.Skip("set CONTEXT_GURU_GOLDEN to a (gitignored) path to record/compare output hashes")
	}
	recs := loadCapture(t, 0)
	p := benchPipe(t)
	got := make([]string, 0, len(recs))
	for _, r := range recs {
		// Fresh store per request so results don't depend on cross-request memory order.
		st := store.NewMemory(store.Options{})
		out, changed := apply.Body(context.Background(), p, st,
			bschemas.ModelProvider(r.Provider), r.Body, "", false)
		got = append(got, sha(out)+" changed="+btoa(changed))
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		b, _ := json.Marshal(got)
		if err := os.WriteFile(golden, b, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("recorded %d output hashes to %s", len(got), golden)
		return
	}
	var prev []string
	if err := json.Unmarshal(want, &prev); err != nil {
		t.Fatal(err)
	}
	if len(prev) != len(got) {
		t.Fatalf("golden has %d entries, capture produced %d", len(prev), len(got))
	}
	for i := range got {
		if got[i] != prev[i] {
			t.Fatalf("request %d: output changed\n got %s\nwant %s", i, got[i], prev[i])
		}
	}
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func btoa(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
