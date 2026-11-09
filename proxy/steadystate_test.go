package proxy_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/internal/tokens"
)

// A conversation of many distinct ~5 KB messages, built from this repository's own source so the
// text tokenizes like code.
var steadyCorpus string

func steadyChunk(i, n int) string {
	if steadyCorpus == "" {
		files, _ := filepath.Glob("../*/*.go")
		var sb strings.Builder
		for _, f := range files {
			b, _ := os.ReadFile(f)
			sb.Write(b)
		}
		steadyCorpus = sb.String()
	}
	off := (i * 7919) % (len(steadyCorpus) - n)
	return steadyCorpus[off : off+n]
}

func steadyMsgs(seed, count, size int) []map[string]any {
	var m []map[string]any
	for i := 0; i < count; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		m = append(m, map[string]any{"role": role, "content": steadyChunk(seed*100000+i, size)})
	}
	return m
}

func steadyHandler(b *testing.B) http.Handler {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	b.Cleanup(up.Close)
	h, _ := buildHandler(&testing.T{}, "preset: house\n", up.URL)
	return h.Mux()
}

func steadyPost(b *testing.B, h http.Handler, m []map[string]any) {
	b.StopTimer() // building the request is the client's work, not the proxy's
	body, _ := json.Marshal(map[string]any{"model": "gpt-x", "messages": m})
	b.StartTimer()
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("x-context-guru-session", "steady")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		b.Fatalf("status %d", rec.Code)
	}
}

// BenchmarkSteadyTurn is the proxy's own per-turn overhead for a session that has already been
// seen: turn 1 warms the token memo, then each iteration sends the transcript plus one new
// message. The upstream is a local stub, so what is measured is the proxy's work.
func benchSteady(b *testing.B, mib int) {
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.Cleanup(func() { slog.SetDefault(old) })
	h := steadyHandler(b)
	const size = 5 << 10
	m := steadyMsgs(1, (mib<<20)/size, size)
	steadyPost(b, h, m) // turn 1, uncounted
	b.ResetTimer()
	s0 := tokens.Stats()
	for i := 0; i < b.N; i++ {
		m = append(m, steadyMsgs(50+i, 1, size)[0])
		steadyPost(b, h, m)
	}
	s := tokens.Stats()
	b.ReportMetric(float64(s.Hits-s0.Hits)/float64(b.N), "memo-hits/turn")
	b.ReportMetric(float64(s.Misses-s0.Misses)/float64(b.N), "memo-misses/turn")
}

func BenchmarkSteadyTurn1MiB(b *testing.B) { benchSteady(b, 1) }
func BenchmarkSteadyTurn4MiB(b *testing.B) { benchSteady(b, 4) }
