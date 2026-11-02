package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/config"
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
		`"tools":[` + tdDecl("Bash") + `,` + tdDecl("Read") + `,` + tdDecl("Workflow") + `,` + tdDecl("Monitor") + `],` +
		`"messages":[` + msgs + `]}`)
}

// tdUpstream answers like the gateway. reject400 makes it refuse any body with defer_loading, the way
// a model without support does. It answers a probe as the API would.
type tdUpstream struct {
	mu          sync.Mutex
	bodies      [][]byte
	reject400   bool
	rejectOther bool // a 400 that does not name a deferral cause
}

func (u *tdUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.bodies = append(u.bodies, b)
	reject := u.reject400
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
	if u.rejectOther && bytes.Contains(b, []byte(`"defer_loading"`)) {
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

func tdHandler(t *testing.T, preset, upstream string) *Handler {
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
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{AnthropicUpstream: upstream})
	t.Cleanup(h.Close)
	// Three past sessions that declared Workflow and Monitor and used neither.
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
	if got := tdTools(first); got != "Bash,Read,Workflow*,Monitor*,context_guru_expand,context_guru_adjudicate,tool_search_tool_bm25" {
		t.Fatalf("tools = %s", got)
	}
	if !strings.Contains(gjson.GetBytes(first, "messages.1.content").String(), "Deferred tools:\nMonitor\nWorkflow") {
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
func TestToolDeferSurvivesRestart(t *testing.T) {
	up := &tdUpstream{}
	us := httptest.NewServer(up)
	defer us.Close()
	h := tdHandler(t, "ultrahigh", us.URL)
	srv := httptest.NewServer(h.Mux())
	tdPost(t, srv, tdBody())
	s2 := bytes.Replace(tdBody(), []byte("sess-1"), []byte("sess-2"), 1)
	tdPost(t, srv, s2)
	turn2 := bytes.Replace(tdBody(`{"role":"assistant","content":"ok"}`, `{"role":"user","content":"next"}`), []byte("sess-1"), []byte("sess-2"), 1)
	tdPost(t, srv, turn2)
	before := up.last()
	srv.Close()

	h2 := tdHandler(t, "ultrahigh", us.URL)
	// A real restart keeps the history and the probe verdict (their tables). Copy what h had.
	h2.toolDefer().hist.Observe("", "sess-1", "/w", tdBody(), 10)
	k := tooldefer.ProbeKey{Upstream: us.URL, Model: "claude-sonnet-5-5", Route: "/v1/messages"}
	h2.toolDefer().prober.Seed(k, h.toolDefer().prober.Verdict(k), time.Now())
	srv2 := httptest.NewServer(h2.Mux())
	defer srv2.Close()
	tdPost(t, srv2, turn2)
	after := up.last()
	if gjson.GetBytes(after, "tools").Raw != gjson.GetBytes(before, "tools").Raw ||
		gjson.GetBytes(after, "messages.1").Raw != gjson.GetBytes(before, "messages.1").Raw {
		t.Fatalf("restart moved the set:\nbefore %s\nafter  %s", tdTools(before), tdTools(after))
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
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{AnthropicUpstream: us.URL})
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
	h.noteToolSearch("s", "false")
	if st, g := h.deferPeek(); st != nil || g == nil || !g.SearchOff("s") {
		t.Fatalf("state built=%v, gate=%v", st != nil, g != nil)
	}
}

// A 400 that does not name a deferral cause is still answered with the original, but deferral stays
// on for the account and model.
func TestToolDeferOther400DoesNotTurnDeferralOff(t *testing.T) {
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
	if h.deferGate().Broken("", "claude-sonnet-5-5") {
		t.Fatal("a 400 with another cause turned deferral off")
	}
	if h.deferGate().Retries.Load() != 1 {
		t.Fatal("the retry was not counted")
	}
	up.mu.Lock()
	up.rejectOther = false
	up.mu.Unlock()
	tdPost(t, srv, bytes.Replace(tdBody(), []byte("sess-1"), []byte("sess-3"), 1))
	if got := tdTools(up.last()); !strings.Contains(got, "*") {
		t.Fatalf("deferral off after an unrelated 400: %s", got)
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
	if h.deferGate().SearchOff("s1") || h.takeProbeAsk(&Tenancy{}) {
		t.Fatal("a route changed state with the switch off")
	}
	t.Setenv("CONTEXT_GURU_CLIENT_SIGNALS", "on")
	t.Setenv("CONTEXT_GURU_KEEPALIVE", "off")
	if c := post("/tool-search", `{"session":"s1","enable_tool_search":"false"}`); c != http.StatusNoContent || !h.deferGate().SearchOff("s1") {
		t.Errorf("/tool-search with keep-alive off = %d", c)
	}
	if c := post("/tool-defer/probe", `{}`); c != http.StatusAccepted || !h.takeProbeAsk(&Tenancy{}) {
		t.Errorf("/tool-defer/probe with keep-alive off = %d", c)
	}
}
