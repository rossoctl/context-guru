package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
)

// On a Chat Completions route, cache_aware_summarizer with model.source: config and no static cheap
// model used to decline every turn (no_model). With the Chat prefix asker (#1030) it sends its summary
// instruction to the caller's own model, as it does on Anthropic and Responses.
func TestCacheAwareSummarizerUsesTheChatPrefixAskerWithoutAStaticModel(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := new(bytes.Buffer)
		_, _ = b.ReadFrom(r.Body)
		mu.Lock()
		bodies = append(bodies, b.Bytes())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"<summary>explored the handler, 3 tests fail.</summary>"}}],` +
			`"usage":{"prompt_tokens":100,"completion_tokens":8,"prompt_tokens_details":{"cached_tokens":90}}}`))
	}))
	defer up.Close()
	cfg, err := config.LoadBytes([]byte("pipeline: [cache_aware_summarizer]\n" +
		"components:\n  cache_aware_summarizer:\n    model:\n      source: config\n" +
		"    keep_last_turns: 1\n    min_tokens: 1\n" +
		"    trigger:\n      min_messages: 1\n      min_request_tokens: 1\n      min_request_frac: 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{OpenAIUpstream: up.URL})
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	const session = "it-chat-summarizer-prefixask"
	post := func(msgs []any) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"model": "gpt-x", "messages": msgs})
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/openai/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-context-guru-session", session)
		req.Header.Set("Authorization", "Bearer sk-test")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("proxy returned %d", resp.StatusCode)
		}
	}
	text := strings.Repeat("ran pytest, 3 failures in src/mod/file.py\n", 40)
	msgs := []any{
		map[string]any{"role": "user", "content": "TASK: fix the failing handler"},
		map[string]any{"role": "assistant", "content": "working on it"},
		map[string]any{"role": "user", "content": text},
		map[string]any{"role": "assistant", "content": "checking"},
		map[string]any{"role": "user", "content": "keep going"},
	}
	before := offload.CacheAwareSummarizerPrefixAskUsed()
	post(msgs)
	post(append(msgs, map[string]any{"role": "assistant", "content": "more"}, map[string]any{"role": "user", "content": "go on"}))
	if !offload.WaitForSummaryForTest(session, 5*time.Second) {
		t.Fatal("no summary was commissioned: the summarizer declined on a Chat route")
	}
	if offload.CacheAwareSummarizerPrefixAskUsed() <= before {
		t.Fatal("the summary did not go through the prefix asker")
	}
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, b := range bodies {
		if strings.Contains(string(b), "OPERATOR INSTRUCTION") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no summary instruction reached the caller's model (%d upstream bodies)", len(bodies))
	}
}
