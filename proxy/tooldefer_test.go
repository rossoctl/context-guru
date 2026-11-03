package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
	"github.com/rossoctl/context-guru/tooldefer"
	"github.com/tidwall/gjson"
)

func tdDecl(name string) string {
	return fmt.Sprintf(`{"name":%q,"description":"Does the %s job. %s","input_schema":{"type":"object","properties":{"x":{"type":"string"}}}}`,
		name, name, strings.Repeat("Detailed usage notes for this tool. ", 25))
}

func tdBody(extra ...string) []byte {
	msgs := `{"role":"user","content":"hi"},{"role":"system","content":"# Environment\nPrimary working directory: /w"}`
	for _, m := range extra {
		msgs += "," + m
	}
	return []byte(`{"model":"claude-sonnet-5-5","max_tokens":64,"metadata":{"user_id":"{\"session_id\":\"sess-1\"}"},` +
		`"tools":[` + tdDecl("Bash") + `,` + tdDecl("Read") + `,` + tdDecl("Workflow") + `,` + tdDecl("CronCreate") + `],` +
		`"messages":[` + msgs + `]}`)
}

// tdUpstream answers like the gateway. reject400 makes it refuse any body with defer_loading, the way
// a model without support does. It answers a probe as the API would.
type tdUpstream struct {
	mu          sync.Mutex
	bodies      [][]byte
	reject400   bool
	rejectOther bool // a 400 that does not name a deferral cause
	rejectAll   bool // a 400 for every request but a probe
}

func (u *tdUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.bodies = append(u.bodies, b)
	reject, rejectOther, rejectAll := u.reject400, u.rejectOther, u.rejectAll
	u.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if probe := gjson.GetBytes(b, "tools.1.name").String(); strings.HasPrefix(probe, "cg_probe_") {
		fmt.Fprintf(w, `{"content":[{"type":"server_tool_use","id":"srvtoolu_1","name":"tool_search_tool_bm25","input":{}},`+
			`{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_1","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":%q}]}},`+
			`{"type":"tool_use","id":"t","name":%q,"input":{"code":7}}],"usage":{"input_tokens":10,"output_tokens":5}}`, probe, probe)
		return
	}
	if reject && bytes.Contains(b, []byte(`"defer_loading"`)) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"tools.2.custom.defer_loading: Extra inputs are not permitted"}}`)
		return
	}
	if rejectAll {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"messages.3: text content blocks must be non-empty"}}`)
		return
	}
	if rejectOther && bytes.Contains(b, []byte(`"defer_loading"`)) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"messages.3: text content blocks must be non-empty"}}`)
		return
	}
	fmt.Fprint(w, `{"type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":5,"cache_read_input_tokens":3000,"output_tokens":2}}`)
}

func (u *tdUpstream) last() []byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	for i := len(u.bodies) - 1; i >= 0; i-- {
		if !strings.Contains(string(u.bodies[i]), "cg_probe_") {
			return u.bodies[i]
		}
	}
	return nil
}

func (u *tdUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	n := 0
	for _, b := range u.bodies {
		if !strings.Contains(string(b), "cg_probe_") {
			n++
		}
	}
	return n
}

// tdPrices prices every Claude model, so the probe runs (only priced models are probed).
type tdPrices struct{}

func (tdPrices) Price(_ context.Context, model string) (modelinfo.Price, bool) {
	if !strings.Contains(model, "claude") {
		return modelinfo.Price{}, false
	}
	return modelinfo.Price{Input: 3e-6, Output: 15e-6, CacheRead: 0.3e-6, CacheWrite: 3.75e-6}, true
}

func tdHandler(t *testing.T, preset, upstream string) *Handler {
	t.Helper()
	return tdHandlerAt(t, preset, upstream, filepath.Join(t.TempDir(), tooldefer.FileName))
}

// tdHandlerAt is a handler with a memory-only session store (no dashboard) whose tool deferral file
// is at path. A second handler on the same path is the same proxy after a restart.
func tdHandlerAt(t *testing.T, preset, upstream, path string) *Handler {
	t.Helper()
	cfg, err := config.LoadBytes([]byte("preset: " + preset + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{AnthropicUpstream: upstream,
		ToolDeferPath: path, Prices: tdPrices{}})
	t.Cleanup(h.Close)
	// Three past sessions that declared Workflow and CronCreate and used neither.
	ds := h.toolDefer()
	for i, s := range []string{"p1", "p2", "p3"} {
		ds.hist.Observe("", s, "/w", tdBody(), int64(i+1))
	}
	ds.prober.Go = func(fn func()) { fn() }
	return h
}

func tdPost(t *testing.T, srv *httptest.Server, body []byte) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func tdTools(b []byte) string {
	var out []string
	gjson.GetBytes(b, "tools").ForEach(func(_, t gjson.Result) bool {
		n := t.Get("name").String()
		if t.Get("defer_loading").Bool() {
			n += "*"
		}
		out = append(out, n)
		return true
	})
	return strings.Join(out, ",")
}

// In ultrahigh, the first request probes, the probe passes, and the thread's first request (sent
// before the probe finished) decided nothing for that thread. A NEW thread then defers.
func TestToolDeferUltrahighEndToEnd(t *testing.T) {
	up := &tdUpstream{}
	us := httptest.NewServer(up)
	defer us.Close()
	h := tdHandler(t, "ultrahigh", us.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	// Session 1, first request: no probe verdict yet. Sent as is, and the probe runs.
	tdPost(t, srv, tdBody())
	if got := tdTools(up.last()); strings.Contains(got, "*") {
		t.Fatalf("deferred before the probe passed: %s", got)
	}
	if h.toolDefer().prober.Verdict(tooldefer.ProbeKey{Upstream: us.URL, Model: "claude-sonnet-5-5", Route: "/v1/messages"}) != tooldefer.Pass {
		t.Fatal("probe did not pass")
	}
	// Session 2: decided at its first request, deferred, and the same bytes on its next turn.
	s2 := bytes.Replace(tdBody(), []byte("sess-1"), []byte("sess-2"), 1)
	tdPost(t, srv, s2)
	first := up.last()
	if got := tdTools(first); got != "Bash,Read,Workflow*,CronCreate*,context_guru_expand,context_guru_adjudicate,tool_search_tool_bm25" {
		t.Fatalf("tools = %s", got)
	}
	if !strings.Contains(gjson.GetBytes(first, "messages.1.content").String(), "Deferred tools:\nCronCreate: Does the CronCreate job.\nWorkflow: Does the Workflow job.") {
		t.Fatalf("names list missing at messages[1]: %s", gjson.GetBytes(first, "messages.1").Raw)
	}
	s2b := bytes.Replace(tdBody(`{"role":"assistant","content":"ok"}`, `{"role":"user","content":"next"}`), []byte("sess-1"), []byte("sess-2"), 1)
	tdPost(t, srv, s2b)
	if gjson.GetBytes(up.last(), "tools").Raw != gjson.GetBytes(first, "tools").Raw {
		t.Fatal("tools changed within the thread")
	}
	if gjson.GetBytes(up.last(), "messages.1").Raw != gjson.GetBytes(first, "messages.1").Raw {
		t.Fatal("names list changed within the thread")
	}
	if s := h.deferStats(); s == nil {
		t.Fatal("no /stats block")
	}
}

// xhigh never defers, with the same history and a passed probe.
func TestToolDeferOffInXhigh(t *testing.T) {
	up := &tdUpstream{}
	us := httptest.NewServer(up)
	defer us.Close()
	h := tdHandler(t, "xhigh", us.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	tdPost(t, srv, tdBody())
	tdPost(t, srv, bytes.Replace(tdBody(), []byte("sess-1"), []byte("sess-2"), 1))
	if got := tdTools(up.last()); strings.Contains(got, "*") || strings.Contains(got, "tool_search") {
		t.Fatalf("xhigh deferred: %s", got)
	}
}

// A 400 on a request we changed never reaches the client: the proxy sends the original once, then
// turns deferral off for that model.
func TestToolDeferRetriesOriginalOn400(t *testing.T) {
	up := &tdUpstream{}
	us := httptest.NewServer(up)
	defer us.Close()
	h := tdHandler(t, "ultrahigh", us.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	tdPost(t, srv, tdBody()) // runs the probe
	up.mu.Lock()
	up.reject400 = true
	up.mu.Unlock()
	before := up.count()
	resp, body := tdPost(t, srv, bytes.Replace(tdBody(), []byte("sess-1"), []byte("sess-2"), 1))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("client saw %d: %s", resp.StatusCode, body)
	}
	if up.count()-before != 2 {
		t.Fatalf("upstream rounds = %d, want 2 (the deferred request, then the original)", up.count()-before)
	}
	if got := tdTools(up.last()); strings.Contains(got, "*") || strings.Contains(got, "tool_search") {
		t.Fatalf("retry was not the original: %s", got)
	}
	if !h.deferGate().Broken("", "claude-sonnet-5-5") || h.deferGate().Retries.Load() != 1 {
		t.Fatal("deferral not turned off after the 400")
	}
	// The next new thread is not deferred.
	tdPost(t, srv, bytes.Replace(tdBody(), []byte("sess-1"), []byte("sess-3"), 1))
	if got := tdTools(up.last()); strings.Contains(got, "*") {
		t.Fatalf("deferred after the turn-off: %s", got)
	}
}

// A restart mid-thread: a new handler over the same history and a fresh store sends the same bytes.
// B1, end to end: a proxy with a memory-only session store (no dashboard) restarts mid-thread. The
// thread's record comes back from tool deferral's own file, so tools and messages[1] are
// byte-identical, with no history and no probe verdict carried over.
func TestToolDeferSurvivesRestart(t *testing.T) {
	up := &tdUpstream{}
	us := httptest.NewServer(up)
	defer us.Close()
	path := filepath.Join(t.TempDir(), tooldefer.FileName)
	h := tdHandlerAt(t, "ultrahigh", us.URL, path)
	srv := httptest.NewServer(h.Mux())
	tdPost(t, srv, tdBody())
	s2 := bytes.Replace(tdBody(), []byte("sess-1"), []byte("sess-2"), 1)
	tdPost(t, srv, s2)
	turn2 := bytes.Replace(tdBody(`{"role":"assistant","content":"ok"}`, `{"role":"user","content":"next"}`), []byte("sess-1"), []byte("sess-2"), 1)
	tdPost(t, srv, turn2)
	before := up.last()
	if !strings.Contains(tdTools(before), "*") {
		t.Fatalf("fixture: nothing deferred before the restart: %s", tdTools(before))
	}
	srv.Close()
	h.Close() // the process ends: the file is flushed

	cfg, _ := config.LoadBytes([]byte("preset: ultrahigh\n"))
	agg := metrics.NewAggregator()
	pipe, _ := cfg.Build(agg)
	h2 := New(pipe, store.NewMemory(store.Options{}), agg, Options{AnthropicUpstream: us.URL, ToolDeferPath: path, Prices: tdPrices{}})
	defer h2.Close()
	srv2 := httptest.NewServer(h2.Mux())
	defer srv2.Close()
	tdPost(t, srv2, turn2)
	after := up.last()
	if gjson.GetBytes(after, "tools").Raw != gjson.GetBytes(before, "tools").Raw ||
		gjson.GetBytes(after, "messages.1").Raw != gjson.GetBytes(before, "messages.1").Raw {
		t.Fatalf("restart moved the set:\nbefore %s\nafter  %s", tdTools(before), tdTools(after))
	}
}

// B1: with no durable place (a memory-only session store and no file path), ultrahigh defers nothing.
func TestToolDeferNoDurablePlaceDefersNothing(t *testing.T) {
	up := &tdUpstream{}
	us := httptest.NewServer(up)
	defer us.Close()
	cfg, _ := config.LoadBytes([]byte("preset: ultrahigh\n"))
	agg := metrics.NewAggregator()
	pipe, _ := cfg.Build(agg)
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{AnthropicUpstream: us.URL, Prices: tdPrices{}})
	defer h.Close()
	ds := h.toolDefer()
	for i, s := range []string{"p1", "p2", "p3"} {
		ds.hist.Observe("", s, "/w", tdBody(), int64(i+1))
	}
	ds.prober.Go = func(fn func()) { fn() }
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	tdPost(t, srv, tdBody())
	tdPost(t, srv, bytes.Replace(tdBody(), []byte("sess-1"), []byte("sess-2"), 1))
	if got := tdTools(up.last()); strings.Contains(got, "*") {
		t.Fatalf("deferred with no durable place: %s", got)
	}
}

// ENABLE_TOOL_SEARCH=false on /tool-search turns deferral off for that session, with keep-alive off;
// "auto" does not.
func TestToolDeferToolSearchRouteWithKeepAliveOff(t *testing.T) {
	up := &tdUpstream{}
	us := httptest.NewServer(up)
	defer us.Close()
	h := tdHandler(t, "ultrahigh", us.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	tdPost(t, srv, tdBody())
	t.Setenv("CONTEXT_GURU_KEEPALIVE", "off") // the flag's route must not depend on keep-alive
	post := func(sid, v string) {
		b := fmt.Sprintf(`{"session":%q,"enable_tool_search":%q}`, sid, v)
		resp, err := http.Post(srv.URL+"/tool-search", "application/json", strings.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("signal status %d", resp.StatusCode)
		}
	}
	// The keep-alive route really is off, so the next post proves the flag does not ride on it.
	if r, err := http.Post(srv.URL+"/signal", "application/json", strings.NewReader(`{"session":"x","event":"SessionEnd"}`)); err == nil {
		r.Body.Close()
		if r.StatusCode != http.StatusNotFound {
			t.Fatalf("/signal with keep-alive off = %d, want 404", r.StatusCode)
		}
	}
	post("sess-2", "0")
	tdPost(t, srv, bytes.Replace(tdBody(), []byte("sess-1"), []byte("sess-2"), 1))
	if got := tdTools(up.last()); strings.Contains(got, "*") {
		t.Fatalf("ENABLE_TOOL_SEARCH=false still deferred: %s", got)
	}
	post("sess-3", "auto")
	tdPost(t, srv, bytes.Replace(tdBody(), []byte("sess-1"), []byte("sess-3"), 1))
	if got := tdTools(up.last()); !strings.Contains(got, "*") {
		t.Fatalf("ENABLE_TOOL_SEARCH=auto turned deferral off: %s", got)
	}
}

// /stats and /signal race the first ultrahigh request, which builds the deferral state. Under -race
// this failed before the state was behind one lock.
func TestToolDeferStatsRacesFirstRequest(t *testing.T) {
	up := &tdUpstream{}
	us := httptest.NewServer(up)
	defer us.Close()
	cfg, err := config.LoadBytes([]byte("preset: ultrahigh\n"))
	if err != nil {
		t.Fatal(err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{AnthropicUpstream: us.URL,
		ToolDeferPath: filepath.Join(t.TempDir(), tooldefer.FileName), Prices: tdPrices{}})
	defer h.Close()
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 6; i++ {
		wg.Add(3)
		go func(i int) {
			defer wg.Done()
			<-start
			tdPost(t, srv, bytes.Replace(tdBody(), []byte("sess-1"), []byte(fmt.Sprintf("race-%d", i)), 1))
		}(i)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 20; j++ {
				if r, err := http.Get(srv.URL + "/stats"); err == nil {
					io.Copy(io.Discard, r.Body)
					r.Body.Close()
				}
			}
		}()
		go func(i int) {
			defer wg.Done()
			<-start
			b := fmt.Sprintf(`{"session":"sig-%d","enable_tool_search":"false"}`, i)
			if r, err := http.Post(srv.URL+"/tool-search", "application/json", strings.NewReader(b)); err == nil {
				r.Body.Close()
			}
		}(i)
	}
	close(start)
	wg.Wait()
}

// A /signal on a proxy that never served an ultrahigh request records the flag without building
// the deferral state (no history load, no database open).
func TestToolDeferSignalDoesNotBuildState(t *testing.T) {
	cfg, _ := config.LoadBytes([]byte("preset: xhigh\n"))
	agg := metrics.NewAggregator()
	pipe, _ := cfg.Build(agg)
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{AnthropicUpstream: "http://127.0.0.1:1"})
	defer h.Close()
	h.noteToolSearch(&Tenancy{}, "s", "false")
	if st, g := h.deferPeek(); st != nil || g == nil || !g.TakePending("", "s", store.NewMemory(store.Options{})) {
		t.Fatalf("state built=%v, gate=%v", st != nil, g != nil)
	}
}

// H1. The original succeeding after the deferred body got a 400 is the proof, whatever the text:
// a 400 that names no known cause still turns deferral off when the original then succeeds. When the
// original fails too, the refusal is not deferral's: deferral stays on.
func TestToolDeferRetryProofIsTheOriginalSucceeding(t *testing.T) {
	up := &tdUpstream{}
	us := httptest.NewServer(up)
	defer us.Close()
	h := tdHandler(t, "ultrahigh", us.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	tdPost(t, srv, tdBody())
	up.mu.Lock()
	up.rejectOther = true
	up.mu.Unlock()
	before := up.count()
	resp, body := tdPost(t, srv, bytes.Replace(tdBody(), []byte("sess-1"), []byte("sess-2"), 1))
	if resp.StatusCode != http.StatusOK || up.count()-before != 2 {
		t.Fatalf("status %d, rounds %d: %s", resp.StatusCode, up.count()-before, body)
	}
	if !h.deferGate().Broken("", "claude-sonnet-5-5") || h.deferGate().TurnedOff.Load() != 1 {
		t.Fatal("an unrecognised 400 whose original succeeded did not turn deferral off")
	}

	// A second account: the original fails too, so deferral stays on.
	up2 := &tdUpstream{}
	us2 := httptest.NewServer(up2)
	defer us2.Close()
	h2 := tdHandler(t, "ultrahigh", us2.URL)
	srv2 := httptest.NewServer(h2.Mux())
	defer srv2.Close()
	tdPost(t, srv2, tdBody())
	up2.mu.Lock()
	up2.rejectAll = true
	up2.mu.Unlock()
	resp, _ = tdPost(t, srv2, bytes.Replace(tdBody(), []byte("sess-1"), []byte("sess-2"), 1))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want the original's 400", resp.StatusCode)
	}
	if h2.deferGate().Broken("", "claude-sonnet-5-5") || h2.deferGate().Retries.Load() != 1 {
		t.Fatal("a 400 the original also got turned deferral off")
	}
}

// The preset picker's probe ask returns at once and starts no request itself; the account's next
// request starts the probe (in the background), even before the thread is decided.
func TestToolDeferProbeAsk(t *testing.T) {
	up := &tdUpstream{}
	us := httptest.NewServer(up)
	defer us.Close()
	h := tdHandler(t, "ultrahigh", us.URL)
	var started []func()
	h.toolDefer().prober.Go = func(fn func()) { started = append(started, fn) }
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	t0 := time.Now()
	resp, err := http.Post(srv.URL+"/tool-defer/probe", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || time.Since(t0) > time.Second || len(up.bodies) != 0 || len(started) != 0 {
		t.Fatalf("status %d, %d upstream calls, %d probes on the ask itself", resp.StatusCode, len(up.bodies), len(started))
	}
	// A thread that already has a stored record needs no probe of its own, so only the ask starts it.
	h.toolDefer().prober.Seed(tooldefer.ProbeKey{Upstream: us.URL, Model: "claude-sonnet-5-5", Route: "/v1/messages"}, tooldefer.Pass, time.Now())
	tdPost(t, srv, tdBody())
	if len(started) != 0 {
		t.Fatal("a fresh verdict was probed again")
	}
	h.toolDefer().prober.Forget(tooldefer.ProbeKey{Upstream: us.URL, Model: "claude-sonnet-5-5", Route: "/v1/messages"})
	started = nil
	resp, _ = http.Post(srv.URL+"/tool-defer/probe", "application/json", strings.NewReader(`{}`))
	resp.Body.Close()
	tdPost(t, srv, bytes.Replace(tdBody(), []byte("sess-1"), []byte("sess-9"), 1))
	if len(started) == 0 {
		t.Fatal("the next request did not start the asked probe")
	}
}

// The ask works in a preset that does not defer yet: the picker sets ultrahigh, and the request
// before the restart starts the probe.
func TestToolDeferProbeAskInXhigh(t *testing.T) {
	up := &tdUpstream{}
	us := httptest.NewServer(up)
	defer us.Close()
	h := tdHandler(t, "xhigh", us.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/tool-defer/probe", "application/json", strings.NewReader(`{}`))
	resp.Body.Close()
	tdPost(t, srv, tdBody())
	k := tooldefer.ProbeKey{Upstream: us.URL, Model: "claude-sonnet-5-5", Route: "/v1/messages"}
	if h.toolDefer().prober.Verdict(k) != tooldefer.Pass {
		t.Fatal("the asked probe did not run")
	}
	if got := tdTools(up.last()); strings.Contains(got, "*") {
		t.Fatalf("xhigh deferred: %s", got)
	}
}

// The operator's CONTEXT_GURU_CLIENT_SIGNALS=off turns both tool deferral routes off (404, nothing
// changed). Keep-alive off with the switch on leaves them working.
func TestToolDeferRoutesObeyTheClientSignalsSwitch(t *testing.T) {
	h := tdHandler(t, "ultrahigh", "http://127.0.0.1:1")
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	post := func(path, body string) int {
		r, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	t.Setenv("CONTEXT_GURU_CLIENT_SIGNALS", "off")
	if c := post("/tool-search", `{"session":"s1","enable_tool_search":"false"}`); c != http.StatusNotFound {
		t.Errorf("/tool-search with the switch off = %d", c)
	}
	if c := post("/tool-defer/probe", `{}`); c != http.StatusNotFound {
		t.Errorf("/tool-defer/probe with the switch off = %d", c)
	}
	if h.deferGate().TakePending("", "s1", store.NewMemory(store.Options{})) || h.takeProbeAsk(&Tenancy{}) {
		t.Fatal("a route changed state with the switch off")
	}
	t.Setenv("CONTEXT_GURU_CLIENT_SIGNALS", "on")
	t.Setenv("CONTEXT_GURU_KEEPALIVE", "off")
	if c := post("/tool-search", `{"session":"s1","enable_tool_search":"false"}`); c != http.StatusNoContent ||
		!h.deferGate().TakePending("", "s1", store.NewMemory(store.Options{})) {
		t.Errorf("/tool-search with keep-alive off = %d", c)
	}
	if c := post("/tool-defer/probe", `{}`); c != http.StatusAccepted || !h.takeProbeAsk(&Tenancy{}) {
		t.Errorf("/tool-defer/probe with keep-alive off = %d", c)
	}
}

// B3 (#861). The purge hook forgets the tenant's tool deferral state: the history in memory, the
// pending opt-outs, and the records and rows in tool deferral's own file.
func TestToolDeferPurgeForgetsTheTenant(t *testing.T) {
	up := &tdUpstream{}
	us := httptest.NewServer(up)
	defer us.Close()
	h := tdHandler(t, "ultrahigh", us.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	tdPost(t, srv, tdBody())
	tdPost(t, srv, bytes.Replace(tdBody(), []byte("sess-1"), []byte("sess-2"), 1))
	ds := h.toolDefer()
	// The single-tenant id is "": give the purge a tenant id by writing a tenant's rows directly.
	ds.hist.Observe("acct", "acct:s9", "/w", tdBody(), 5)
	f := h.defer_.file
	tooldefer.Save(f.Store, "acct:s9", "", tooldefer.Record{Deferred: []string{"Workflow"}})
	tooldefer.SetSearchOff(f.Store, "acct:s9", true)
	h.deferGate().NoteToolSearch("acct", "acct:s10", "false", nil, false)
	f.Store.Put(store.ToolDeferPrefix+"other:s1", []byte(`{"d":[]}`))
	n, err := h.purgeToolDefer(context.Background(), "acct")
	if err != nil {
		t.Fatal(err)
	}
	_ = n
	if ds.hist.Has("acct", "acct:s9") {
		t.Error("the history kept the tenant's session")
	}
	if _, ok := tooldefer.Load(f.Store, "acct:s9", ""); ok {
		t.Error("the file kept the tenant's record")
	}
	if tooldefer.SearchOff(f.Store, "acct:s9") || h.deferGate().TakePending("acct", "acct:s10", f.Store) {
		t.Error("an opt-out of the tenant survived the purge")
	}
	if _, ok := f.Store.Get(store.ToolDeferPrefix + "other:s1"); !ok {
		t.Error("the purge took another tenant's record")
	}
}

// B4b (#861). A flag for a session the proxy has seen is stored at once; one for a session it has
// not seen waits for that session's first request, and is applied there.
func TestToolDeferOptOutForUnseenSessionWaits(t *testing.T) {
	up := &tdUpstream{}
	us := httptest.NewServer(up)
	defer us.Close()
	h := tdHandler(t, "ultrahigh", us.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	tdPost(t, srv, tdBody()) // the probe
	post := func(sid string) {
		r, err := http.Post(srv.URL+"/tool-search", "application/json",
			strings.NewReader(fmt.Sprintf(`{"session":%q,"enable_tool_search":"false"}`, sid)))
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
	}
	// Before its first request (the `claude -p` race): it waits, then applies.
	post("sess-new")
	if tooldefer.SearchOff(h.defer_.file.Store, "sess-new") {
		t.Fatal("a flag for an unseen session was stored at once")
	}
	tdPost(t, srv, bytes.Replace(tdBody(), []byte("sess-1"), []byte("sess-new"), 1))
	if got := tdTools(up.last()); strings.Contains(got, "*") {
		t.Fatalf("the pending flag was not applied at the first request: %s", got)
	}
	if !tooldefer.SearchOff(h.defer_.file.Store, "sess-new") {
		t.Fatal("the pending flag did not reach the record store")
	}
	// A seen session: stored at once.
	post("sess-1")
	if !tooldefer.SearchOff(h.defer_.file.Store, "sess-1") {
		t.Fatal("a flag for a seen session was not stored")
	}
}

// H4 (#861). A model the price table does not know is never probed, so it is never deferred.
func TestToolDeferUnknownModelIsNotProbed(t *testing.T) {
	up := &tdUpstream{}
	us := httptest.NewServer(up)
	defer us.Close()
	h := tdHandler(t, "ultrahigh", us.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	b := bytes.Replace(tdBody(), []byte("claude-sonnet-5-5"), []byte("claude-made-up-9"), 1)
	h.opts.Prices = tdPricesOnly{"claude-sonnet-5-5"}
	tdPost(t, srv, b)
	for _, rb := range up.bodies {
		if strings.Contains(string(rb), "cg_probe_") {
			t.Fatal("an unpriced model was probed")
		}
	}
}

type tdPricesOnly struct{ model string }

func (p tdPricesOnly) Price(_ context.Context, model string) (modelinfo.Price, bool) {
	if model != p.model {
		return modelinfo.Price{}, false
	}
	return modelinfo.Price{Input: 3e-6, Output: 15e-6}, true
}

// Round 4 (#861). A purge after a restart on a preset that does not defer (so this process never
// opened tool deferral's file), still deletes the tenant's rows in the file and reports them. A
// purge with no file creates none.
func TestToolDeferPurgeReachesAnUnopenedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), tooldefer.FileName)
	f, err := tooldefer.OpenFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	tooldefer.Save(f.Store, "acct:s1", "", tooldefer.Record{Deferred: []string{"Workflow"}})
	tooldefer.SetSearchOff(f.Store, "acct:s1", true)
	tooldefer.Save(f.Store, "other:s1", "", tooldefer.Record{Deferred: []string{"Workflow"}})
	m := tooldefer.NewMem(f.DB)
	stop := m.Open(time.Hour, nil)
	for i := 0; i < 200 && !m.Ready(); i++ {
		time.Sleep(5 * time.Millisecond)
	}
	m.Observe("acct", "acct:s1", "/w", tdBody(), time.Now().UnixMilli())
	stop() // flushes the history
	f.Close()

	// The restarted proxy, now on a preset that does not defer: the file is not open (ultrahigh opens
	// it at start since #871).
	cfg, _ := config.LoadBytes([]byte("preset: xhigh\n"))
	agg := metrics.NewAggregator()
	pipe, _ := cfg.Build(agg)
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{AnthropicUpstream: "http://127.0.0.1:1", ToolDeferPath: path})
	defer h.Close()
	if h.defer_.file != nil {
		t.Fatal("fixture: the file is open before any request")
	}
	n, err := h.purgeToolDefer(context.Background(), "acct")
	if err != nil {
		t.Fatal(err)
	}
	if n < 3 { // 1 record, 1 flag, at least 1 history row
		t.Fatalf("the purge reported %d rows", n)
	}
	if h.defer_.file != nil {
		t.Fatal("the purge left the file open as the deferral state")
	}
	db, err := tooldefer.OpenForPurge(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var hist, mine, other int
	db.QueryRow(`SELECT COUNT(*) FROM tool_defer_history WHERE tenant_id = 'acct'`).Scan(&hist)
	db.QueryRow(`SELECT COUNT(*) FROM session_state WHERE key >= ? AND key < ?`, []byte("cg:tdf:acct:"), []byte("cg:tdf:acct;")).Scan(&mine)
	db.QueryRow(`SELECT COUNT(*) FROM session_state WHERE key >= ? AND key < ?`, []byte("cg:tdf:other:"), []byte("cg:tdf:other;")).Scan(&other)
	if hist != 0 || mine != 0 || other != 1 {
		t.Fatalf("after the purge: history %d, records %d (want 0), other tenant %d (want 1)", hist, mine, other)
	}

	// No file at the path: the purge creates none.
	none := filepath.Join(t.TempDir(), tooldefer.FileName)
	h2 := New(pipe, store.NewMemory(store.Options{}), agg, Options{AnthropicUpstream: "http://127.0.0.1:1", ToolDeferPath: none})
	defer h2.Close()
	if n, err := h2.purgeToolDefer(context.Background(), "acct"); err != nil || n != 0 {
		t.Fatalf("purge with no file: %d, %v", n, err)
	}
	if _, err := os.Stat(none); !os.IsNotExist(err) {
		t.Fatalf("the purge created the file: %v", err)
	}
}
