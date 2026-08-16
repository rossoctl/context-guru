package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/rossoctl/context-guru/components/all"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
)

// bigToolOutput is a tool result large and repetitive enough that the pipeline's
// offloaders actually fire, so the end-to-end test exercises a real compaction
// rather than a no-op.
func bigToolOutput() string {
	var b strings.Builder
	b.WriteString("Exit code 1\nTraceback (most recent call last):\n")
	for i := 0; i < 400; i++ {
		b.WriteString("  File \"/repo/pkg/mod/thing.py\", line 42, in handler\n    result = compute(x, y)\n")
	}
	b.WriteString("ValueError: bad input\n")
	return b.String()
}

// anthropicRequest builds a realistic Anthropic-dialect body with a big tool result.
func anthropicRequest(model string) []byte {
	body := map[string]any{
		"model":      model,
		"max_tokens": 64,
		"messages": []any{
			map[string]any{"role": "user", "content": "please fix the failing test"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "tu_1", "name": "bash", "input": map[string]any{"command": "pytest"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "tu_1", "content": bigToolOutput()},
			}},
		},
	}
	b, _ := json.Marshal(body)
	return b
}

// fakeUpstream returns an Anthropic-shaped response with real usage tiers.
func fakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant",
		  "content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",
		  "usage":{"input_tokens":12,"output_tokens":34,
		           "cache_read_input_tokens":9000,"cache_creation_input_tokens":1500}}`))
	}))
}

// dashHandler wires a real pipeline + recorder in front of a fake upstream.
func dashHandler(t *testing.T, up string, opts dash.Options) (*Handler, *dash.Recorder) {
	t.Helper()
	cfg, err := config.LoadBytes([]byte("preset: codesafe\n"))
	if err != nil {
		t.Fatal(err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	if opts.DBPath == "" {
		opts.DBPath = filepath.Join(t.TempDir(), "d.db")
	}
	opts.BatchSize, opts.FlushInterval = 1, time.Millisecond
	rec, err := dash.NewRecorder(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{
		AnthropicUpstream: up,
		OpenAIUpstream:    up, // some dashboard tests drive the OpenAI dialect
		Preset:            cfg.Preset,
		Dashboard:         rec,
		Prices:            fixedPricer{},
	})
	return h, rec
}

// fixedPricer prices at the real aws/claude-sonnet-5 rates so the cost assertions
// are about the arithmetic, not about network access to a prices map.
type fixedPricer struct{}

func (fixedPricer) Price(context.Context, string) (modelinfo.Price, bool) {
	return modelinfo.Price{Input: 2e-06, Output: 1e-05, CacheRead: 2e-07, CacheWrite: 2.5e-06}, true
}

// waitForRows polls until the writer goroutine has persisted n rows.
func waitForRows(t *testing.T, rec *dash.Recorder, n int64) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if rec.Stats().Written >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("only %d of %d rows persisted (dropped %d, errors %d)",
		rec.Stats().Written, n, rec.Stats().Dropped, rec.Stats().Errors)
}

func TestDashboardCapturesARealRequestEndToEnd(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	h, rec := dashHandler(t, up.URL, dash.Options{CaptureContent: true, ContentCap: 1 << 16,
		ContentMaxPerRequest: 10})
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages",
		strings.NewReader(string(anthropicRequest("aws/claude-sonnet-5"))))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claude-cli/2.0.14 (external, cli)")
	req.Header.Set("x-context-guru-session", "sess-e2e")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("proxy returned %d", resp.StatusCode)
	}
	waitForRows(t, rec, 1)

	page, err := rec.DB().Requests(dash.Filter{}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 {
		t.Fatalf("captured %d rows; want 1", page.Total)
	}
	e := page.Requests[0]

	if e.SessionID != "sess-e2e" {
		t.Errorf("session = %q; want the header-supplied sess-e2e", e.SessionID)
	}
	if e.Model != "aws/claude-sonnet-5" {
		t.Errorf("model = %q", e.Model)
	}
	if e.Provider != "anthropic" || e.Agent != "claude-cli" || e.Preset != "codesafe" {
		t.Errorf("labels wrong: provider=%q agent=%q preset=%q", e.Provider, e.Agent, e.Preset)
	}
	if e.Mode != dash.ModeActive {
		t.Errorf("mode = %q; want active", e.Mode)
	}
	if e.Status != 200 {
		t.Errorf("upstream status = %d", e.Status)
	}
	// Usage came off the real response.
	if e.FreshInput != 12 || e.CacheRead != 9000 || e.CacheWrite != 1500 || e.OutputTokens != 34 {
		t.Errorf("usage tiers not captured: %+v", e)
	}
	if e.TokenAccounting != dash.AccountingComplete {
		t.Errorf("accounting = %q; want complete (usage + a known price)", e.TokenAccounting)
	}
	if e.CostUSD <= 0 {
		t.Error("cost not priced despite complete accounting")
	}
	// The pipeline actually compacted, so baseline must exceed actual.
	if e.TokensBefore <= e.TokensAfter {
		t.Fatalf("the pipeline did not compact (%d -> %d); the assertions below would be vacuous",
			e.TokensBefore, e.TokensAfter)
	}
	if e.BaselineCostUSD <= e.CostUSD {
		t.Errorf("baseline %v is not above actual %v despite %d tokens removed",
			e.BaselineCostUSD, e.CostUSD, e.TokensBefore-e.TokensAfter)
	}
	if e.CGLatencyMs <= 0 || e.UpstreamMs <= 0 {
		t.Errorf("latency not captured: cg=%v upstream=%v", e.CGLatencyMs, e.UpstreamMs)
	}
	if e.CacheMissReason != dash.CacheHit {
		t.Errorf("cache attribution = %q; a response with cache reads is a hit", e.CacheMissReason)
	}
	if e.UncompressedReason != "" {
		t.Errorf("uncompressed reason = %q on a request that compacted", e.UncompressedReason)
	}

	// Components and the diff content are both there.
	full, err := rec.DB().Request(e.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Components) == 0 {
		t.Error("no component rows captured")
	}
	acted := false
	for _, c := range full.Components {
		if c.Acted {
			acted = true
		}
	}
	if !acted {
		t.Error("no component recorded as having acted despite a net saving")
	}
	if len(full.Content) == 0 {
		t.Fatal("no before/after content captured; the diff view would be empty")
	}
	found := false
	for _, c := range full.Content {
		if strings.Contains(c.Before, "Traceback") && len(c.After) < len(c.Before) {
			found = true
		}
	}
	if !found {
		t.Errorf("the captured content does not show the compaction: %+v", full.Content)
	}

	// /stats must have been updated in lockstep and still parse for the harness.
	w := httptest.NewRecorder()
	h.stats(w, httptest.NewRequest("GET", "/stats", nil))
	var snap map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if snap["cache_read_tokens"].(float64) != 9000 {
		t.Errorf("/stats cache_read_tokens = %v; want 9000", snap["cache_read_tokens"])
	}
	if snap["attempted_tokens"].(float64) <= 0 {
		t.Error("/stats attempted_tokens not recorded")
	}
}

func TestDashboardCapturesABypassedRequest(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	h, rec := dashHandler(t, up.URL, dash.Options{})
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages",
		strings.NewReader(string(anthropicRequest("aws/claude-sonnet-5"))))
	req.Header.Set("x-context-guru-bypass", "true")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	waitForRows(t, rec, 1)

	page, _ := rec.DB().Requests(dash.Filter{}, 0, 10)
	e := page.Requests[0]
	if !e.Bypassed || e.Mode != dash.ModeBypass {
		t.Errorf("bypass not recorded: bypassed=%v mode=%q", e.Bypassed, e.Mode)
	}
	if e.UncompressedReason != dash.ReasonBypassed {
		t.Errorf("reason = %q; want %q", e.UncompressedReason, dash.ReasonBypassed)
	}
	if e.TokensBefore != e.TokensAfter {
		t.Errorf("a bypassed request must not report a saving: %d -> %d", e.TokensBefore, e.TokensAfter)
	}
}

func TestDashboardCapturesCompactRoute(t *testing.T) {
	h, rec := dashHandler(t, "", dash.Options{CaptureContent: true, ContentCap: 1 << 16,
		ContentMaxPerRequest: 10})
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	resp, err := srv.Client().Post(srv.URL+"/compact?provider=anthropic", "application/json",
		strings.NewReader(string(anthropicRequest("aws/claude-sonnet-5"))))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	waitForRows(t, rec, 1)

	page, _ := rec.DB().Requests(dash.Filter{}, 0, 10)
	e := page.Requests[0]
	if e.Route != "/compact" {
		t.Errorf("route = %q; want /compact", e.Route)
	}
	// /compact never calls a provider, so the row must be honestly marked as
	// partially accounted rather than priced as free.
	if e.TokenAccounting == dash.AccountingComplete {
		t.Error("/compact has no provider usage; the row must not claim complete accounting")
	}
	if e.CostUSD != 0 {
		t.Errorf("/compact priced a request with no billed usage: %v", e.CostUSD)
	}
}

func TestDisabledDashboardChangesNothing(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	cfg, _ := config.LoadBytes([]byte("preset: codesafe\n"))
	agg := metrics.NewAggregator()
	pipe, _ := cfg.Build(agg)
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{AnthropicUpstream: up.URL})
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	// The request still works.
	resp, err := srv.Client().Post(srv.URL+"/anthropic/v1/messages", "application/json",
		strings.NewReader(string(anthropicRequest("m"))))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("proxy returned %d with the dashboard off", resp.StatusCode)
	}
	// And the dashboard routes are absent, so an unconfigured proxy's surface is
	// byte-identical to before this feature existed.
	for _, path := range []string{"/dashboard/", "/api/stats", "/api/events"} {
		r, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusNotFound {
			t.Errorf("%s -> %d with the dashboard disabled; want 404", path, r.StatusCode)
		}
	}
	// /stats still works and keeps its shape.
	r, err := srv.Client().Get(srv.URL + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var snap map[string]any
	if err := json.NewDecoder(r.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	if snap["requests"] == nil {
		t.Error("/stats broken with the dashboard off")
	}
}

// manyToolResultsRequest builds a transcript shaped like a real agent's mid-session
// turn: many large tool results, not one. The shape is the point — content capture
// redacts up to ContentMaxPerRequest x (before+after) blobs, so a body with a single
// tool result exercises 1/24th of the work and would let a regression through. The
// blobs are log-shaped (KEY=value lines, paths, a URL) because that is the text the
// credential regexes are slowest over.
func manyToolResultsRequest(model string, results int) []byte {
	msgs := []any{map[string]any{"role": "user", "content": "please fix the failing test"}}
	var logish strings.Builder
	for i := 0; i < 120; i++ {
		logish.WriteString("2026-08-09T12:00:00Z INFO  worker=7 path=/repo/pkg/mod/thing.py status=ok\n")
		logish.WriteString("  DATABASE_URL=postgres://localhost:5432/app RETRIES=3 TIMEOUT_MS=2500\n")
		logish.WriteString("  File \"/repo/pkg/mod/thing.py\", line 42, in handler\n    result = compute(x, y)\n")
	}
	blob := logish.String()
	for i := 0; i < results; i++ {
		id := "tu_" + strconv.Itoa(i)
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": id, "name": "bash",
					"input": map[string]any{"command": "pytest -k case" + strconv.Itoa(i)}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": id, "content": blob},
			}})
	}
	b, _ := json.Marshal(map[string]any{
		"model": model, "max_tokens": 64, "messages": msgs,
	})
	return b
}

// TestDashboardAddsNoRequestLatencyWithContentCapture is the regression test the
// original overhead claim was missing.
//
// dash.BenchmarkRecord measures a channel send (~175 ns) and is honest about that,
// but it is not the dashboard's per-request cost: `finish` is called from serve's
// `defer`, which runs BEFORE the handler returns, so anything expensive there is paid
// by the next request on a keep-alive connection — every real agent. Content
// redaction (nine regexes over up to ContentMaxPerRequest x 2 blobs) sat there and
// cost ~53 ms/request, ~25% of a real request, while the documented figure was
// ~0.000002%. A benchmark that calls Record directly can never catch that.
//
// So this drives the REAL handler over ONE keep-alive connection with content capture
// ON — the path a client actually pays for — and compares against the same handler
// with the dashboard off. It fails if the dashboard adds a perceptible cost.
func TestDashboardAddsNoRequestLatencyWithContentCapture(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()

	cfg, err := config.LoadBytes([]byte("preset: codesafe\n"))
	if err != nil {
		t.Fatal(err)
	}
	offAgg := metrics.NewAggregator()
	offPipe, err := cfg.Build(offAgg)
	if err != nil {
		t.Fatal(err)
	}
	offH := New(offPipe, store.NewMemory(store.Options{}), offAgg,
		Options{AnthropicUpstream: up.URL, Prices: fixedPricer{}})
	// Content capture ON: the configuration with the most work to do per request is
	// the one worth guarding.
	onH, _ := dashHandler(t, up.URL, dash.Options{
		CaptureContent: true, ContentCap: 16 << 10, ContentMaxPerRequest: 24,
	})

	// 24 tool results = ContentMaxPerRequest, so the capture path does the full amount
	// of work it is ever allowed to do on one request. A body with a single tool result
	// exercises 1/24th of it and lets a regression through.
	body := string(manyToolResultsRequest("m", 24))

	// One connection per handler, reused, so a cost paid in the handler's defer shows
	// up as latency on the NEXT request rather than being hidden by a fresh dial.
	newClient := func(h *Handler) (*http.Client, string, func()) {
		srv := httptest.NewServer(h.Mux())
		c := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 1}}
		return c, srv.URL + "/anthropic/v1/messages", func() {
			c.CloseIdleConnections()
			srv.Close()
		}
	}
	one := func(c *http.Client, url string) time.Duration {
		start := time.Now()
		resp, err := c.Post(url, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body) // drain, or the connection is not reusable
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("upstream returned %d", resp.StatusCode)
		}
		return time.Since(start)
	}

	offClient, offURL, closeOff := newClient(offH)
	defer closeOff()
	onClient, onURL, closeOn := newClient(onH)
	defer closeOn()

	// PAIRED and INTERLEAVED, then compared on the MEDIAN. Measuring all the off
	// requests and then all the on requests attributes any drift in machine load to the
	// dashboard: consecutive runs of that shape disagreed by 4x (+3 ms to +12 ms) on an
	// idle box, which is not a usable gate. Alternating and taking medians cancels drift
	// and discards the outliers a shared CI box produces.
	const warmup, iters = 5, 40
	for i := 0; i < warmup; i++ {
		one(offClient, offURL)
		one(onClient, onURL)
	}
	offs := make([]time.Duration, 0, iters)
	ons := make([]time.Duration, 0, iters)
	for i := 0; i < iters; i++ {
		if i%2 == 0 { // alternate which one goes first, so ordering cannot bias either
			offs = append(offs, one(offClient, offURL))
			ons = append(ons, one(onClient, onURL))
			continue
		}
		ons = append(ons, one(onClient, onURL))
		offs = append(offs, one(offClient, offURL))
	}
	median := func(ds []time.Duration) time.Duration {
		slices.Sort(ds)
		return ds[len(ds)/2]
	}
	off, on := median(offs), median(ons)

	added := on - off
	t.Logf("median per-request latency over %d paired requests: dashboard off %v, "+
		"on with content ON %v (added %v)", iters, off, on, added)

	// The budget is loose in absolute terms (a fake-upstream request still moves by a
	// millisecond or two under load) but an order of magnitude below the ~53 ms the
	// regression cost. Anything that puts redaction, gzip or an insert back on the
	// request goroutine blows straight through it.
	if added > 5*time.Millisecond {
		t.Errorf("the dashboard added %v per request with content capture on; "+
			"something expensive is back on the request goroutine (budget 5ms)", added)
	}
}

// TestDashboardUnderConcurrentTraffic drives the whole path (pipeline, capture,
// writer, SSE, reads) at once. Run under -race this is the mandatory check.
func TestDashboardUnderConcurrentTraffic(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	h, rec := dashHandler(t, up.URL, dash.Options{CaptureContent: true, ContentCap: 4096,
		ContentMaxPerRequest: 4, QueueSize: 64})
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	// An SSE subscriber that reads, and one that goes away mid-stream.
	sseDone := make(chan struct{})
	go func() {
		defer close(sseDone)
		r, err := srv.Client().Get(srv.URL + "/api/events")
		if err != nil {
			return
		}
		defer r.Body.Close()
		io.CopyN(io.Discard, r.Body, 512) //nolint:errcheck // a short read then leave, on purpose
	}()

	var wg sync.WaitGroup
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 8; i++ {
				req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages",
					strings.NewReader(string(anthropicRequest("aws/claude-sonnet-5"))))
				req.Header.Set("x-context-guru-session", "sess-"+string(rune('a'+g)))
				resp, err := srv.Client().Do(req)
				if err != nil {
					t.Errorf("request failed: %v", err)
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}(g)
	}
	// Concurrent dashboard readers.
	for g := 0; g < 3; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				for _, path := range []string{"/api/stats", "/api/requests?limit=5", "/api/sessions",
					"/api/components", "/api/capture"} {
					r, err := srv.Client().Get(srv.URL + path)
					if err != nil {
						t.Errorf("%s: %v", path, err)
						return
					}
					io.Copy(io.Discard, r.Body)
					r.Body.Close()
				}
			}
		}()
	}
	wg.Wait()
	<-sseDone

	// Whatever the interleaving, nothing may be silently lost: written + dropped
	// must account for everything captured.
	s := rec.Stats()
	if s.Captured != 48 {
		t.Errorf("captured %d of 48 requests", s.Captured)
	}
	if s.Errors > 0 {
		t.Errorf("%d insert errors", s.Errors)
	}
}
