package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
	"github.com/rossoctl/context-guru/tenant"
)

// Context Guru's own token never reaches an upstream, in any header or body of any request, the
// summary call included. A token of the wrong length is the hard case: the forwarded request
// removes it (by its prefix), but CallerKey skips only a token of the exact shape, so a damaged
// token could become the summary call's key.
func TestSummaryCallNeverSendsContextGurusToken(t *testing.T) {
	valid := tenant.TokenPrefix + strings.Repeat("A", 26)
	if !tenant.LooksLikeToken(valid) {
		t.Fatalf("test token %q is not token-shaped; fix the test", valid)
	}
	damaged := valid[:len(valid)-1] // the prefix and one character short: removed, but not valid
	routes := map[string]string{"openai": "/openai/v1/chat/completions", "bob": bobModelPath}
	for rname, route := range routes {
		for name, hdr := range map[string]http.Header{
			"a damaged token is the only key":        {"Authorization": {"Bearer " + damaged}},
			"a token beside the caller's key":        {"Authorization": {"Bearer " + valid}, "X-Api-Key": {"caller-key"}},
			"two Authorization values, token second": {"Authorization": {"Bearer caller-key", "Bearer " + valid}},
		} {
			t.Run(rname+"/"+name, func(t *testing.T) {
				var mu sync.Mutex
				var seen []string
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					b, _ := io.ReadAll(r.Body)
					var all strings.Builder
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
				req, _ := http.NewRequest(http.MethodPost, srv.URL+route, strings.NewReader(tokenCanaryRequest()))
				req.Header = hdr.Clone()
				req.Header.Set("Content-Type", "application/json")

				resp, err := srv.Client().Do(req)

				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				mu.Lock()
				defer mu.Unlock()
				if len(seen) == 0 {
					t.Fatal("the upstream got no request; the test proves nothing")
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
