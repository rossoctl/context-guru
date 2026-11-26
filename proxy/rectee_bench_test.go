package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The cost question, stated as a benchmark: what does attaching the tee do to a streamed
// response, and what does the OFF path cost relative to today's code?
//
// benchStream drives streamRec over an in-memory upstream of a realistic SSE turn.
func benchStream(b *testing.B, bodySize int, teeCap int) {
	body := strings.Repeat("event: content_block_delta\ndata: {\"delta\":{\"text\":\"xxxxxxxxxxxxxxxx\"}}\n\n",
		bodySize/72)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(body))
	}))
	defer up.Close()
	h := &Handler{}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := http.Get(up.URL)
		if err != nil {
			b.Fatal(err)
		}
		var tee *recTee
		if teeCap > 0 {
			tee = newRecTee(teeCap)
		}
		h.streamRec(httptest.NewRecorder(), resp, tee)
	}
}

// ~64 KB turn: a typical Claude Code response.
func BenchmarkStreamRecordOff64K(b *testing.B) { benchStream(b, 64<<10, 0) }
func BenchmarkStreamRecordOn64K(b *testing.B)  { benchStream(b, 64<<10, 8<<20) }

// ~1 MB turn: a long tool-heavy response.
func BenchmarkStreamRecordOff1M(b *testing.B) { benchStream(b, 1<<20, 0) }
func BenchmarkStreamRecordOn1M(b *testing.B)  { benchStream(b, 1<<20, 8<<20) }
