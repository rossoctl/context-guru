package proxy_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// An expand call that resolves NOTHING must still complete the turn.
//
// This is what pays for advertising the tool on every request in a session (see
// expand.InjectAuto: the tools array has to be byte-stable or every change to it discards
// the whole cached prefix). Advertising on a turn with nothing to expand invites a call
// against an id that is not in the store — an id that has aged out, or a model that
// invented one — and the old behaviour was to replay the model's raw tool_use to the
// client. A client implements no such tool, so it receives a turn with a tool call it
// cannot answer and no text; on an agent's own compaction request Claude Code reads that as
// a summary that came back empty, and three of them disable auto-compact for the session.
//
// `resolved` already carries "[expand: original for id … is no longer available]" for every
// unresolved id, so the continuation is well formed whether anything came back or not. The
// fix is to send it.
func TestAnExpandCallThatResolvesNothingStillFinishesTheTurn(t *testing.T) {
	var rounds atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if rounds.Add(1) == 1 {
			// Round 1: the model asks for an id that is not in the store.
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[` +
				`{"id":"c1","type":"function","function":{"name":"context_guru_expand",` +
				`"arguments":"{\"id\":\"GONE\"}"}}]}}]}`))
			return
		}
		// Round 2 must carry the placeholder, or the model is being asked to continue with
		// an unanswered tool call and the provider will reject the request outright.
		if !strings.Contains(string(body), "no longer available") {
			t.Errorf("round 2 does not carry the placeholder tool_result: %s", body)
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"proceeding without it"}}]}`))
	}))
	defer upstream.Close()

	h, _ := buildHandler(t, offloadCapablePipeline, upstream.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	body, err := json.Marshal(map[string]any{
		"model":    "gpt-x",
		"tools":    []any{map[string]any{"type": "function", "function": map[string]any{"name": "read_file"}}},
		"messages": []any{map[string]any{"role": "user", "content": "go"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if got := rounds.Load(); got != 2 {
		t.Fatalf("upstream rounds = %d, want 2: a call that resolved nothing must be answered "+
			"with the placeholder and re-invoked, not replayed to the client", got)
	}
	if strings.Contains(string(out), "context_guru_expand") {
		t.Errorf("the client received the model's raw expand tool_use, which it cannot "+
			"answer; on a compaction request that counts as a failed summary: %s", out)
	}
	if !strings.Contains(string(out), "proceeding without it") {
		t.Errorf("the client did not receive the model's finished turn: %s", out)
	}
}
