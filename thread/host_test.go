package thread_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/context-guru/store"
	"github.com/rossoctl/context-guru/thread"
)

// hostBody builds an Anthropic-dialect body that declares one tool, so its thread is a
// main-agent candidate. Even indexes are user turns, odd ones assistant turns.
func hostBody(texts ...string) []byte {
	msgs := make([]any, len(texts))
	for i, t := range texts {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs[i] = map[string]any{"role": role, "content": []any{map[string]any{"type": "text", "text": t}}}
	}
	b, _ := json.Marshal(map[string]any{
		"model": "claude-sonnet-5", "system": "sys", "messages": msgs,
		"tools": []any{map[string]any{"name": "Read", "input_schema": map[string]any{"type": "object"}}},
	})
	return b
}

// host is an embedding host (#1247) that sets apply.Opts.ThreadOf the way the doc of that field
// says, with exported API only: this file is package thread_test, so it cannot reach anything
// unexported.
type host struct {
	tracker *thread.Tracker
	store   store.Store
	pipe    *components.Pipeline
}

func (h *host) serve(t *testing.T, header http.Header, body []byte) apply.Trace {
	t.Helper()
	orig := body
	agentID := strings.TrimSpace(header.Get("x-claude-code-agent-id"))
	if !thread.ValidHeaderID(agentID) {
		agentID = ""
	}
	threadOf := func(session string) thread.Result {
		return h.tracker.Resolve("tenant\x00"+session, agentID, orig, thread.IsAgentCompaction(orig))
	}
	res := apply.BodyOpts(context.Background(), h.pipe, h.store, apply.Opts{
		Provider: bschemas.Anthropic, Body: body, Session: "sess-1", Tenant: "tenant", ThreadOf: threadOf,
	})
	if !res.ThreadResolved {
		res.Thread = threadOf(res.Session)
	}
	return res.Trace
}

func agentHeader(v string) http.Header {
	h := http.Header{}
	h.Set("x-claude-code-agent-id", v)
	return h
}

// TestAnEmbeddingHostGetsTheProxysThreads drives a session through a host built only from exported
// API: the main thread, a subagent named by its header, an invalid header, the agent's own
// compaction and the request after it.
func TestAnEmbeddingHostGetsTheProxysThreads(t *testing.T) {
	h := &host{tracker: thread.New(), store: store.NewMemory(store.Options{}), pipe: components.NewPipeline(nil, nil)}
	compact := "Your task is to create a detailed summary of the conversation so far."
	steps := []struct {
		name    string
		header  http.Header
		body    []byte
		wantID  string
		wantSrc thread.Source
		after   bool
	}{
		{"main", nil, hostBody("task", "a1", "u2"), thread.Primary, thread.SourcePrefix, false},
		{"subagent", agentHeader(" agentS "), hostBody("sub"), thread.AgentThread("agentS"), thread.SourceHeader, false},
		{"invalid header falls back to prefix continuity", agentHeader("bad id!"), hostBody("task", "a1", "u2", "a3", "u4"), thread.Primary, thread.SourcePrefix, false},
		{"compaction", nil, hostBody("task", "a1", "u2", "a3", "u4", "a5", compact), thread.Primary, thread.SourcePrefix, false},
		{"after compaction", nil, hostBody("summary of everything", "continue"), thread.Primary, thread.SourcePrefix, true},
	}
	for _, s := range steps {
		tr := h.serve(t, s.header, s.body)
		if tr.Thread.ID != s.wantID || tr.Thread.Source != s.wantSrc || tr.Thread.AfterCompaction != s.after {
			t.Fatalf("%s: thread = %+v, want id %q source %q after-compaction %v", s.name, tr.Thread, s.wantID, s.wantSrc, s.after)
		}
	}
}

// The agent thread id has one rule: AgentThread builds it, IsAgentThread and the keeper's
// IsSubagentThread both recognize it, and neither takes the primary thread.
func TestAgentThreadIsOneRule(t *testing.T) {
	id := thread.AgentThread("worker@team")
	if !thread.IsAgentThread(id) || !keepalive.IsSubagentThread(id) {
		t.Fatalf("%q is not recognized as an agent thread", id)
	}
	if thread.IsAgentThread(thread.Primary) || keepalive.IsSubagentThread(thread.Primary) {
		t.Fatal("the primary thread is recognized as an agent thread")
	}
	got := thread.New().Resolve("t\x00s", "worker@team", hostBody("sub"), false)
	if got.ID != id || got.Source != thread.SourceHeader {
		t.Fatalf("Resolve gave %+v, want id %q from the header", got, id)
	}
}
