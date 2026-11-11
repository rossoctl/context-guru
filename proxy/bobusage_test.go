package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/providerusage"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
)

// Real usage blocks from Bob's model route (2026-11-04), counts only.
const (
	bobColdUsage = `{"cache_creation_input_tokens":20315,"cache_read_input_tokens":0,"cache_validity":false,` +
		`"completion_tokens":12,"credits":0.040662,"prompt_tokens":20319,` +
		`"prompt_tokens_details":{"cache_creation_token_details":{"ephemeral_1h_input_tokens":20157,` +
		`"ephemeral_5m_input_tokens":158},"cache_creation_tokens":20315,"cache_write_tokens":20315,` +
		`"cached_tokens":0,"text_tokens":4},"total_tokens":20331}`
	bobWarmUsage = `{"cache_creation_input_tokens":99,"cache_read_input_tokens":20315,"cache_validity":true,` +
		`"completion_tokens":12,"prompt_tokens":20418,` +
		`"prompt_tokens_details":{"cache_creation_token_details":{"ephemeral_1h_input_tokens":0,` +
		`"ephemeral_5m_input_tokens":99},"cache_creation_tokens":99,"cache_write_tokens":99,` +
		`"cached_tokens":20315,"text_tokens":4},"total_tokens":20430}`
)

// bobBase is what the shared parser makes of a block, as parseUsageWhy builds it.
func bobBase(t *testing.T, raw string) Usage {
	t.Helper()
	u, why := parseUsageWhy([]byte(`{"usage":` + raw + `}`))
	if why != usageMissNone {
		t.Fatalf("shared parser rejected the block: %v", why)
	}
	return u
}

func sameUsage(a, b Usage) bool {
	return a.FreshInput == b.FreshInput && a.CacheRead == b.CacheRead && a.CacheWrite == b.CacheWrite &&
		a.CacheWrite1h == b.CacheWrite1h && a.Output == b.Output && a.StopReason == b.StopReason
}

func TestBobUsageReadsTheColdWrite(t *testing.T) {
	got := bobUsage(gjson.Parse(bobColdUsage), bobBase(t, bobColdUsage))
	want := Usage{FreshInput: 4, CacheRead: 0, CacheWrite: 20315, CacheWrite1h: 20157, Output: 12}
	if !sameUsage(got, want) {
		t.Fatalf("bobUsage(cold) = %+v, want %+v", got, want)
	}
}

func TestBobUsageReadsTheWarmWrite(t *testing.T) {
	got := bobUsage(gjson.Parse(bobWarmUsage), bobBase(t, bobWarmUsage))
	want := Usage{FreshInput: 4, CacheRead: 20315, CacheWrite: 99, CacheWrite1h: 0, Output: 12}
	if !sameUsage(got, want) {
		t.Fatalf("bobUsage(warm) = %+v, want %+v", got, want)
	}
}

func TestBobUsageFallsBackToCacheCreationFields(t *testing.T) {
	for name, raw := range map[string]string{
		"cache_creation_tokens":       `{"prompt_tokens":100,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":10,"cache_creation_tokens":60}}`,
		"cache_creation_input_tokens": `{"prompt_tokens":100,"completion_tokens":1,"cache_creation_input_tokens":60,"prompt_tokens_details":{"cached_tokens":10}}`,
	} {
		got := bobUsage(gjson.Parse(raw), bobBase(t, raw))
		want := Usage{FreshInput: 30, CacheRead: 10, CacheWrite: 60, Output: 1}
		if !sameUsage(got, want) {
			t.Errorf("%s: got %+v, want %+v", name, got, want)
		}
	}
}

func TestBobUsageLeavesAPlainOpenAIBlockUnchanged(t *testing.T) {
	raw := `{"prompt_tokens":1000,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":800}}`
	base := bobBase(t, raw)
	base.StopReason = "stop"
	if got := bobUsage(gjson.Parse(raw), base); !sameUsage(got, base) {
		t.Fatalf("bobUsage changed a block with no write fields: %+v, want %+v", got, base)
	}
}

func TestBobUsageLeavesGarbageUnchanged(t *testing.T) {
	base := Usage{FreshInput: 7, CacheRead: 3, Output: 2, StopReason: "stop"}
	for name, raw := range map[string]string{
		"not json":          `not json`,
		"array":             `[1,2,3]`,
		"string write":      `{"prompt_tokens":10,"prompt_tokens_details":{"cache_write_tokens":"lots"}}`,
		"negative write":    `{"prompt_tokens":10,"prompt_tokens_details":{"cache_write_tokens":-5}}`,
		"write over prompt": `{"prompt_tokens":10,"prompt_tokens_details":{"cache_write_tokens":50}}`,
		"1h over write":     `{"prompt_tokens":100,"prompt_tokens_details":{"cache_write_tokens":10,"cache_creation_token_details":{"ephemeral_1h_input_tokens":20}}}`,
		"no prompt_tokens":  `{"input_tokens":10,"cache_creation_input_tokens":5}`,
		"empty":             `{}`,
	} {
		if got := bobUsage(gjson.Parse(raw), base); !sameUsage(got, base) {
			t.Errorf("%s: bobUsage = %+v, want base %+v", name, got, base)
		}
	}
}

// The shared parser is NOT changed by the Bob fix: pin what it makes of Bob's cold block today.
func TestSharedParserStillIgnoresBobWrites(t *testing.T) {
	got, ok := providerusage.Parse(gjson.Parse(bobColdUsage))
	want := providerusage.Tiers{FreshInput: 20319, CacheRead: 0, CacheWrite: 0, CacheWrite1h: 0, Output: 12}
	if !ok || got != want {
		t.Fatalf("providerusage.Parse(cold) = %+v ok=%v, want %+v", got, ok, want)
	}
}

// bobUsageHandler is bobSessionHandler with an upstream that answers every chat route with the
// cold block, as JSON or as an OpenAI chat event stream.
func bobUsageHandler(t *testing.T, sse bool) (*httptest.Server, *dash.Recorder) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sse {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte(`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}` + "\n\n"))
			w.Write([]byte(`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"))
			w.Write([]byte(`data: {"id":"c1","object":"chat.completion.chunk","choices":[],"usage":` + bobColdUsage + `}` + "\n\n"))
			w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":` + bobColdUsage + `}`))
	}))
	t.Cleanup(up.Close)
	cfg, err := config.LoadBytes([]byte("preset: codesafe\n"))
	if err != nil {
		t.Fatal(err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := dash.NewRecorder(dash.Options{DBPath: t.TempDir() + "/d.db", BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{
		AnthropicUpstream: up.URL, OpenAIUpstream: up.URL, BobUpstream: up.URL,
		Preset: cfg.Preset, Dashboard: rec, Prices: fixedPricer{},
	})
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h.Mux())
	t.Cleanup(srv.Close)
	return srv, rec
}

// rowAt sends one request and returns its dashboard row.
func rowAt(t *testing.T, srv *httptest.Server, rec *dash.Recorder, path, body string, hdr map[string]string) *dash.Event {
	t.Helper()
	before := rec.Stats().Written
	req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s returned %d", path, resp.StatusCode)
	}
	waitForRows(t, rec, before+1)
	page, err := rec.DB().Requests(dash.Filter{}, 0, 1)
	if err != nil || len(page.Requests) != 1 {
		t.Fatalf("newest row: %v %v", page, err)
	}
	return page.Requests[0]
}

// chatBody is an OpenAI chat body; stream adds what Bob sends for `premium-ide`.
func chatBody(stream bool) string {
	if !stream {
		return bobBody("fix it", "")
	}
	return `{"model":"premium-ide","stream":true,"stream_options":{"include_usage":true},` +
		`"messages":[{"role":"system","content":"You are Bob."},{"role":"user","content":"fix it"}]}`
}

func transport(sse bool) string {
	if sse {
		return "sse"
	}
	return "json"
}

func TestBobRouteRowReadsCacheWrites(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(transport(sse), func(t *testing.T) {
			srv, rec := bobUsageHandler(t, sse)
			row := rowAt(t, srv, rec, bobRoute, chatBody(sse), map[string]string{"X-Task-Id": bobIDETask})
			if row.FreshInput != 4 || row.CacheRead != 0 || row.CacheWrite != 20315 || row.CacheWrite1h != 20157 || row.OutputTokens != 12 {
				t.Fatalf("row = fresh %d read %d write %d write1h %d out %d; want 4/0/20315/20157/12",
					row.FreshInput, row.CacheRead, row.CacheWrite, row.CacheWrite1h, row.OutputTokens)
			}
			resp, err := srv.Client().Get(srv.URL + "/api/cachestate?session=" + bobIDETask)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var cs map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&cs); err != nil {
				t.Fatalf("/api/cachestate: status %d: %v", resp.StatusCode, err)
			}
			if cs["ttl_s"] != float64(3600) || cs["cache_write"] != float64(20315) {
				t.Fatalf("/api/cachestate ttl_s = %v cache_write = %v, want 3600 and 20315 (body %v)",
					cs["ttl_s"], cs["cache_write"], cs)
			}
		})
	}
}

// A body with tools gets the proxy's expand tool, so serve reads the response whole (the
// buffered path) rather than streaming it through.
func TestBobRouteRowReadsCacheWritesOnTheBufferedPath(t *testing.T) {
	srv, rec := bobUsageHandler(t, false)
	body := `{"model":"premium","messages":[{"role":"system","content":"You are Bob."},{"role":"user","content":"fix it"}],` +
		`"tools":[{"type":"function","function":{"name":"read_file","description":"Read a file.","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`
	row := rowAt(t, srv, rec, bobRoute, body, map[string]string{"X-Task-Id": bobIDETask})
	if row.FreshInput != 4 || row.CacheRead != 0 || row.CacheWrite != 20315 || row.CacheWrite1h != 20157 || row.OutputTokens != 12 {
		t.Fatalf("row = fresh %d read %d write %d write1h %d out %d; want 4/0/20315/20157/12",
			row.FreshInput, row.CacheRead, row.CacheWrite, row.CacheWrite1h, row.OutputTokens)
	}
}

// The user's constraint: the same block on any other route is read exactly as before.
func TestNonBobRoutesIgnoreBobCacheWrites(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run("openai-"+transport(sse), func(t *testing.T) {
			srv, rec := bobUsageHandler(t, sse)
			row := rowAt(t, srv, rec, "/openai/v1/chat/completions", chatBody(sse), map[string]string{"X-Task-Id": bobIDETask})
			if row.FreshInput != 20319 || row.CacheRead != 0 || row.CacheWrite != 0 || row.CacheWrite1h != 0 || row.OutputTokens != 12 {
				t.Fatalf("/openai row = fresh %d read %d write %d write1h %d out %d; want today's 20319/0/0/0/12",
					row.FreshInput, row.CacheRead, row.CacheWrite, row.CacheWrite1h, row.OutputTokens)
			}
		})
	}
	// The block has no input_tokens, so the shared parser reads it on the Anthropic route through
	// its prompt_tokens branch, exactly as it does today.
	t.Run("anthropic-json", func(t *testing.T) {
		srv, rec := bobUsageHandler(t, false)
		row := rowAt(t, srv, rec, "/anthropic/v1/messages",
			`{"model":"claude-x","max_tokens":8,"system":"sys","messages":[{"role":"user","content":"fix it"}]}`, nil)
		if row.FreshInput != 20319 || row.CacheRead != 0 || row.CacheWrite != 0 || row.CacheWrite1h != 0 || row.OutputTokens != 12 {
			t.Fatalf("/anthropic row = fresh %d read %d write %d write1h %d out %d; want today's 20319/0/0/0/12",
				row.FreshInput, row.CacheRead, row.CacheWrite, row.CacheWrite1h, row.OutputTokens)
		}
	})
}
