package proxy

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/agent"
	"github.com/rossoctl/context-guru/agent/bob"
	"github.com/rossoctl/context-guru/metrics"
)

// Real-shaped ids from Bob's model route (2026-11-09). The characters are made up; the shape is not.
const (
	bobIDA = "chatcmpl-0f3c9a1e-5b7d-4e2a-9c1f-8d6b2a4e7c10" // backend A: chatcmpl-, 45 chars
	bobIDB = "nb3xQ7kLm2PzR8tYv4WcJ1s"                       // backend B: 23 chars of [A-Za-z0-9]
	// The canaries must never reach a log line.
	bobCanaryID   = "chatcmpl-CANARY0ID0abcdefghijklmnopqrstuvwxyz"
	bobCanaryText = "CANARY-ANSWER-TEXT-7731"
	// Backend B's usage block: no cache writes, no cache read, some reasoning tokens.
	bobBUsage = `{"completion_tokens":50,"completion_tokens_details":{"reasoning_tokens":30},` +
		`"prompt_tokens":12440,"prompt_tokens_details":{"cached_tokens":0,"text_tokens":12440},"total_tokens":12490}`
)

// bobLogs sends every log record to a buffer for the test.
func bobLogs(t *testing.T) *syncBuffer {
	t.Helper()
	logs := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	bob.ResetBackendForTest()
	return logs
}

func bobLabelCount(t *testing.T, label string) int64 {
	t.Helper()
	return bob.BackendCounts()[label]
}

func TestBobRouteRowRecordsTheBackend(t *testing.T) {
	for _, tc := range []struct {
		name, id, usage, want string
		psf                   bool
	}{
		{"A", bobIDA, bobColdUsage, "A", true},
		{"B", bobIDB, bobBUsage, "B", false},
	} {
		for _, sse := range []bool{false, true} {
			t.Run(tc.name+"-"+transport(sse), func(t *testing.T) {
				logs := bobLogs(t)
				before := bobLabelCount(t, tc.want)
				srv, rec := bobUpstreamHandler(t, replyWith(sse, bobReplyBytes(sse, tc.id, tc.usage, tc.psf)))
				row := rowAt(t, srv, rec, bobRoute, chatBody(sse), map[string]string{"X-Task-Id": bobIDETask})
				if row.Backend != tc.want {
					t.Fatalf("row backend = %q, want %q", row.Backend, tc.want)
				}
				if got := bobLabelCount(t, tc.want); got != before+1 {
					t.Errorf("bob_backend.%s = %d, want %d", tc.want, got, before+1)
				}
				if strings.Contains(logs.String(), "cg.bob_backend_unknown") {
					t.Errorf("a known backend logged a WARN: %s", logs.String())
				}
			})
		}
	}
}

func replyWith(sse bool, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if sse {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		w.Write(body)
	}
}

// A chatcmpl- id with B's usage block: the marks disagree. The row says "other", the counter
// moves on each reply, and the WARN comes once, with no id and no answer text in it.
func TestBobRouteUnknownShapeWarnsOnceAndCounts(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(transport(sse), func(t *testing.T) {
			logs := bobLogs(t)
			before := bobLabelCount(t, "other")
			srv, rec := bobUpstreamHandler(t, replyWith(sse, bobReplyBytes(sse, bobCanaryID, bobBUsage, true)))
			for i := 0; i < 2; i++ {
				row := rowAt(t, srv, rec, bobRoute, chatBody(sse), map[string]string{"X-Task-Id": bobIDETask})
				if row.Backend != "other" {
					t.Fatalf("reply %d: row backend = %q, want other", i, row.Backend)
				}
			}
			if got := bobLabelCount(t, "other"); got != before+2 {
				t.Errorf("bob_backend.other = %d, want %d", got, before+2)
			}
			out := logs.String()
			if n := strings.Count(out, "cg.bob_backend_unknown"); n != 1 {
				t.Fatalf("WARN lines = %d, want 1:\n%s", n, out)
			}
			want := "msg=cg.bob_backend_unknown prefix=chat len=45 alnum=false cache_write=false " +
				"creation_details=false mixed_ids=false provider_fields=true"
			if !strings.Contains(out, want) {
				t.Errorf("WARN line does not contain %q:\n%s", want, out)
			}
			if strings.Contains(out, bobCanaryID) || strings.Contains(out, "CANARY0ID") || strings.Contains(out, bobCanaryText) {
				t.Errorf("the log carries the reply id or text:\n%s", out)
			}
		})
	}
}

// Bob's free router helper calls: an empty id, no usage, a `metadata` key. No label, no WARN,
// counted as none. On the buffered path (tools) too, which reads no usage at all here.
func TestBobRouterReplyHasNoLabelAndNoWarn(t *testing.T) {
	router := []byte(`{"id":"","object":"chat.completion","model":"premium-ide","metadata":{"route":"x"},` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"` + bobCanaryText + `"},"finish_reason":"stop"}]}`)
	tools := `{"model":"premium","messages":[{"role":"user","content":"fix it"}],` +
		`"tools":[{"type":"function","function":{"name":"read_file","description":"Read a file.","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`
	for name, body := range map[string]string{"stream-through": chatBody(false), "buffered": tools} {
		t.Run(name, func(t *testing.T) {
			logs := bobLogs(t)
			before := bobLabelCount(t, "none")
			srv, rec := bobUpstreamHandler(t, replyWith(false, router))
			row := rowAt(t, srv, rec, bobRoute, body, map[string]string{"X-Task-Id": bobIDETask})
			if row.Backend != "" {
				t.Fatalf("router row backend = %q, want empty", row.Backend)
			}
			if got := bobLabelCount(t, "none"); got != before+1 {
				t.Errorf("bob_backend.none = %d, want %d", got, before+1)
			}
			if strings.Contains(logs.String(), "cg.bob_backend_unknown") {
				t.Errorf("a router reply logged a WARN: %s", logs.String())
			}
		})
	}
}

// The buffered path (a body with tools) labels a reply too.
func TestBobRouteBufferedPathRecordsTheBackend(t *testing.T) {
	bobLogs(t)
	srv, rec := bobUpstreamHandler(t, replyWith(false, bobReplyBytes(false, bobIDB, bobBUsage, false)))
	body := `{"model":"premium","messages":[{"role":"user","content":"fix it"}],` +
		`"tools":[{"type":"function","function":{"name":"read_file","description":"Read a file.","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`
	if row := rowAt(t, srv, rec, bobRoute, body, map[string]string{"X-Task-Id": bobIDETask}); row.Backend != "B" {
		t.Fatalf("buffered row backend = %q, want B", row.Backend)
	}
}

// The same A reply on the OpenAI route records no label and moves no counter.
func TestNonBobRouteRecordsNoBackend(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(transport(sse), func(t *testing.T) {
			logs := bobLogs(t)
			before := bob.BackendCounts()
			srv, rec := bobUpstreamHandler(t, replyWith(sse, bobReplyBytes(sse, bobIDA, bobColdUsage, true)))
			row := rowAt(t, srv, rec, "/openai/v1/chat/completions", chatBody(sse), nil)
			if row.Backend != "" {
				t.Fatalf("/openai row backend = %q, want empty", row.Backend)
			}
			after := bob.BackendCounts()
			for _, l := range []string{"A", "B", "other", "none", "id_no_usage"} {
				if after[l] != before[l] {
					t.Errorf("bob_backend.%s moved on the OpenAI route: %d -> %d", l, before[l], after[l])
				}
			}
			if strings.Contains(logs.String(), "cg.bob_backend_unknown") {
				t.Errorf("the OpenAI route logged a Bob WARN")
			}
		})
	}
}

// /stats always has bob_backend with its four keys, also before any Bob traffic.
func TestStatsHasBobBackendWithNoBobTraffic(t *testing.T) {
	bobLogs(t)
	w := httptest.NewRecorder()
	New(nil, nil, metrics.NewAggregator(), Options{}).stats(w, httptest.NewRequest("GET", "/stats", nil))
	var st map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	// The counts are process-wide, so earlier tests may have moved them: check the keys.
	var bb map[string]int64
	if err := json.Unmarshal(st["bob_backend"], &bb); err != nil || len(bb) != 5 {
		t.Fatalf("/stats bob_backend = %s (%v), want an object with 5 counts", st["bob_backend"], err)
	}
	for _, l := range []string{"A", "B", "other", "none", "id_no_usage"} {
		if _, ok := bb[l]; !ok {
			t.Errorf("/stats bob_backend has no %q: %s", l, st["bob_backend"])
		}
	}
}

// /stats shows the counts under bob_backend once Bob traffic has been seen.
func TestStatsShowsBobBackendCounts(t *testing.T) {
	bobLogs(t)
	srv, rec := bobUpstreamHandler(t, replyWith(true, bobReplyBytes(true, bobIDA, bobColdUsage, true)))
	rowAt(t, srv, rec, bobRoute, chatBody(true), map[string]string{"X-Task-Id": bobIDETask})
	resp, err := srv.Client().Get(srv.URL + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st struct {
		BobBackend map[string]int64 `json:"bob_backend"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	want := bob.BackendCounts()
	if st.BobBackend["A"] < 1 || len(st.BobBackend) != 5 {
		t.Fatalf("/stats bob_backend = %v, want A >= 1 and the keys A, B, other, none, id_no_usage", st.BobBackend)
	}
	for _, l := range []string{"A", "B", "other", "none", "id_no_usage"} {
		if st.BobBackend[l] != want[l] {
			t.Errorf("bob_backend.%s = %d, want %s", l, st.BobBackend[l], strconv.FormatInt(want[l], 10))
		}
	}
}

// A stream over the sniffer window is read as head + tail, and both cut a chunk. At no cut offset
// may that cut chunk give a partial id, which would read as a mixed-id stream and a false "other".
func TestBobBackendOfALongStreamIsRightAtEveryCutOffset(t *testing.T) {
	id := "chatcmpl-0123456789abcdefghijklmnopqrstuvwxyz"
	chunk := `data: {"id":"` + id + `","object":"chat.completion.chunk","provider_specific_fields":{},"choices":[{"delta":{"content":"word "}}]}` + "\n\n"
	usage := `data: {"id":"` + id + `","choices":[],"usage":{"prompt_tokens":17642,"prompt_tokens_details":{"cached_tokens":17393,"cache_write_tokens":245,"cache_creation_token_details":{}},"completion_tokens":10}}` + "\n\ndata: [DONE]\n"
	bad := map[string]int{}
	for pad := 0; pad < len(chunk); pad++ {
		var b strings.Builder
		b.WriteString(strings.Repeat("x", pad)) // shift where both cuts fall
		for b.Len() < 3*sniffMax {
			b.WriteString(chunk)
		}
		b.WriteString(usage)
		s := newSniffer(true)
		s.write([]byte(b.String()))
		if !s.spliced() {
			t.Fatal("stream not spliced; the test proves nothing")
		}
		if got := (bob.Bob{}).ReadUsage("text/event-stream", s.bytes(), agent.Usage{}).Backend; got != "A" {
			bad[got]++
		}
	}
	if len(bad) > 0 {
		t.Errorf("a long stream got a wrong label at some cut offsets: %v", bad)
	}
}

// A reply with an id but no usage block keeps the empty label and logs no WARN (unchanged). It is
// counted apart, in id_no_usage, so a backend that stops sending usage shows as a rising number,
// not as router traffic. A router reply (empty id) is not counted there.
func TestBobReplyWithAnIDButNoUsageIsCountedApart(t *testing.T) {
	noUsage := []byte(`{"id":"chatcmpl-0123456789abcdefghijklmnopqrstuvwxyz","object":"chat.completion",` +
		`"model":"premium-ide","choices":[{"index":0,"message":{"role":"assistant","content":"` + bobCanaryText + `"},"finish_reason":"stop"}]}`)
	router := []byte(`{"id":"","object":"chat.completion","model":"premium-ide","metadata":{"route":"x"},` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}]}`)
	for name, c := range map[string]struct {
		reply []byte
		bump  int64
	}{"id but no usage": {noUsage, 1}, "router": {router, 0}} {
		t.Run(name, func(t *testing.T) {
			logs := bobLogs(t)
			before := bobLabelCount(t, "id_no_usage")
			srv, rec := bobUpstreamHandler(t, replyWith(false, c.reply))
			row := rowAt(t, srv, rec, bobRoute, chatBody(false), map[string]string{"X-Task-Id": bobIDETask})
			if row.Backend != "" {
				t.Fatalf("backend = %q, want empty (unchanged)", row.Backend)
			}
			if got := bobLabelCount(t, "id_no_usage"); got != before+c.bump {
				t.Errorf("bob_backend.id_no_usage = %d, want %d", got, before+c.bump)
			}
			if strings.Contains(logs.String(), "cg.bob_backend_unknown") {
				t.Errorf("logged a WARN: %s", logs.String())
			}
		})
	}
}

// bobReplyBytes is a reply from Bob's model route. psf adds the top-level provider_specific_fields
// that backend A sends. The answer text is the canary.
func bobReplyBytes(sse bool, id, usage string, psf bool) []byte {
	extra := ""
	if psf {
		extra = `,"provider_specific_fields":{}`
	}
	if !sse {
		return []byte(`{"id":"` + id + `","object":"chat.completion","model":"premium-ide","system_fingerprint":""` + extra +
			`,"choices":[{"index":0,"message":{"role":"assistant","content":"` + bobCanaryText + `"},"finish_reason":"stop"}],"usage":` + usage + `}`)
	}
	return []byte(`data: {"id":"` + id + `","object":"chat.completion.chunk","model":"premium-ide"` + extra +
		`,"choices":[{"index":0,"delta":{"role":"assistant","content":"` + bobCanaryText + `"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"` + id + `","object":"chat.completion.chunk","model":"premium-ide"` + extra +
		`,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		`data: {"id":"` + id + `","object":"chat.completion.chunk","model":"premium-ide"` + extra +
		`,"choices":[],"usage":` + usage + `}` + "\n\n" +
		"data: [DONE]\n\n")
}
