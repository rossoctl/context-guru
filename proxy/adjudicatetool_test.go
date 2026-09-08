package proxy_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/internal/adjudicate"
)

// sweepPipeline is a pipeline that CONTAINS extract_llm_sweep, i.e. one that can actually adjudicate.
// The gate on the injection is (Anthropic route AND this component present), so a fixture built with
// `pipeline: []` cannot exercise the injection at all — and a test that asserted the tool appeared
// under `pipeline: []` was asserting the defect: the tool was reaching the `off` control arm of every
// published comparison. See proxy.chat.
const sweepPipeline = "pipeline: [extract_llm_sweep]\n"

// forwardedBody posts one request through the proxy on the ANTHROPIC route with a sweep-bearing
// pipeline, and returns exactly what reached upstream.
func forwardedBody(t *testing.T, body []byte) []byte {
	t.Helper()
	return forwardedOn(t, "/anthropic/v1/messages", sweepPipeline, body)
}

// forwardedOn is forwardedBody with the route and pipeline spelled out, for the cases whose whole
// point is that one of those two differs.
func forwardedOn(t *testing.T, route, yaml string, body []byte) []byte {
	t.Helper()
	var up upstreamCapture
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.record(r)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(route, "anthropic") {
			_, _ = w.Write([]byte(`{"id":"m1","type":"message","role":"assistant","model":"claude",` +
				`"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",` +
				`"usage":{"input_tokens":5,"output_tokens":1}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()
	h, _ := buildHandler(t, yaml, upstream.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	resp, err := http.Post(srv.URL+route, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return up.last().body
}

// toolsRequest is the ANTHROPIC dialect, because that is the only dialect the injection targets:
// prefixAskerFor returns nil for every other provider and cheapmodel/openai.go has no CompletePrefixed
// at all, so the definition could never be read there.
func toolsRequest(t *testing.T, msgs ...map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"model":      "claude-sonnet-5",
		"max_tokens": 64,
		"tools": []any{map[string]any{"name": "read_file", "description": "read a file",
			"input_schema": map[string]any{"type": "object"}}},
		"messages": msgs,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// streamingToolsRequest is toolsRequest with stream:true, for the SSE splice path.
func streamingToolsRequest(t *testing.T, msgs ...map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"model":      "claude-sonnet-5",
		"max_tokens": 64,
		"stream":     true,
		"tools": []any{map[string]any{"name": "read_file", "description": "read a file",
			"input_schema": map[string]any{"type": "object"}}},
		"messages": msgs,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// toolsRequestOpenAI is the same request in the OpenAI dialect, for the provider half of the gate.
func toolsRequestOpenAI(t *testing.T, msgs ...map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"model":    "gpt-x",
		"tools":    []any{map[string]any{"type": "function", "function": map[string]any{"name": "read_file"}}},
		"messages": msgs,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ADVERTISED ON EVERY TURN, not only on the turn a sweep is about to ask. `tools` hashes ahead of
// system and messages, so a tool that appears when the sweep fires and disappears on the next turn
// invalidates the cached prefix from position zero — the flap expand's InjectAuto exists to prevent.
// And the declaration is the whole mechanism, measured over three passes per arm on the same transcript:
// with the tool declared and no tool_choice, unparseable replies ran 9.1% (7 of 77 replied asks) against
// 30.0% (6 of 20) on main's tool_choice:none — Fisher two-tailed p = 0.0245 — and 55.8% of replies came
// back carrying a tool_use where main produced none at all. Dropping tool_choice WITHOUT declaring the
// tool is worse than main (58.3% unparseable), which is why the DECLARATION and not the tool_choice
// removal is what earns its place. The "6 of 6 against 0 of 6" this comment used to cite was a six-item
// hand pass and is retracted: main returns verdicts on 71.5% of the items it asks about, so the defect
// is that ~30% of its asks come back unusable, not that all of them do.
func TestAdjudicateToolAdvertisedOnEveryTurn(t *testing.T) {
	got := forwardedBody(t, toolsRequest(t, map[string]any{"role": "user", "content": "go"}))
	if len(got) == 0 {
		t.Fatal("nothing reached upstream")
	}
	if !strings.Contains(string(got), adjudicate.ToolName) {
		t.Fatalf("the adjudication tool was not advertised on an ordinary turn: %s", got)
	}
	// Appended last, after the client's own tool, so the client's order is untouched.
	tools := gjson.GetBytes(got, "tools").Array()
	if n := len(tools); n < 2 || tools[n-1].Get("name").String() != adjudicate.ToolName {
		t.Errorf("our tool is not last in the tools array: %s", gjson.GetBytes(got, "tools").Raw)
	}
	if tools[0].Get("name").String() != "read_file" {
		t.Error("the client's own tool moved; its order must be preserved exactly")
	}
	// A turn with nothing to adjudicate must carry it too — that is the point of injecting always.
	got2 := forwardedBody(t, toolsRequest(t, map[string]any{"role": "user", "content": "and again"}))
	if !strings.Contains(string(got2), adjudicate.ToolName) {
		t.Errorf("the tool came and went between turns, which discards the whole cached prefix: %s", got2)
	}
}

// A bypassed compaction request must not get it, for the same reason expand skips one: bypass promises
// a byte-identical forward.
func TestAdjudicateToolNotAdvertisedOnAnAgentCompaction(t *testing.T) {
	got := forwardedBody(t, toolsRequest(t, map[string]any{"role": "user", "content": ccCompactPrompt}))
	if strings.Contains(string(got), adjudicate.ToolName) {
		t.Errorf("injected into a bypassed compaction, which must forward byte-identically: %s", got)
	}
}

// A stray call the AGENT made must be answered on the request path. The client cannot execute a tool
// the proxy injected, so it answers "not found" and the agent loses a turn to a dead end.
func TestAdjudicateStrayCallIsAnsweredOnTheRequestPath(t *testing.T) {
	before := adjudicate.StrayAnswered()
	got := forwardedBody(t, toolsRequest(t,
		map[string]any{"role": "user", "content": "go"},
		map[string]any{"role": "assistant", "content": []any{map[string]any{
			"type": "tool_use", "id": "c1", "name": adjudicate.ToolName, "input": map[string]any{},
		}}},
		map[string]any{"role": "user", "content": []any{map[string]any{
			"type": "tool_result", "tool_use_id": "c1", "is_error": true,
			"content": "Error: No such tool available: " + adjudicate.ToolName,
		}}},
	))
	if strings.Contains(string(got), "No such tool available") {
		t.Errorf("the client's dead-end refusal was forwarded to the model unchanged: %s", got)
	}
	if !strings.Contains(string(got), "runs automatically") {
		t.Errorf("no substitute answer was written: %s", got)
	}
	if adjudicate.StrayAnswered() == before {
		t.Error("the stray was not counted; adjudicate_stray is the only signal that the tool's " +
			"description stopped working")
	}
}

// THE GATE, half one: a pipeline with no extract_llm_sweep can never adjudicate, so advertising the
// tool there buys nothing and costs the cacheable prefix. `off` is the A/B CONTROL ARM of every
// published comparison in this repo, and codesmart is a shipped preset with no sweep in it; injecting
// unconditionally perturbed both by a measured 946 bytes at the head of the prefix on every request.
func TestAdjudicateToolNotAdvertisedWhenThePipelineCannotAdjudicate(t *testing.T) {
	for _, yaml := range []string{"pipeline: []\n", "pipeline: [format]\n"} {
		got := forwardedOn(t, "/anthropic/v1/messages", yaml,
			toolsRequest(t, map[string]any{"role": "user", "content": "go"}))
		if len(got) == 0 {
			t.Fatalf("%s: nothing reached upstream", yaml)
		}
		if strings.Contains(string(got), adjudicate.ToolName) {
			t.Errorf("%s: advertised on a pipeline that cannot adjudicate: %s", yaml, got)
		}
	}
}

// THE GATE, half two: the provider. prefixAskerFor returns nil for anything but Anthropic and
// cheapmodel/openai.go has no CompletePrefixed at all, so on the OpenAI route the definition is
// unreachable by construction — it was pure prefix cost, ~217 tokens per request.
func TestAdjudicateToolNotAdvertisedOnANonAnthropicRoute(t *testing.T) {
	got := forwardedOn(t, "/openai/v1/chat/completions", sweepPipeline,
		toolsRequestOpenAI(t, map[string]any{"role": "user", "content": "go"}))
	if len(got) == 0 {
		t.Fatal("nothing reached upstream")
	}
	if strings.Contains(string(got), adjudicate.ToolName) {
		t.Errorf("advertised on a route that can never read it: %s", got)
	}
}

// THE LEAK, non-streaming path. A tool_use for a proxy-injected tool must never reach the client: the
// client never declared it, cannot execute it, and answers "not found", losing the agent a turn. It
// must instead be answered IN BAND, before the client is written to, which leaves the request-path
// repair as a backstop rather than the primary defence.
func TestAdjudicateStrayCallDoesNotReachTheClientOnTheJSONPath(t *testing.T) {
	var up upstreamCapture
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		round := up.record(r)
		w.Header().Set("Content-Type", "application/json")
		if round == 1 {
			// The model calls OUR tool, which is always a defect by construction.
			_, _ = w.Write([]byte(`{"id":"m1","type":"message","role":"assistant","model":"claude",` +
				`"content":[{"type":"tool_use","id":"stray1","name":"` + adjudicate.ToolName +
				`","input":{"verdicts":[]}}],"stop_reason":"tool_use",` +
				`"usage":{"input_tokens":5,"output_tokens":1}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"m2","type":"message","role":"assistant","model":"claude",` +
			`"content":[{"type":"text","text":"done"}],"stop_reason":"end_turn",` +
			`"usage":{"input_tokens":6,"output_tokens":2}}`))
	}))
	defer upstream.Close()
	h, _ := buildHandler(t, sweepPipeline, upstream.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/anthropic/v1/messages", "application/json",
		bytes.NewReader(toolsRequest(t, map[string]any{"role": "user", "content": "go"})))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// PRECONDITION: the loop must actually have run a second round, or this asserts nothing.
	if round := up.hits(); round < 2 {
		t.Fatalf("the stray was never intercepted -- only %d upstream round(s), client got: %s",
			round, got)
	}
	second := up.body(2)
	if strings.Contains(string(got), adjudicate.ToolName) {
		t.Errorf("the proxy-injected tool_use reached the CLIENT: %s", got)
	}
	// Answered in band, so the model could finish its turn rather than wait for a tool nobody runs.
	if !strings.Contains(string(second), "runs automatically") {
		t.Errorf("the stray was withheld but never answered upstream: %s", second)
	}
	if !strings.Contains(string(got), "done") {
		t.Errorf("the client did not receive the finished turn: %s", got)
	}
}

// THE HOLE IN "answered in band on both paths", pinned rather than papered over. When the model calls
// our tool AND a client tool in the SAME assistant turn, expand.ResponseCalls reports otherTools, the
// response loop bail()s, and our tool_use reaches the client raw — the loop DOES see this path and
// defers it deliberately, because it cannot continue a turn whose other tool_use only the client can
// execute without either inventing a result for the client's tool or dropping the client's call.
//
// This test asserts what actually happens today, not what would be nicer: the leak this turn, the
// repair on the next request, the client's own tool_result untouched, and the stray counted exactly
// once. It exists so that the behaviour is a decision on the record rather than something a later
// change "fixes" by guessing. If the deferral is ever replaced by a real in-band answer for co-called
// turns, this test SHOULD fail and be rewritten — that is the point of pinning it.
func TestAdjudicateStrayCoCalledWithClientToolLeaks(t *testing.T) {
	// The assistant turn both requests share: one call to the CLIENT's tool, one to ours.
	assistantCoCall := map[string]any{"role": "assistant", "content": []any{
		map[string]any{"type": "tool_use", "id": "cli1", "name": "read_file",
			"input": map[string]any{"path": "a.go"}},
		map[string]any{"type": "tool_use", "id": "stray1", "name": adjudicate.ToolName,
			"input": map[string]any{"verdicts": []any{}}},
	}}

	// --- Turn 1: the leak. -------------------------------------------------------------------
	var rounds atomic.Int64 // an atomic carries the happens-before edge the round trip does not
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		rounds.Add(1)
		w.Header().Set("Content-Type", "application/json")
		blocks, _ := json.Marshal(assistantCoCall["content"])
		_, _ = w.Write([]byte(`{"id":"m1","type":"message","role":"assistant","model":"claude",` +
			`"content":` + string(blocks) + `,"stop_reason":"tool_use",` +
			`"usage":{"input_tokens":5,"output_tokens":2}}`))
	}))
	defer upstream.Close()
	h, _ := buildHandler(t, sweepPipeline, upstream.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/anthropic/v1/messages", "application/json",
		bytes.NewReader(toolsRequest(t, map[string]any{"role": "user", "content": "go"})))
	if err != nil {
		t.Fatal(err)
	}
	leaked, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// PRECONDITION: exactly one upstream round. A continuation would mean the loop answered in band
	// after all, and then the rest of this test is asserting nothing about the co-call path.
	if rounds := rounds.Load(); rounds != 1 {
		t.Fatalf("expected the loop to bail on otherTools after ONE round, got %d — the co-call path "+
			"no longer defers, so this test's premise is gone: %s", rounds, leaked)
	}
	// The leak itself. Asserted, not lamented: this is the documented cost of the deferral.
	if !strings.Contains(string(leaked), adjudicate.ToolName) {
		t.Errorf("expected the co-called proxy tool_use to reach the client this turn (the known, "+
			"deliberate deferral); it did not, so the response loop changed: %s", leaked)
	}
	if !strings.Contains(string(leaked), "cli1") {
		t.Errorf("the CLIENT's own tool_use did not survive the round: %s", leaked)
	}

	// --- Turn 2: the repair. -----------------------------------------------------------------
	// The client did what a client does: ran its own tool, and answered ours "not found" because the
	// PROXY injected it and the client never declared it.
	before := adjudicate.StrayAnswered()
	got := forwardedBody(t, toolsRequest(t,
		map[string]any{"role": "user", "content": "go"},
		assistantCoCall,
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "cli1",
				"content": "package main"},
			map[string]any{"type": "tool_result", "tool_use_id": "stray1", "is_error": true,
				"content": "Error: No such tool available: " + adjudicate.ToolName},
		}},
	))
	if strings.Contains(string(got), "No such tool available") {
		t.Errorf("the client's dead-end refusal reached the model unchanged: %s", got)
	}
	if !strings.Contains(string(got), "runs automatically") {
		t.Errorf("the stray was leaked AND never repaired on the next request: %s", got)
	}
	// The client's own tool_result must come through untouched — the repair keys off the tool_use it
	// answers, so a bug here would rewrite somebody else's result.
	blocks := gjson.GetBytes(got, `messages.2.content`).Array()
	if len(blocks) != 2 {
		t.Fatalf("the repaired turn does not have both tool_results: %s", got)
	}
	if c := blocks[0].Get("content").String(); c != "package main" {
		t.Errorf("the CLIENT's tool_result was rewritten to %q; the repair must only touch ours", c)
	}
	if blocks[0].Get("is_error").Exists() {
		t.Errorf("is_error was invented on the client's own tool_result: %s", blocks[0].Raw)
	}
	// Ours: answered, and no longer an error — leaving is_error set tells the model its call failed
	// while handing it that call's answer.
	if blocks[1].Get("is_error").Bool() {
		t.Errorf("is_error stayed set on the repaired block: %s", blocks[1].Raw)
	}
	if n := adjudicate.StrayAnswered() - before; n != 1 {
		t.Errorf("the leaked stray was counted %d times, want exactly 1 — adjudicate_stray is the "+
			"only signal that the tool's description stopped working", n)
	}
}

// THE LEAK, streaming path. The splicer withheld only the expand tool by name, so an adjudication call
// streamed through event by event and the client saw it live.
func TestAdjudicateStrayCallDoesNotReachTheClientOnTheSSEPath(t *testing.T) {
	var round atomic.Int64 // an atomic carries the happens-before edge the round trip does not
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		n := round.Add(1)
		if n == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			for _, ev := range []string{
				`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","model":"claude","content":[],"usage":{"input_tokens":5,"output_tokens":0}}}`,
				`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"stray1","name":"` + adjudicate.ToolName + `","input":{}}}`,
				`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
				`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`,
				`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
			} {
				_, _ = w.Write([]byte(ev + "\n\n"))
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
			return
		}
		// The continuation MUST also stream. The request said stream:true, and a JSON body on a
		// later round is a documented upstream anomaly that cannot be spliced into an event
		// stream -- the loop then hands the withheld events back, which is the very leak this
		// test is trying to observe. A fixture that answers in JSON therefore fails for a reason
		// that has nothing to do with the withhold set.
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, ev := range []string{
			`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"m2","type":"message","role":"assistant","model":"claude","content":[],"usage":{"input_tokens":6,"output_tokens":0}}}`,
			`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}`,
			`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
			`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
			`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
		} {
			_, _ = w.Write([]byte(ev + "\n\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer upstream.Close()
	h, _ := buildHandler(t, sweepPipeline, upstream.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	body := streamingToolsRequest(t, map[string]any{"role": "user", "content": "go"})
	resp, err := http.Post(srv.URL+"/anthropic/v1/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if round := round.Load(); round < 2 {
		t.Fatalf("the streamed stray was never intercepted -- only %d round(s), client got: %s",
			round, got)
	}
	if strings.Contains(string(got), adjudicate.ToolName) {
		t.Errorf("the proxy-injected tool_use was STREAMED to the client: %s", got)
	}
	if !strings.Contains(string(got), "done") {
		t.Errorf("the client did not receive the finished turn: %s", got)
	}
}
