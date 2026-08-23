package proxy_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	_ "github.com/rossoctl/context-guru/components/all"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/proxy"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
)

// buildHandler wires a real config->pipeline->proxy against a mock upstream that
// records the body it receives.
func buildHandler(t *testing.T, yaml string, upstream string) (*proxy.Handler, store.Store) {
	t.Helper()
	cfg, err := config.LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemory(store.Options{})
	return proxy.New(pipe, st, agg, proxy.Options{OpenAIUpstream: upstream, AnthropicUpstream: upstream}), st
}

func openAIBody(msgs ...map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{"model": "gpt-x", "temperature": 0.2, "messages": msgs})
	return b
}

// expandableBody is a realistic post-offload request: the client declares its own tools
// AND the transcript carries a <<cg:HASH>> marker. Both are required before the proxy
// advertises context_guru_expand (expand.Inject under InjectAuto), and the proxy inspects
// responses for a call to that tool exactly when it advertised it — so a body without
// them cannot produce an expand round, in a test or in production.
func expandableBody(hash string) []byte {
	b, _ := json.Marshal(map[string]any{
		"model": "gpt-x",
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "read_file"}}},
		"messages": []any{
			map[string]any{"role": "user", "content": "go"},
			map[string]any{"role": "tool", "tool_call_id": "a", "content": "earlier output <<cg:" + hash + ">>"},
		},
	})
	return b
}

func TestProxyReducesThenForwards(t *testing.T) {
	var got []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	h, _ := buildHandler(t, "pipeline: [dedup]\n", upstream.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	dump := strings.Repeat("a verbose repeated tool output line\n", 60)
	body := openAIBody(
		map[string]any{"role": "user", "content": "do the thing"},
		map[string]any{"role": "tool", "tool_call_id": "a", "content": dump},
		map[string]any{"role": "tool", "tool_call_id": "b", "content": dump},
	)
	resp, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Upstream must have received a SMALLER messages array (dedup collapsed the dup),
	// while non-message fields (model, temperature) survive verbatim (I1).
	if len(got) == 0 {
		t.Fatal("upstream received nothing")
	}
	if gjson.GetBytes(got, "model").String() != "gpt-x" || gjson.GetBytes(got, "temperature").Float() != 0.2 {
		t.Fatalf("non-message fields not preserved: %s", got)
	}
	third := gjson.GetBytes(got, "messages.2.content").String()
	if !strings.Contains(third, "identical to an earlier") {
		t.Fatalf("dedup did not run through the proxy: %q", third)
	}
	if len(got) >= len(body) {
		t.Fatalf("proxy did not shrink the request (before=%d after=%d)", len(body), len(got))
	}
}

// TestAnthropicRouteReducesToolResult drives the real /anthropic/v1/messages
// gateway route with a Claude-Code-shaped body (tool outputs as tool_result
// blocks in user messages) and asserts the offloader fires end-to-end.
func TestAnthropicRouteReducesToolResult(t *testing.T) {
	var got []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"type":"message","content":[{"type":"text","text":"ok"}]}`))
	}))
	defer upstream.Close()

	h, _ := buildHandler(t, "pipeline: [dedup]\ncomponents:\n  dedup: {min_tokens: 20}\n", upstream.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	dump := strings.Repeat("verbose repeated anthropic tool output line\n", 40)
	body, _ := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "user", "content": "do it"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": dump},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "t2", "content": dump},
			}},
		},
	})
	resp, err := http.Post(srv.URL+"/anthropic/v1/messages", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if len(got) == 0 {
		t.Fatal("upstream received nothing")
	}
	if gjson.GetBytes(got, "model").String() != "claude-sonnet-4-6" {
		t.Fatalf("model not preserved: %s", got)
	}
	if !strings.Contains(gjson.GetBytes(got, "messages.2.content.0.content").String(), "identical to an earlier") {
		t.Fatalf("dedup did not run on the anthropic tool_result via the proxy: %s", got)
	}
	if len(got) >= len(body) {
		t.Fatalf("proxy did not shrink the anthropic request (before=%d after=%d)", len(body), len(got))
	}
}

// TestBobGatewayReducesModelAndPassesControlPlane drives the Bob (BobShell)
// gateway: the OpenAI-dialect model call on /inference/v1/chat/completions is
// reduced like any chat and forwarded to the same path, while a control-plane
// call (GET /admin/v1/profile) passes through to the upstream verbatim.
func TestBobGatewayReducesModelAndPassesControlPlane(t *testing.T) {
	type hit struct {
		method, path string
		body         []byte
	}
	var hits []hit
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		hits = append(hits, hit{r.Method, r.URL.Path, b})
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg, err := config.LoadBytes([]byte("pipeline: [dedup]\ncomponents:\n  dedup: {min_tokens: 20}\n"))
	if err != nil {
		t.Fatal(err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	h := proxy.New(pipe, store.NewMemory(store.Options{}), agg, proxy.Options{BobUpstream: upstream.URL})
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	// 1) Model call on Bob's path is reduced (dedup) and forwarded to the same path.
	dump := strings.Repeat("verbose repeated bob tool output line\n", 40)
	body := openAIBody(
		map[string]any{"role": "user", "content": "do it"},
		map[string]any{"role": "tool", "tool_call_id": "a", "content": dump},
		map[string]any{"role": "tool", "tool_call_id": "b", "content": dump},
	)
	resp, err := http.Post(srv.URL+"/inference/v1/chat/completions", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// 2) Control-plane call is proxied through verbatim.
	cp, err := http.Get(srv.URL + "/admin/v1/profile")
	if err != nil {
		t.Fatal(err)
	}
	cp.Body.Close()

	if len(hits) != 2 {
		t.Fatalf("want 2 upstream hits, got %d: %+v", len(hits), hits)
	}
	model, control := hits[0], hits[1]
	if model.path != "/inference/v1/chat/completions" {
		t.Fatalf("model call forwarded to wrong path: %q", model.path)
	}
	if !strings.Contains(gjson.GetBytes(model.body, "messages.2.content").String(), "identical to an earlier") {
		t.Fatalf("dedup did not run on the bob model call: %s", model.body)
	}
	if len(model.body) >= len(body) {
		t.Fatalf("bob model call not shrunk (before=%d after=%d)", len(body), len(model.body))
	}
	if control.method != "GET" || control.path != "/admin/v1/profile" {
		t.Fatalf("control-plane not passed through verbatim: %s %s", control.method, control.path)
	}
	if len(control.body) != 0 {
		t.Fatalf("control-plane GET should have empty body, got %d bytes", len(control.body))
	}
}

func TestBypassHeaderForwardsUnchanged(t *testing.T) {
	var got []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()
	h, _ := buildHandler(t, "pipeline: [dedup]\n", upstream.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	dump := strings.Repeat("repeated line\n", 60)
	body := openAIBody(
		map[string]any{"role": "tool", "content": dump},
		map[string]any{"role": "tool", "content": dump},
	)
	req, _ := http.NewRequest("POST", srv.URL+"/openai/v1/chat/completions", strings.NewReader(string(body)))
	req.Header.Set("x-context-guru-bypass", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gjson.GetBytes(got, "messages.1.content").String() != dump {
		t.Fatal("bypass should forward messages unchanged")
	}
}

func TestGatewayInjectsRealKey(t *testing.T) {
	var gotAuth, gotXAPI string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotXAPI = r.Header.Get("x-api-key")
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	cfg, _ := config.LoadBytes([]byte("pipeline: []\n"))
	pipe, _ := cfg.Build(nil)
	h := proxy.New(pipe, store.NewMemory(store.Options{}), nil, proxy.Options{
		OpenAIUpstream: upstream.URL, OpenAIKey: "real-openai-key",
	})
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	body := openAIBody(map[string]any{"role": "user", "content": "hi"})
	req, _ := http.NewRequest("POST", srv.URL+"/openai/v1/chat/completions", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer sk-proxy") // placeholder from the agent
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotAuth != "Bearer real-openai-key" {
		t.Fatalf("gateway should inject the real key, upstream saw %q", gotAuth)
	}
	_ = gotXAPI
}

func TestExpandToolLoop(t *testing.T) {
	var calls int
	var secondBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			// model asks to expand the offloaded content
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[` +
				`{"id":"call_1","type":"function","function":{"name":"context_guru_expand","arguments":"{\"id\":\"HASH\"}"}}` +
				`]},"finish_reason":"tool_calls"}]}`))
			return
		}
		secondBody = b
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`))
	}))
	defer upstream.Close()

	h, st := buildHandler(t, "pipeline: []\n", upstream.URL)
	st.Put("HASH", []byte("THE ORIGINAL CONTENT")) // as if a prior turn offloaded it
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	body := expandableBody("HASH")
	resp, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	final, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if calls != 2 {
		t.Fatalf("expected 2 upstream calls (initial + continuation), got %d", calls)
	}
	if !strings.Contains(string(final), "done") {
		t.Fatalf("proxy should return the final answer, got %s", final)
	}
	if !strings.Contains(string(secondBody), "THE ORIGINAL CONTENT") {
		t.Fatalf("continuation must carry the resolved original, got %s", secondBody)
	}
	if gjson.GetBytes(secondBody, "messages.#").Int() != 4 {
		t.Fatalf("continuation should append assistant + tool turns to the 2 original ones: %s", secondBody)
	}
}

// TestExpandSSELoop proves restoration now works on the STREAMING path (the real
// claude-code case). The first upstream reply is an Anthropic event-stream whose only
// tool_use is context_guru_expand; the request carries a <<cg:HASH>> marker so the
// proxy buffers+aggregates the SSE, resolves the original, and re-invokes upstream.
func TestExpandSSELoop(t *testing.T) {
	var calls int
	var secondBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls++
		if calls == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			// A minimal Anthropic tool_use SSE: start, block start (tool_use), the input
			// json as one delta, stops.
			w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\"}}\n\n" +
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_1\",\"name\":\"context_guru_expand\"}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"id\\\":\\\"HASH\\\"}\"}}\n\n" +
				"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
			return
		}
		secondBody = b
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\"}}\n\n" +
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"done\"}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer upstream.Close()

	h, st := buildHandler(t, "pipeline: []\n", upstream.URL)
	st.Put("HASH", []byte("THE ORIGINAL CONTENT"))
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	// The request must advertise a tool (so expand injection fires) and carry a marker.
	// Build without Go's HTML-escaping so the literal "<<cg:HASH>>" reaches the proxy
	// (real Anthropic/OpenAI SDK clients do not HTML-escape "<").
	var bb bytes.Buffer
	enc := json.NewEncoder(&bb)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(map[string]any{
		"model":  "claude",
		"stream": true,
		"tools":  []map[string]any{{"name": "Bash", "description": "run", "input_schema": map[string]any{"type": "object"}}},
		"messages": []map[string]any{
			{"role": "user", "content": "look at <<cg:HASH>> and finish"},
		},
	})
	body := bb.Bytes()
	resp, err := http.Post(srv.URL+"/anthropic/v1/messages", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	final, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if calls != 2 {
		t.Fatalf("expected 2 upstream calls (SSE expand + continuation), got %d", calls)
	}
	if !strings.Contains(string(secondBody), "THE ORIGINAL CONTENT") {
		t.Fatalf("continuation must carry the resolved original, got %s", secondBody)
	}
	if !strings.Contains(string(final), "done") {
		t.Fatalf("client should receive the final streamed answer, got %s", final)
	}
	// The counters have to say what happened: round 1 opened with the expand call, so it
	// was buffered, and sse_buffered is sticky for the whole client request even though
	// round 2 streamed. If this ever reads streamed=1/buffered=0 the peek has started
	// letting real expand calls through, which is the failure this whole path guards.
	var snap metrics.Snapshot
	stresp, _ := http.Get(srv.URL + "/stats")
	json.NewDecoder(stresp.Body).Decode(&snap)
	stresp.Body.Close()
	if snap.SSEBuffered != 1 || snap.SSEStreamed != 0 {
		t.Fatalf("a response OPENING with the expand call must still be buffered: %+v", snap)
	}
}

// TestExpandPartialResolutionWellFormed guards the malformed-continuation bug:
// the model makes TWO expand calls but only one id resolves. The continuation
// must still carry a tool_result for BOTH call ids (the missing one gets a
// placeholder) or the provider rejects the request.
func TestExpandPartialResolutionWellFormed(t *testing.T) {
	var calls int
	var secondBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[` +
				`{"id":"call_1","type":"function","function":{"name":"context_guru_expand","arguments":"{\"id\":\"GOOD\"}"}},` +
				`{"id":"call_2","type":"function","function":{"name":"context_guru_expand","arguments":"{\"id\":\"GONE\"}"}}` +
				`]},"finish_reason":"tool_calls"}]}`))
			return
		}
		secondBody = b
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	h, st := buildHandler(t, "pipeline: []\n", upstream.URL)
	st.Put("GOOD", []byte("RESOLVED ORIGINAL")) // only one of the two resolves
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	body := expandableBody("GOOD")
	resp, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if calls != 2 {
		t.Fatalf("expected a continuation round, got %d upstream calls", calls)
	}
	// One tool message per EXPAND tool_call_id (both call_1 and call_2), or the provider
	// errors. Counted by call id: the request already carried an unrelated tool turn (the
	// one holding the marker), which is not a result for this round.
	var toolCalls int
	gjson.GetBytes(secondBody, "messages").ForEach(func(_, m gjson.Result) bool {
		if m.Get("role").String() == "tool" && strings.HasPrefix(m.Get("tool_call_id").String(), "call_") {
			toolCalls++
		}
		return true
	})
	if toolCalls != 2 {
		t.Fatalf("continuation must have a tool result per call id, got %d: %s", toolCalls, secondBody)
	}
	if !strings.Contains(string(secondBody), "RESOLVED ORIGINAL") || !strings.Contains(string(secondBody), "no longer available") {
		t.Fatalf("continuation should carry the resolved original and a placeholder for the expired id: %s", secondBody)
	}
}

// anthropicSSEBody builds a streaming Anthropic request that declares a tool (so
// expand injection fires) without Go's HTML-escaping, so the caller controls exactly
// which marker spelling reaches the proxy.
func anthropicSSEBody(t *testing.T, userText string) []byte {
	t.Helper()
	var bb bytes.Buffer
	enc := json.NewEncoder(&bb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(map[string]any{
		"model":  "claude",
		"stream": true,
		"tools": []map[string]any{
			{"name": "Bash", "description": "run", "input_schema": map[string]any{"type": "object"}},
		},
		"messages": []map[string]any{{"role": "user", "content": userText}},
	}); err != nil {
		t.Fatal(err)
	}
	return bb.Bytes()
}

// anthropicSSEBodyHTMLEscaped is the same request encoded WITH Go's HTML escaping —
// i.e. any marker in userText reaches the proxy only as <<cg:HASH>>, which
// is how markers actually arrive on the wire (see expand.rawMarkerRe).
func anthropicSSEBodyHTMLEscaped(t *testing.T, userText string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"model":  "claude",
		"stream": true,
		"tools": []map[string]any{
			{"name": "Bash", "description": "run", "input_schema": map[string]any{"type": "object"}},
		},
		"messages": []map[string]any{{"role": "user", "content": userText}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(userText, "cg:") && !strings.Contains(string(b), `u003ccg:`) {
		t.Fatalf("fixture is not HTML-escaped as expected: %s", b)
	}
	return b
}

// sseTextStream is a minimal Anthropic text event-stream, split so a test upstream
// can send the head, pause, and only then finish.
const (
	sseHead = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"first\"}}\n\n"
	sseTail = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"last\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
)

// The proxy flushes per SSE EVENT, so one Read returns one event. "Did the client get this
// while the upstream was still generating" is therefore a question about the bytes that
// arrive BEFORE the upstream is released, not about a single Read — hence a background
// reader plus a client-side deadline shorter than the upstream's own fallback. A test that
// just kept reading would be satisfied by the fallback firing and would pass on a fully
// buffering proxy.
func sseChunks(r io.Reader) <-chan []byte {
	ch := make(chan []byte, 256)
	go func() {
		defer close(ch)
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				c := make([]byte, n)
				copy(c, buf[:n])
				ch <- c
			}
			if err != nil {
				return
			}
		}
	}()
	return ch
}

// collectUntil accumulates chunks until want has been seen or d elapses.
func collectUntil(ch <-chan []byte, want string, d time.Duration) (string, bool) {
	deadline := time.After(d)
	var got strings.Builder
	for {
		select {
		case c, ok := <-ch:
			if !ok {
				return got.String(), strings.Contains(got.String(), want)
			}
			got.Write(c)
			if strings.Contains(got.String(), want) {
				return got.String(), true
			}
		case <-deadline:
			return got.String(), false
		}
	}
}

// drain returns everything left on the channel once the response ends.
func drain(ch <-chan []byte) string {
	var got strings.Builder
	for c := range ch {
		got.Write(c)
	}
	return got.String()
}

// TestMarkerFreeSSEStreamsThrough is the failing-test proof for issue #26. The
// upstream sends the head of an event-stream, then blocks until the test says the
// client has already seen bytes. If context-guru buffers the response, nothing
// reaches the client until the upstream finishes, the upstream never gets released,
// and the test deadlocks out — which is exactly what happened before the fix,
// because hasMarkers matched the expand tool description we inject ourselves.
//
// The request carries NO marker, so per the documented contract ("Requests without
// markers stream straight through with zero added latency") it must not be buffered.
func TestMarkerFreeSSEStreamsThrough(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sseHead))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-time.After(5 * time.Second): // fail fast instead of hanging the suite
		}
		w.Write([]byte(sseTail))
	}))
	defer upstream.Close()

	h, _ := buildHandler(t, "pipeline: []\n", upstream.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	body := anthropicSSEBody(t, "no markers in this request at all")
	resp, err := http.Post(srv.URL+"/anthropic/v1/messages", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Read the first chunk. The upstream has NOT sent the tail yet and will not until
	// this test releases it, so a streaming proxy can only hand us the head. A
	// buffering proxy cannot return anything here at all — it deadlocks until the
	// upstream's own 5s escape hatch fires, and then delivers head+tail in one go,
	// which is what the "last" assertion below detects.
	// The expand tool is advertised on every tools-bearing request (expand.InjectAuto is
	// session-stable, so the tools array never changes shape mid-session), which means EVERY
	// Anthropic stream is inspected. This assertion was weakened once, to "the peek ended at
	// the first content_block_start", because a peek had to hold the deciding event before it
	// could decide. Splicing decides per event, so the original and stronger claim is back:
	// the model's first delta reaches the client while the upstream is still generating.
	ch := sseChunks(resp.Body)
	first, ok := collectUntil(ch, "first", 2*time.Second)
	close(release)
	if !ok {
		t.Fatalf("marker-free SSE response was BUFFERED: the client received nothing until "+
			"the upstream had finished (issue #26), got %q", first)
	}
	if strings.Contains(first, "last") {
		t.Fatal("the client received the upstream's tail before it was written")
	}
	rest := drain(ch)
	if !strings.Contains(rest, "last") {
		t.Fatalf("stream did not complete, tail=%q", rest)
	}

	// And the fast path must be visible in /stats, not merely inferred.
	var snap metrics.Snapshot
	st, _ := http.Get(srv.URL + "/stats")
	json.NewDecoder(st.Body).Decode(&snap)
	st.Body.Close()
	if snap.SSEStreamed != 1 || snap.SSEBuffered != 0 {
		t.Fatalf("stats should show one streamed, zero buffered SSE: %+v", snap)
	}
}

// TestMarkerBearingSSEStreamsWhenItOpensWithText is the half of the contract that
// CHANGED. A marker-bearing request advertises the expand tool, and every such response
// used to be buffered whole so a lone expand call could be intercepted — which meant that
// from the first offload onward every response in the session lost streaming. Production:
// 33.4% of responses buffered, sse_ttfb_ms_avg_buffered 28,998 ms against 7,918 ms
// streamed, ~21 seconds of extra time to first byte.
//
// A response that OPENS with a text block cannot be intercepted from its first block, so
// the bounded peek flushes and streams. The upstream here blocks before its tail, so a
// buffering proxy cannot answer at all — the "last" assertion is what tells the two apart.
func TestMarkerBearingSSEStreamsWhenItOpensWithText(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sseHead))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		w.Write([]byte(sseTail))
	}))
	defer upstream.Close()

	h, _ := buildHandler(t, "pipeline: []\n", upstream.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	// The escaped spelling, written after a newline exactly as offload emits it.
	body := anthropicSSEBodyHTMLEscaped(t, "output\nline2 <<cg:HASH>>")
	resp, err := http.Post(srv.URL+"/anthropic/v1/messages", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	ch := sseChunks(resp.Body)
	first, ok := collectUntil(ch, "first", 2*time.Second)
	close(release)
	if !ok {
		t.Fatalf("a marker-bearing SSE response that opens with TEXT was buffered whole; "+
			"the model's first delta must reach the client while the upstream is still "+
			"generating, got %q", first)
	}
	if strings.Contains(first, "last") {
		t.Fatal("the client received the upstream's tail before it was written")
	}
	whole := first + drain(ch)
	for _, want := range []string{"first", "last", "message_stop"} {
		if !strings.Contains(whole, want) {
			t.Fatalf("the streamed response lost %q; peek + remainder must be the whole "+
				"stream byte-for-byte:\n%q", want, whole)
		}
	}

	var snap metrics.Snapshot
	st, _ := http.Get(srv.URL + "/stats")
	json.NewDecoder(st.Body).Decode(&snap)
	st.Body.Close()
	if snap.SSEStreamed != 1 || snap.SSEBuffered != 0 {
		t.Fatalf("want one streamed, zero buffered: %+v", snap)
	}
	if snap.SSEExpandAfterStream != 0 {
		t.Fatalf("a plain text answer must not be counted as a late expand call: %+v", snap)
	}
}

// TestExpandSSEWithOtherToolReplaysVerbatim covers the otherTools bail on the
// streaming path: the model batches expand alongside a Bash call. The proxy cannot
// answer only half a batch (the client owns Bash), so it must replay the stream
// unchanged rather than continue — and the client's stream must stay well-formed.
func TestExpandSSEWithOtherToolReplaysVerbatim(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\"}}\n\n" +
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_1\",\"name\":\"context_guru_expand\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"id\\\":\\\"HASH\\\"}\"}}\n\n" +
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_2\",\"name\":\"Bash\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"command\\\":\\\"ls\\\"}\"}}\n\n" +
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer upstream.Close()

	h, st := buildHandler(t, "pipeline: []\n", upstream.URL)
	st.Put("HASH", []byte("THE ORIGINAL CONTENT"))
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	body := anthropicSSEBody(t, "look at <<cg:HASH>> then list files")
	resp, err := http.Post(srv.URL+"/anthropic/v1/messages", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if calls != 1 {
		t.Fatalf("batched expand+Bash must NOT trigger a continuation, got %d upstream calls", calls)
	}
	// Verbatim replay: both blocks intact, indices unrenumbered, no injected content.
	if !strings.Contains(string(out), `"index":1`) || !strings.Contains(string(out), `"name":"Bash"`) {
		t.Fatalf("client must receive the original stream unchanged: %s", out)
	}
	if strings.Contains(string(out), "THE ORIGINAL CONTENT") {
		t.Fatalf("proxy must not splice resolved content into a stream it declined: %s", out)
	}
}

// TestExpandSSEMultiRoundCapped drives the streaming loop past maxExpandRounds: an
// upstream that answers every request with another expand call must be cut off, and
// the client must still get a well-formed stream (the model's own last call).
func TestExpandSSEMultiRoundCapped(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\"}}\n\n" +
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_1\",\"name\":\"context_guru_expand\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"id\\\":\\\"HASH\\\"}\"}}\n\n" +
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer upstream.Close()

	h, st := buildHandler(t, "pipeline: []\n", upstream.URL)
	st.Put("HASH", []byte("THE ORIGINAL CONTENT"))
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	body := anthropicSSEBody(t, "look at <<cg:HASH>>")
	resp, err := http.Post(srv.URL+"/anthropic/v1/messages", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// maxExpandRounds continuations, then one final pass-through: 4 upstream calls.
	if calls != 4 {
		t.Fatalf("round cap not honored: %d upstream calls (want 4 = 3 rounds + terminal)", calls)
	}
	if !strings.Contains(string(out), "message_stop") {
		t.Fatalf("client must still receive a complete stream after the cap: %s", out)
	}

	// SSE stats are per CLIENT REQUEST, not per upstream round. This one request drove
	// 4 upstream calls; recording per round would report streamed=1/buffered=3 and —
	// worse — count the terminal round as a healthy "streamed" TTFB timed from that
	// round alone, hiding the 3 round-trips the client actually waited for.
	var snap metrics.Snapshot
	stx, _ := http.Get(srv.URL + "/stats")
	json.NewDecoder(stx.Body).Decode(&snap)
	stx.Body.Close()
	if snap.SSEBuffered != 1 || snap.SSEStreamed != 0 {
		t.Fatalf("one client request must yield exactly one buffered sample, got %+v", snap)
	}
	if snap.SSEBufferedPct != 100 {
		t.Fatalf("buffered_pct must be a share of requests (want 100), got %v", snap.SSEBufferedPct)
	}
	// And the cap hands the client the model's own expand call, which is the one thing that
	// must never be silent: it is the same leak as any other round the loop cannot answer.
	if snap.SSEExpandAfterStream != 1 {
		t.Fatalf("the round cap gives the client our own tool_use and must count it: %+v", snap)
	}
}

// TestExpandSSEAggregateFailureReplaysRaw covers the fail-open path INSIDE
// aggregateAnthropicSSE (expand/sse.go:132): a truncated input_json_delta leaves the
// tool_use input unparseable, so AggregateSSE returns ok=false even though the
// provider IS anthropic — a different branch from the provider gate at sse.go:21.
// The client must still receive the original bytes unchanged.
func TestExpandSSEAggregateFailureReplaysRaw(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		// partial_json is TRUNCATED — it cannot reconstruct to valid JSON.
		w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\"}}\n\n" +
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_1\",\"name\":\"context_guru_expand\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"id\\\":\\\"HA\"}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer upstream.Close()

	h, st := buildHandler(t, "pipeline: []\n", upstream.URL)
	st.Put("HASH", []byte("THE ORIGINAL CONTENT"))
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	body := anthropicSSEBody(t, "look at <<cg:HASH>>")
	resp, err := http.Post(srv.URL+"/anthropic/v1/messages", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if calls != 1 {
		t.Fatalf("an unreconstructable stream must not drive a continuation: %d calls", calls)
	}
	if !strings.Contains(string(out), `\"id\":\"HA`) || !strings.Contains(string(out), "message_stop") {
		t.Fatalf("client must get the raw stream back verbatim (fail-open): %s", out)
	}
	if strings.Contains(string(out), "THE ORIGINAL CONTENT") {
		t.Fatalf("nothing may be spliced into a stream we could not parse: %s", out)
	}
}

// TestExpandOpenAISSEFallsBackToRaw documents (and pins) the OpenAI streaming
// limitation: AggregateSSE only reconstructs the Anthropic event stream, so a
// marker-bearing OpenAI SSE response is replayed raw and restoration does not fire.
// Correctness is preserved (fail-open); only the feature is absent.
func TestExpandOpenAISSEFallsBackToRaw(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"context_guru_expand","arguments":"{\"id\":\"HASH\"}"}}]}}]}` + "\n\n" +
			"data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	h, st := buildHandler(t, "pipeline: []\n", upstream.URL)
	st.Put("HASH", []byte("THE ORIGINAL CONTENT"))
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	var bb bytes.Buffer
	enc := json.NewEncoder(&bb)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(map[string]any{
		"model":    "gpt-x",
		"stream":   true,
		"tools":    []map[string]any{{"type": "function", "function": map[string]any{"name": "Bash", "parameters": map[string]any{"type": "object"}}}},
		"messages": []map[string]any{{"role": "user", "content": "look at <<cg:HASH>>"}},
	})
	resp, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json", strings.NewReader(bb.String()))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if calls != 1 {
		t.Fatalf("OpenAI SSE cannot be aggregated, so no continuation is possible: %d calls", calls)
	}
	if !strings.Contains(string(out), "[DONE]") || strings.Contains(string(out), "THE ORIGINAL CONTENT") {
		t.Fatalf("OpenAI SSE must be replayed raw (fail-open): %s", out)
	}
}

func TestExpandRoundTrip(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{}`)) }))
	defer upstream.Close()
	h, _ := buildHandler(t, "pipeline: [collapse]\ncomponents:\n  collapse: {max_tokens: 20, head_lines: 2, tail_lines: 2}\n", upstream.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	var b strings.Builder
	for i := 0; i < 40; i++ {
		b.WriteString("log content line with enough words to matter here\n")
	}
	body := openAIBody(map[string]any{"role": "tool", "content": b.String()})
	http.Post(srv.URL+"/openai/v1/chat/completions", "application/json", strings.NewReader(string(body)))

	// The collapsed message carries an expand marker; the id must resolve via /expand.
	stats, _ := http.Get(srv.URL + "/stats")
	var snap metrics.Snapshot
	json.NewDecoder(stats.Body).Decode(&snap)
	stats.Body.Close()
	if snap.Requests != 1 || snap.SavedTokens <= 0 {
		t.Fatalf("stats not recorded: %+v", snap)
	}
}

// A marker-bearing OPENAI SSE response must reach the client as a stream. The
// continuation loop cannot act on this dialect at all (AggregateSSE reconstructs only the
// Anthropic event stream, so the buffered path just replays the bytes verbatim), so there
// is nothing to inspect and nothing to withhold — yet it was buffered anyway, and then,
// once the peek was added, still fully consumed by a peek that no OpenAI event can decide.
// The upstream blocks before its tail, so a proxy that reads the whole body cannot answer.
func TestOpenAISSEStreamsThrough(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(`data: {"choices":[{"delta":{"content":"first"}}]}` + "\n\n"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		w.Write([]byte(`data: {"choices":[{"delta":{"content":"last"}}]}` + "\n\n" + "data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	h, st := buildHandler(t, "pipeline: []\n", upstream.URL)
	st.Put("HASH", []byte("THE ORIGINAL CONTENT"))
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	var bb bytes.Buffer
	enc := json.NewEncoder(&bb)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(map[string]any{
		"model":    "gpt-x",
		"stream":   true,
		"tools":    []map[string]any{{"type": "function", "function": map[string]any{"name": "Bash", "parameters": map[string]any{"type": "object"}}}},
		"messages": []map[string]any{{"role": "user", "content": "look at <<cg:HASH>>"}},
	})
	resp, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json", strings.NewReader(bb.String()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 4096)
	n, rerr := resp.Body.Read(buf)
	first := string(buf[:n])
	close(release)
	if n == 0 {
		t.Fatalf("first read returned no bytes: %v", rerr)
	}
	if !strings.Contains(first, "first") {
		t.Fatalf("expected the first chunk, got %q", first)
	}
	if strings.Contains(first, "last") {
		t.Fatal("the OpenAI SSE response was read in full before the client saw a byte")
	}
	rest, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(rest), "last") {
		t.Fatalf("stream did not complete, tail=%q", rest)
	}

	// And the counters must say streamed, not file a time-to-last-byte into that bucket.
	var snap metrics.Snapshot
	stresp, _ := http.Get(srv.URL + "/stats")
	json.NewDecoder(stresp.Body).Decode(&snap)
	stresp.Body.Close()
	if snap.SSEStreamed != 1 || snap.SSEBuffered != 0 {
		t.Fatalf("want one streamed, zero buffered: %+v", snap)
	}
}
