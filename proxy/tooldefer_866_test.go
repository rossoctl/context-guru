package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
	"github.com/rossoctl/context-guru/tenant"
	"github.com/rossoctl/context-guru/tooldefer"
)

// #866 N5. A streamed response of 64–128 KiB: the sniffer's head and tail overlap. A search in the
// overlap was counted twice; disjoint holds every byte once.
func TestSnifferDisjointCountsOnce(t *testing.T) {
	search := "data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"server_tool_use\",\"name\":\"tool_search_tool_bm25\"}}\n\n"
	pad := func(n int) string { return "data: {\"type\":\"ping\",\"x\":\"" + strings.Repeat("p", n) + "\"}\n\n" }
	// About 100 KiB: the search sits at 60 KiB, inside the head and the tail both.
	body := pad(60<<10) + search + pad(40<<10)
	sn := newSniffer(true)
	for i := 0; i < len(body); i += 4096 {
		sn.write([]byte(body[i:min(i+4096, len(body))]))
	}
	if n, _ := tooldefer.SearchesInSSE(sn.bytes()); n != 2 {
		t.Fatalf("the test does not reach the overlap: bytes() holds %d searches, want 2", n)
	}
	if got := sn.disjoint(); string(got) != body {
		t.Fatalf("disjoint is not the body: %d bytes, want %d", len(got), len(body))
	}
	if n, _ := tooldefer.SearchesInSSE(sn.disjoint()); n != 1 {
		t.Fatalf("disjoint counts %d searches, want 1", n)
	}
	// Past 128 KiB there is a gap, and disjoint is bytes(): each search once.
	long := search + pad(200<<10) + search
	sn = newSniffer(true)
	sn.write([]byte(long))
	if n, _ := tooldefer.SearchesInSSE(sn.disjoint()); n != 2 {
		t.Fatalf("with a gap: %d searches, want 2", n)
	}
	// Under 64 KiB, the whole body.
	sn = newSniffer(true)
	sn.write([]byte(search))
	if string(sn.disjoint()) != search {
		t.Fatal("a short body is not kept whole")
	}
}

// sse100k is sseUpstream with 100 KiB of text after the search, so the sniffer's windows overlap
// on the stream-through path.
type sse100k struct{ sseUpstream }

func (u sse100k) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(b))
	if bytes.Contains(b, []byte("cg_probe_")) {
		u.sseUpstream.ServeHTTP(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	ev := func(typ, data string) { fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, data) }
	ev("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-sonnet-5-5","usage":{"input_tokens":5,"cache_read_input_tokens":3000,"output_tokens":1}}}`)
	ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+strings.Repeat("a", 50<<10)+`"}}`)
	ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
	ev("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"server_tool_use","id":"srvtoolu_1","name":"tool_search_tool_bm25","input":{}}}`)
	ev("content_block_stop", `{"type":"content_block_stop","index":1}`)
	// A search sent with another tool call: no result, dropped (N8).
	ev("content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"t1","name":"Read","input":{}}}`)
	ev("content_block_stop", `{"type":"content_block_stop","index":2}`)
	ev("content_block_start", `{"type":"content_block_start","index":3,"content_block":{"type":"text","text":""}}`)
	ev("content_block_delta", `{"type":"content_block_delta","index":3,"delta":{"type":"text_delta","text":"`+strings.Repeat("b", 50<<10)+`"}}`)
	ev("content_block_stop", `{"type":"content_block_stop","index":3}`)
	ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":20}}`)
	ev("message_stop", `{"type":"message_stop"}`)
}

// streamThrough is a handler that defers but carries no proxy tool, so a stream goes through the
// sniffer path. Three past sessions declared Workflow and Monitor and used neither.
func streamThrough(t *testing.T, upstream string) *Handler {
	t.Helper()
	cfg, _ := config.LoadBytes([]byte("pipeline: [toolfilter]\ncomponents:\n  toolfilter:\n    defer: auto\n"))
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{AnthropicUpstream: upstream,
		ToolDeferPath: filepath.Join(t.TempDir(), tooldefer.FileName), Prices: tdPrices{}, InjectExpand: "never"})
	t.Cleanup(h.Close)
	ds := h.toolDefer()
	for i, s := range []string{"p1", "p2", "p3"} {
		ds.hist.Observe("", s, "/w", tdBody(), int64(i+1))
	}
	ds.prober.Seed(tooldefer.ProbeKey{Upstream: upstream, Model: "claude-sonnet-5-5", Route: "/v1/messages"}, tooldefer.Pass, time.Now())
	return h
}

func streamed(b []byte) []byte {
	return bytes.Replace(b, []byte(`"max_tokens":64,`), []byte(`"max_tokens":64,"stream":true,`), 1)
}

// N5 end to end, and N8's dropped search: a 100 KiB stream-through response counts its one search
// once, as dropped.
func TestToolDeferLedgerCountsA100KiBStreamOnce(t *testing.T) {
	us := httptest.NewServer(sse100k{})
	defer us.Close()
	h := streamThrough(t, us.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	resp, out := tdPost(t, srv, streamed(tdBody()))
	if resp.StatusCode != http.StatusOK || len(out) < 100<<10 {
		t.Fatalf("status %d, %d bytes", resp.StatusCode, len(out))
	}
	st := h.toolDefer().ledger.Snapshot()
	if st.Requests != 1 || st.Searches != 1 || st.DroppedSearches != 1 {
		t.Fatalf("ledger = %+v, want 1 request with 1 search, dropped", st)
	}
}

// N8. A search that loads a tool this request deferred is a wrong guess, booked per account, and
// /stats names the counters.
func TestToolDeferStatsCountWrongGuessesPerAccount(t *testing.T) {
	us := httptest.NewServer(sseUpstream{}) // its search loads Monitor
	defer us.Close()
	h := streamThrough(t, us.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	if resp, _ := tdPost(t, srv, streamed(tdBody())); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	st := h.toolDefer().ledger.Snapshot()
	if st.WrongGuesses != 1 || st.DroppedSearches != 0 {
		t.Fatalf("ledger = %+v, want 1 wrong guess (Monitor), 0 dropped", st)
	}
	m := h.deferStats().(map[string]any)
	acc := m["accounts"].(map[string]tooldefer.Stats)
	if m["wrong_guesses"] != int64(1) || m["dropped_searches"] != int64(0) || acc[""].WrongGuesses != 1 || acc[""].Requests != 1 {
		t.Fatalf("/stats tool_defer = %+v", m)
	}
}

// N12a. The probe carries the client's OAuth beta, and no other beta.
func TestToolDeferProbeSendsTheClientsOAuthBeta(t *testing.T) {
	var mu sync.Mutex
	var betas []string
	up := &tdUpstream{}
	us := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if bytes.Contains(b, []byte("cg_probe_")) {
			mu.Lock()
			betas = append(betas, r.Header.Get("Anthropic-Beta"))
			mu.Unlock()
		}
		r.Body = io.NopCloser(bytes.NewReader(b))
		up.ServeHTTP(w, r)
	}))
	defer us.Close()
	h := tdHandler(t, "ultrahigh", us.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages", bytes.NewReader(tdBody()))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-test")
	req.Header.Set("Anthropic-Beta", "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(betas) != 1 || betas[0] != "oauth-2025-04-20" {
		t.Fatalf("probe betas = %q, want [oauth-2025-04-20]", betas)
	}
}

// GLM note on #872. A 400 that names a missing reference, on a body whose `tools` the stub cannot
// extend: the response stays readable, with its bytes.
func TestRepairOnMissingRefKeepsTheBodyWhenTheStubFails(t *testing.T) {
	msg := `{"type":"error","error":{"type":"invalid_request_error","message":"Tool reference 'mcp__gone__x' not found in available tools"}}`
	resp := &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(msg))}
	if _, ok := repairOnMissingRef(resp, []byte(`{"tools":{"not":"an array"}}`)); ok {
		t.Fatal("repaired a body it cannot stub")
	}
	if b, _ := io.ReadAll(resp.Body); string(b) != msg {
		t.Fatalf("the provider's message is lost: %q", b)
	}
}

// GLM note on #872. A tenant that turns deferral on after the start gets the eager build: the
// state exists before its first request.
func TestToolDeferTenantAfterStartBuildsTheState(t *testing.T) {
	path := filepath.Join(t.TempDir(), tooldefer.FileName)
	f := newHostedFixtureOpts(t, "anthropic", "anthropic", "", func(o *Options) { o.ToolDeferPath = path })
	if ds, _ := f.h.deferPeek(); ds != nil {
		t.Fatal("built at start with no tenant on ultrahigh")
	}
	tn, _ := f.register(t, "a@ibm.com")
	doc := "preset: ultrahigh\n"
	if err := f.reg.Update(tn, tn.ID, tenant.Patch{ConfigYAML: &doc}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500; i++ {
		if ds, _ := f.h.deferPeek(); ds != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("a tenant that picked ultrahigh after the start did not get the eager build")
}

// GLM note on #872. The start waits one bound in all (startWait), for the load and every seed
// together: two seeds that never end return at the bound, not at twice it, and never hang.
func TestWaitAllIsOneBound(t *testing.T) {
	never1, never2 := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	if waitAll(ctx, never1) {
		t.Fatal("reported done")
	}
	if waitAll(ctx, never1, never2) || time.Since(t0) > time.Second {
		t.Fatalf("waited %v for one 50 ms bound", time.Since(t0))
	}
	closed := make(chan struct{})
	close(closed)
	if !waitAll(context.Background(), closed, closed) {
		t.Fatal("closed channels not done")
	}
}
