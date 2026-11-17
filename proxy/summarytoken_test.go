package proxy

import (
	"encoding/json"
	"fmt"
	"io"
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
	"github.com/rossoctl/context-guru/tenant"
)

// Context Guru's own token never reaches an upstream, in the URL, a header or the body of any
// request, the summary call included. A token of the wrong length is the hard case: CallerKey skips
// only a token of the exact shape, so a damaged token can become the summary call's key. The
// dispatcher's scrub (setUpstreamAuth in Dispatcher.Do) must remove it.
//
// The summary call runs in a detached goroutine, so the test waits for it and fails when it does
// not come. Each case has its own session, so one case's summary in flight cannot block the next.
func TestSummaryCallNeverSendsContextGurusToken(t *testing.T) {
	valid := tenant.TokenPrefix + strings.Repeat("A", 26)
	if !tenant.LooksLikeToken(valid) {
		t.Fatalf("test token %q is not token-shaped; fix the test", valid)
	}
	damaged := valid[:len(valid)-1] // the prefix and one character short: removed, but not valid
	routes := []struct{ name, path string }{{"openai", "/openai/v1/chat/completions"}, {"bob", bobModelPath}}
	cases := []struct {
		name string
		hdr  http.Header
	}{
		{"a damaged token is the only key", http.Header{"Authorization": {"Bearer " + damaged}}},
		{"a token beside the caller's key", http.Header{"Authorization": {"Bearer " + valid}, "X-Api-Key": {"caller-key"}}},
		{"two Authorization values, token second", http.Header{"Authorization": {"Bearer caller-key", "Bearer " + valid}}},
	}
	for _, route := range routes {
		for i, tc := range cases {
			t.Run(route.name+"/"+tc.name, func(t *testing.T) {
				var mu sync.Mutex
				var seen []string
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					b, err := io.ReadAll(r.Body)
					if err != nil {
						t.Errorf("reading a request body: %v", err)
					}
					var all strings.Builder
					all.WriteString(r.URL.String() + "\n")
					for k, vs := range r.Header {
						all.WriteString(k + ": " + strings.Join(vs, ",") + "\n")
					}
					all.Write(b)
					mu.Lock()
					seen = append(seen, all.String())
					mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"<summary>s</summary>"}}],` +
						`"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
				}))
				defer up.Close()
				h := tokenCanaryHandler(t, Options{OpenAIUpstream: up.URL, BobUpstream: up.URL})
				srv := httptest.NewServer(h.Mux())
				defer srv.Close()
				req, _ := http.NewRequest(http.MethodPost, srv.URL+route.path, strings.NewReader(tokenCanaryRequest()))
				req.Header = tc.hdr.Clone()
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("x-context-guru-session", fmt.Sprintf("token-canary-%s-%d", route.name, i))

				resp, err := srv.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if !offload.WaitForAllSummariesForTest(5 * time.Second) {
					t.Fatal("the summary call never finished")
				}

				mu.Lock()
				defer mu.Unlock()
				summaries := 0
				for _, s := range seen {
					// The summary call carries max_tokens; the client's own chat does not.
					if strings.Contains(s, `"max_tokens"`) {
						summaries++
					}
				}
				if summaries == 0 {
					t.Fatalf("the upstream got %d requests and no summary call; the test proves nothing", len(seen))
				}
				for i, s := range seen {
					for _, tok := range []string{valid, damaged} {
						if strings.Contains(s, tok) {
							t.Errorf("request %d to the upstream holds context-guru's token (%d characters)", i, len(tok))
						}
					}
				}
			})
		}
	}
}

// tokenCanaryHandler runs summarize on the incoming model: the model and credential of the request.
func tokenCanaryHandler(t *testing.T, opts Options) *Handler {
	t.Helper()
	cfg, err := config.LoadBytes([]byte(
		"pipeline: [summarize]\ncomponents:\n  summarize:\n    keep_last: 1\n" +
			"    start_from_message: 0\n    min_tokens: 1\n" +
			"    trigger: {min_request_frac: 0}\n    model:\n      source: incoming\n"))
	if err != nil {
		t.Fatal(err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	h := New(pipe, store.NewMemory(store.Options{}), agg, opts)
	t.Cleanup(h.Close)
	return h
}

// tokenCanaryRequest is an OpenAI-dialect chat that summarize folds: four turns, with the big tool
// output inside the span that keep_last: 1 folds.
func tokenCanaryRequest() string {
	b, _ := json.Marshal(map[string]any{
		"model": "gpt-x",
		"messages": []any{
			map[string]any{"role": "user", "content": "please fix the failing test"},
			map[string]any{"role": "assistant", "content": "let me run the tests"},
			map[string]any{"role": "tool", "tool_call_id": "tc_1", "content": bigToolOutput()},
			map[string]any{"role": "assistant", "content": "working on it"},
		},
	})
	return string(b)
}
