package proxy

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestCacheAttributionOnlyUsesKnownExpiry(t *testing.T) {
	for _, tc := range []struct {
		provider, model string
		want            int64
	}{
		{"anthropic", "claude-sonnet-5", (5 * time.Minute).Milliseconds()},
		{"openai", "azure/gpt-5.6-luna", 0},
		{"openai", "gpt-5.5", 0},
		{"gemini", "gemini-3-pro", 0},
	} {
		if got := cacheAttributionTTLMs(tc.provider, tc.model, false); got != tc.want {
			t.Errorf("%s/%s: TTL = %d, want %d", tc.provider, tc.model, got, tc.want)
		}
	}
}

func TestCacheAttributionTTLFollowsTheBilledTier(t *testing.T) {
	hour := time.Hour.Milliseconds()
	for _, tc := range []struct {
		provider, model string
		oneHour         bool
		want            int64
	}{
		{"anthropic", "claude-sonnet-5", true, hour},
		{"bedrock", "anthropic.claude-sonnet-5", true, hour},
		{"anthropic", "claude-sonnet-5", false, (5 * time.Minute).Milliseconds()},
		{"openai", "gpt-5.5", true, 0},
	} {
		if got := cacheAttributionTTLMs(tc.provider, tc.model, tc.oneHour); got != tc.want {
			t.Errorf("%s/%s oneHour=%v: TTL = %d, want %d", tc.provider, tc.model, tc.oneHour, got, tc.want)
		}
	}
}

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

// billedUpstream answers like a provider that bills the body it received: 4 bytes a token, the
// first 80% read from cache and the rest written. A saving priced by the accountant library is
// checked against the request's own size (PriceMode's plausibility bound), so a test that asserts
// a saving needs a usage that bills the forwarded body, as a real provider's does.
func billedUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		// The provider bills the prompt without the definitions of the tools deferred with defer_loading (R56).
		n := len(billedBytesOf(b)) / 4
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"msg_1","type":"message","role":"assistant",
		  "content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",
		  "usage":{"input_tokens":12,"output_tokens":34,
		           "cache_read_input_tokens":%d,"cache_creation_input_tokens":%d}}`, n*8/10, n-n*8/10-12)
	}))
}

// fakeUpstream returns an Anthropic-shaped response with real usage tiers.
func fakeUpstream(t *testing.T) *httptest.Server { return fakeUpstreamUsage(t, 12, 9000, 1500) }

// fakeUpstreamUsage is fakeUpstream with the prompt usage the caller chooses.
func fakeUpstreamUsage(t *testing.T, in, cacheRead, cacheWrite int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"msg_1","type":"message","role":"assistant",
		  "content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",
		  "usage":{"input_tokens":%d,"output_tokens":34,
		           "cache_read_input_tokens":%d,"cache_creation_input_tokens":%d}}`, in, cacheRead, cacheWrite)
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
	// A tracker started two hours ago: this test models steady-state traffic, and a
	// just-started tracker would hold the request inside the restart grace (#496), where the
	// first request of a thread reads all-cached and nothing is attempted.
	h.tracker.SetStartedAt(time.Now().Add(-2 * time.Hour).UnixMilli())
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
	// The saving itself is the accountant library's, checked against a usage that bills the body
	// (TestDashboardBooksTheAccountantSaving): this upstream's fixed usage does not, so the library's
	// figure fails the plausibility bound and the event books no saving.
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

// The dashboard and insights group rows by agent. Bob's real User-Agents (seen live
// 2026-11-02) must land as "bob" through the capture path, and others must stay as they are.
func TestDashboardLabelsBobUserAgentsAsBob(t *testing.T) {
	cases := map[string]string{
		"IBM Bob/2.3.0": "bob",
		"ai-sdk/openai-compatible/3.0.36 ai-sdk/provider-utils/5.0.29 runtime/node.js/v24.15.0 IBM Bob/2.3.0": "bob",
		"bob-shell/2.2.1":                   "bob",
		"claude-cli/2.0.14 (external, cli)": "claude-cli",
		"codex_exec/0.160.1":                "codex",
	}
	for ua, want := range cases {
		t.Run(ua, func(t *testing.T) {
			up := fakeUpstream(t)
			defer up.Close()
			h, rec := dashHandler(t, up.URL, dash.Options{})
			srv := httptest.NewServer(h.Mux())
			defer srv.Close()

			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages",
				strings.NewReader(string(anthropicRequest("aws/claude-sonnet-5"))))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("User-Agent", ua)
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			waitForRows(t, rec, 1)

			page, err := rec.DB().Requests(dash.Filter{}, 0, 10)
			if err != nil {
				t.Fatal(err)
			}
			if page.Total != 1 {
				t.Fatalf("captured %d rows; want 1", page.Total)
			}
			if got := page.Requests[0].Agent; got != want {
				t.Errorf("agent = %q; want %q", got, want)
			}
		})
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
//
// HOW IT MEASURES (#1013, #1031). It times the dashboard's OWN calls on the request goroutine
// (newCapture, the note* methods, the accountant's accBegin and accEnd, finish) through spendHook, a
// test-only hook that is nil in production. It does not time the whole request. The old test compared
// the median of paired whole-request differences against min(45 ms, max(5 ms, 20% of the baseline)).
// That failed on a slow CI runner: the baseline was 500-730 ms and the added time was 49-77 ms in
// EVERY round (#1013 reopen comment), so best-of-3 rounds (#1014) did not help. A fastest-request
// comparison failed too, on a box loaded to 16 busy loops on 4 cores: 7 of 20 runs. The old test also
// missed a 50 ms sleep in capture.finish under load (#1031), because that cost runs in the handler's
// defer and hid behind the next "off" request.
//
// Why this works. A cost on the request path, before the response or in the defer, adds its full size
// to EVERY request. Upstream time, pipeline time and the runner's speed are not in the figure. The
// test sends requests one after the other, waits until each finish has run, and takes the MEDIAN of
// the dashboard's time per request. The median ignores the few requests that the scheduler stalls.
// The guarded regression is 50-53 ms and never shrinks under load.
//
// Two checks, because the accountant's cost grows with the body (#1059):
//   - TestDashboardAddsNoRequestLatencyWithContentCapture counts ALL the dashboard's work, the
//     accountant (accBegin, accEnd) included, on a small body (2 tool results). The growth of the
//     accountant's cost with body size is tracked in #1059 and is NOT covered by this test.
//   - TestDashboardCaptureAddsNoRequestLatencyAtMaxContent runs content capture at its maximum
//     (24 tool results) and leaves the accountant out (accBegin, accEnd, accBilledTTL and accCompute),
//     because on that body the accountant costs about 24 ms per request under -race and 150 ms under
//     load (#1059). It covers the capture work.
//
// A span that sits inside another (accBilledTTL in accCompute in finish) is counted once: the hook
// takes a nested span's time out of its parent.
//
// MAINTENANCE RULE: new dashboard work on the request path must carry timed(), or this test cannot
// see it. It sees only the calls that carry timed().

// dashboardSpends sends n requests with a body of the given number of tool results through a
// dashboard with content capture on. It returns, sorted, the time each request's dashboard calls
// took on the request goroutine, leaving out the calls named in skip.
func dashboardSpends(t *testing.T, n, results int, skip ...string) []time.Duration {
	t.Helper()
	up := fakeUpstream(t)
	defer up.Close()
	// Content capture ON: the configuration with the most work to do per request is
	// the one worth guarding.
	onH, _ := dashHandler(t, up.URL, dash.Options{
		CaptureContent: true, ContentCap: 16 << 10, ContentMaxPerRequest: 24,
	})
	body := string(manyToolResultsRequest("m", results))

	srv := httptest.NewServer(onH.Mux())
	defer srv.Close()
	c := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 1}}
	defer c.CloseIdleConnections()
	url := srv.URL + "/anthropic/v1/messages"
	one := func() {
		resp, err := c.Post(url, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body) // drain, or the connection is not reusable
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("upstream returned %d", resp.StatusCode)
		}
	}

	var mu sync.Mutex
	own := map[string]time.Duration{} // time per call name, a nested span taken out of its parent
	done := make(chan struct{}, 1000)
	parent := map[string]string{"accBilledTTL": "accCompute", "accCompute": "finish"}
	hook := func(call string, d time.Duration) {
		mu.Lock()
		own[call] += d
		if p, ok := parent[call]; ok {
			own[p] -= d
		}
		mu.Unlock()
		if call == "finish" {
			done <- struct{}{}
		}
	}
	// The hook is set BEFORE the warmup, and each request waits for its own finish. finish runs in
	// the handler's defer, so the client can have the response before it runs. A finish still in
	// flight across the hook's Store would shift every sample by one.
	spendHook.Store(&hook)
	defer spendHook.Store(nil)
	request := func() {
		one()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("finish did not run")
		}
	}
	for i := 0; i < 3; i++ { // warm up
		request()
	}
	out := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		mu.Lock()
		clear(own)
		mu.Unlock()
		request()
		mu.Lock()
		var sum time.Duration
		for call, d := range own {
			if !slices.Contains(skip, call) {
				sum += d
			}
		}
		out = append(out, sum)
		mu.Unlock()
	}
	if len(done) != 0 {
		t.Fatalf("%d finish signals left over: the samples are shifted", len(done))
	}
	spendHook.Store(nil)
	slices.Sort(out)
	return out
}

func checkSpend(t *testing.T, spends []time.Duration, budget time.Duration) {
	t.Helper()
	med := spends[len(spends)/2]
	t.Logf("dashboard time per request on the request goroutine, %d requests: median %v, min %v, max %v (budget %v on the median)",
		len(spends), med, spends[0], spends[len(spends)-1], budget)
	if med > budget {
		t.Errorf("the dashboard spent a median of %v per request on the request goroutine (budget %v): something "+
			"expensive is on the request path, before the response or in the handler's defer", med, budget)
	}
}

// maxSmallBodySpend is the budget on the median, 2 tool results, accountant counted. Measured under
// -race on 4 cores, 5 runs of 50 requests each (median of the per-request times, p95 in brackets):
//   - idle: 2.5-2.6 ms (4.4-4.5 ms)
//   - under load, 16 busy loops pinned to 4 cores: 9.1-14.7 ms (30.5-35.8 ms; worst request 58.5 ms)
//
// The budget is 2x the worst median (14.7 ms), rounded up to 30 ms. That leaves grace for noise and
// stays clearly under the 50 ms a regression adds to EVERY request, so the median reaches 50 ms or more.
// The mean of the same runs reached 30 ms under load, which is why the check uses the median.
const maxSmallBodySpend = 30 * time.Millisecond

func TestDashboardAddsNoRequestLatencyWithContentCapture(t *testing.T) {
	checkSpend(t, dashboardSpends(t, 50, 2), maxSmallBodySpend)
}

// maxMaxContentSpend is the budget on the median, 24 tool results, accountant left out. Measured
// under -race on 4 cores with 16 busy loops: worst run mean 10.5 ms. The budget is 2x that, plus
// grace, and well under the 50 ms the guarded regression adds.
const maxMaxContentSpend = 25 * time.Millisecond

func TestDashboardCaptureAddsNoRequestLatencyAtMaxContent(t *testing.T) {
	checkSpend(t, dashboardSpends(t, 20, 24, "accBegin", "accEnd", "accBilledTTL", "accCompute"), maxMaxContentSpend)
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

// A real request through the proxy books the accountant library's saving: the pipeline compacted,
// the upstream billed the forwarded body, and the stored baseline is above the cost.
func TestDashboardBooksTheAccountantSaving(t *testing.T) {
	up := billedUpstream(t)
	defer up.Close()
	h, rec := dashHandler(t, up.URL, dash.Options{})
	h.tracker.SetStartedAt(time.Now().Add(-2 * time.Hour).UnixMilli())
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages",
		strings.NewReader(string(anthropicRequest("aws/claude-sonnet-5"))))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-context-guru-session", "sess-acc")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	waitForRows(t, rec, 1)
	// The first request of a thread has no history, so its saving is an estimate and is held out of
	// the books (issue #915). It is booked from the second request on.
	first, err := rec.DB().Requests(dash.Filter{}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if f := first.Requests[0]; !f.PriceEstimated || f.BaselineCostUSD != f.CostUSD || f.EstimatedSavedUSD <= 0 {
		t.Fatalf("first request: estimated %v baseline %v cost %v held %v, want an estimate held out of the books",
			f.PriceEstimated, f.BaselineCostUSD, f.CostUSD, f.EstimatedSavedUSD)
	}
	req2, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages",
		strings.NewReader(string(anthropicRequest("aws/claude-sonnet-5"))))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("x-context-guru-session", "sess-acc")
	resp2, err := srv.Client().Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	waitForRows(t, rec, 2)
	page, err := rec.DB().Requests(dash.Filter{}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	e := page.Requests[0] // newest first
	if e.PriceEstimated {
		t.Fatalf("the second request of the thread is still an estimate")
	}
	if e.TokensBefore <= e.TokensAfter {
		t.Fatalf("the pipeline did not compact (%d -> %d)", e.TokensBefore, e.TokensAfter)
	}
	if e.BaselineCostUSD <= e.CostUSD {
		t.Fatalf("baseline %v is not above actual %v despite %d tokens removed",
			e.BaselineCostUSD, e.CostUSD, e.TokensBefore-e.TokensAfter)
	}
	full, err := rec.DB().Request(e.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	priced := false
	for _, c := range full.Components {
		priced = priced || c.PricedBy == dash.PricedByAccountant
	}
	if !priced {
		t.Fatalf("no component row is priced by the accountant library: %+v", full.Components)
	}
}
