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
)

// These tests PIN what a client receives when the model calls context_guru_expand on an agent
// compaction turn. A compaction request is not bypassed, so it carries our tools like every
// earlier turn of its thread (that keeps the cache prefix). The prompt tells the model to call no
// tool, so a call is rare. When it happens the outcome depends on the dialect and on streaming.
// The cases that end in a raw tool call are known gaps, documented in
// docs/how-to/agent-compaction.md. A test here failing means the behavior changed: update the
// docs with it.

const anthropicExpandJSON = `{"id":"m1","type":"message","role":"assistant","model":"claude","content":[` +
	`{"type":"tool_use","id":"c1","name":"context_guru_expand","input":{"id":"HASH"}}],` +
	`"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":1}}`

const anthropicTextJSON = `{"id":"m2","type":"message","role":"assistant","model":"claude","content":[` +
	`{"type":"text","text":"SUMMARY"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":1}}`

// compactionWithMarker is a compaction request whose history holds a marker, on a pipeline that
// can mint one, so the expand tool is advertised.
func compactionWithMarker(t *testing.T, anthropic, stream bool) []byte {
	t.Helper()
	msgs := []map[string]any{
		{"role": "user", "content": "earlier output <<cg:HASH>>"},
		{"role": "user", "content": ccCompactPrompt},
	}
	if anthropic {
		if stream {
			return streamingToolsRequest(t, msgs...)
		}
		return toolsRequest(t, msgs...)
	}
	b, err := json.Marshal(map[string]any{
		"model": "gpt-x", "stream": stream,
		"tools":    []any{map[string]any{"type": "function", "function": map[string]any{"name": "read_file"}}},
		"messages": msgs,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func postCompaction(t *testing.T, upstream http.HandlerFunc, route string, body []byte) (out string, rounds *atomic.Int32) {
	t.Helper()
	rounds = new(atomic.Int32)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rounds.Add(1)
		upstream(w, r)
	}))
	defer up.Close()
	h, st := buildHandler(t, offloadCapablePipeline, up.URL)
	st.Put("HASH", []byte("THE ORIGINAL CONTENT"))
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	resp, err := http.Post(srv.URL+route, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), rounds
}

func writeSSE(w http.ResponseWriter, s string) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte(s))
}

// Anthropic, JSON: one expand call, then text. The proxy answers the call and the client gets text.
func TestCompactionExpandOnceAnthropicJSONGivesTheClientText(t *testing.T) {
	out, rounds := postCompaction(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "tool_result") {
			_, _ = w.Write([]byte(anthropicExpandJSON))
			return
		}
		_, _ = w.Write([]byte(anthropicTextJSON))
	}, "/anthropic/v1/messages", compactionWithMarker(t, true, false))
	if rounds.Load() != 2 || !strings.Contains(out, "SUMMARY") || strings.Contains(out, "context_guru_expand") {
		t.Fatalf("rounds=%d, want 2 and a text-only answer: %s", rounds.Load(), out)
	}
}

// Anthropic, SSE: the same, streamed. This is the Claude Code path.
func TestCompactionExpandOnceAnthropicSSEGivesTheClientText(t *testing.T) {
	head, tail := leadThenExpand("text")
	out, rounds := postCompaction(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "tool_result") {
			writeSSE(w, head+tail)
			return
		}
		writeSSE(w, round2Answer)
	}, "/anthropic/v1/messages", compactionWithMarker(t, true, true))
	if rounds.Load() != 2 || !strings.Contains(out, "ANSWERED") || strings.Contains(out, "context_guru_expand") {
		t.Fatalf("rounds=%d, want 2 and a text-only stream: %s", rounds.Load(), out)
	}
}

// Known gap (b), JSON: a model that calls expand on every round gets 1 + maxExpandRounds (3)
// upstream rounds, then the client receives the raw tool call and no summary.
func TestCompactionExpandOnEveryRoundAnthropicJSONLeaksTheToolCallAfterTheCap(t *testing.T) {
	out, rounds := postCompaction(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(anthropicExpandJSON))
	}, "/anthropic/v1/messages", compactionWithMarker(t, true, false))
	if rounds.Load() != 4 || !strings.Contains(out, "context_guru_expand") || strings.Contains(out, "SUMMARY") {
		t.Fatalf("rounds=%d, want 4 and the raw expand call: %s", rounds.Load(), out)
	}
}

// Known gap (b), SSE.
func TestCompactionExpandOnEveryRoundAnthropicSSELeaksTheToolCallAfterTheCap(t *testing.T) {
	head, tail := leadThenExpand("text")
	out, rounds := postCompaction(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, head+tail)
	}, "/anthropic/v1/messages", compactionWithMarker(t, true, true))
	if rounds.Load() != 4 || !strings.Contains(out, "context_guru_expand") {
		t.Fatalf("rounds=%d, want 4 and the raw expand call: %s", rounds.Load(), out)
	}
}

// Known gap (a): OpenAI dialect with stream:true. The proxy does not inspect that stream, so an
// expand call reaches the client as a raw tool call after one upstream round.
func TestCompactionExpandOpenAISSELeaksTheToolCall(t *testing.T) {
	out, rounds := postCompaction(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function",`+
			`"function":{"name":"context_guru_expand","arguments":"{\"id\":\"HASH\"}"}}]}}]}`+"\n\n"+
			"data: [DONE]\n\n")
	}, "/openai/v1/chat/completions", compactionWithMarker(t, false, true))
	if rounds.Load() != 1 || !strings.Contains(out, "context_guru_expand") {
		t.Fatalf("rounds=%d, want 1 and the raw expand call: %s", rounds.Load(), out)
	}
}
