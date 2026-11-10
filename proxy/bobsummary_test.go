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
	"github.com/tidwall/gjson"
)

// seenCall is one request a fake OpenAI-dialect upstream received.
type seenCall struct {
	path, auth string
	body       []byte
}

// summaryUpstream is a fake OpenAI-dialect upstream for both the chat call and the summary call.
// It answers every request with a usable summary, so the summarize component accepts it.
func summaryUpstream(t *testing.T) (*httptest.Server, func() []seenCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []seenCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, seenCall{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: b})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"<summary>the span, summarized</summary>"}}],` +
			`"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []seenCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]seenCall(nil), calls...)
	}
}

// summaryHandler runs summarize on the incoming model: the model and credential of the request.
func summaryHandler(t *testing.T, source string, opts Options) *Handler {
	t.Helper()
	cfg, err := config.LoadBytes([]byte(
		"pipeline: [summarize]\ncomponents:\n  summarize:\n    keep_last: 1\n" +
			"    start_from_message: 0\n    min_tokens: 1\n" +
			"    trigger: {min_request_frac: 0}\n    model:\n      source: " + source + "\n"))
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

// summarizableOpenAIRequest is summarizableRequest in the OpenAI dialect: four turns, the big
// tool output inside the span that keep_last: 1 folds.
func summarizableOpenAIRequest(model string) string {
	b, _ := json.Marshal(map[string]any{
		"model": model,
		"messages": []any{
			map[string]any{"role": "user", "content": "please fix the failing test"},
			map[string]any{"role": "assistant", "content": "let me run the tests"},
			map[string]any{"role": "tool", "tool_call_id": "tc_1", "content": bigToolOutput()},
			map[string]any{"role": "assistant", "content": "working on it"},
		},
	})
	return string(b)
}

// postSummarizable sends the request on path with the caller's bearer key, and returns the calls
// the fake upstream received that are summary calls. A summary call is the one whose body is not
// the client's chat: it carries max_tokens, which the client request does not.
func postSummarizable(t *testing.T, h *Handler, path, model, auth string, calls func() []seenCall) []seenCall {
	t.Helper()
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(summarizableOpenAIRequest(model)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy returned %d", resp.StatusCode)
	}
	var summaries []seenCall
	for _, c := range calls() {
		if gjson.GetBytes(c.body, "max_tokens").Exists() {
			summaries = append(summaries, c)
		}
	}
	return summaries
}

// On Bob's route, the summary call must go to Bob's model path. It used to go to
// /v1/chat/completions, which Bob does not serve, so every LLM component failed open and saved
// nothing on Bob. The model stays the request's own: Bob picks the real model.
func TestBobSummaryCallUsesBobsModelPath(t *testing.T) {
	up, calls := summaryUpstream(t)
	h := summaryHandler(t, "incoming", Options{BobUpstream: up.URL})

	got := postSummarizable(t, h, bobModelPath, "premium-ide", "Apikey bob-caller-key", calls)

	if len(got) != 1 {
		t.Fatalf("Bob's upstream got %d summary calls, want 1; all calls: %d", len(got), len(calls()))
	}
	if got[0].path != bobModelPath {
		t.Errorf("summary call path = %q, want %q", got[0].path, bobModelPath)
	}
	if m := gjson.GetBytes(got[0].body, "model").String(); m != "premium-ide" {
		t.Errorf("summary call model = %q, want the request's own model premium-ide", m)
	}
	if got[0].auth != "Apikey bob-caller-key" {
		t.Errorf("summary call did not carry the caller's Bob credential")
	}
}

// The guard for every route but Bob's: the plain OpenAI route's summary call still goes to
// /v1/chat/completions.
func TestOpenAISummaryCallPathIsUnchanged(t *testing.T) {
	up, calls := summaryUpstream(t)
	h := summaryHandler(t, "incoming", Options{OpenAIUpstream: up.URL})

	got := postSummarizable(t, h, "/openai/v1/chat/completions", "gpt-x", "Bearer openai-caller-key", calls)

	if len(got) != 1 {
		t.Fatalf("OpenAI upstream got %d summary calls, want 1; all calls: %d", len(got), len(calls()))
	}
	if got[0].path != "/v1/chat/completions" {
		t.Errorf("summary call path = %q, want /v1/chat/completions", got[0].path)
	}
	if got[0].auth != "Bearer openai-caller-key" {
		t.Errorf("summary call did not carry the caller's OpenAI credential")
	}
}

// P1: on Bob's route, context-guru's own token never reaches Bob, in any header or body of any
// request, the summary call included. The summary call reuses the caller's Authorization header, so
// a token there must never be that key. A token of the wrong length is the hard case: the forwarded
// request scrubs it (prefix only), but it is not a valid token, so it must not become the summary
// call's key either.
func TestBobSummaryCallNeverSendsContextGurusToken(t *testing.T) {
	valid := tenant.TokenPrefix + strings.Repeat("A", 26)
	if !tenant.LooksLikeToken(valid) {
		t.Fatalf("test token %q is not token-shaped; fix the test", valid)
	}
	damaged := valid[:len(valid)-1] // the prefix and one character short: scrubbed, not valid
	for name, hdr := range map[string]http.Header{
		"a damaged token is the only key":        {"Authorization": {"Apikey " + damaged}},
		"a token beside the Bob key":             {"Authorization": {"Apikey " + valid}, "X-Api-Key": {"bob-caller-key"}},
		"two Authorization values, token second": {"Authorization": {"Apikey bob-caller-key", "Apikey " + valid}},
	} {
		t.Run(name, func(t *testing.T) {
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
			h := summaryHandler(t, "incoming", Options{OpenAIUpstream: up.URL, BobUpstream: up.URL})
			srv := httptest.NewServer(h.Mux())
			defer srv.Close()
			req, _ := http.NewRequest(http.MethodPost, srv.URL+bobModelPath, strings.NewReader(summarizableOpenAIRequest("premium-ide")))
			req.Header = hdr.Clone()
			req.Header.Set("Content-Type", "application/json")
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			mu.Lock()
			defer mu.Unlock()
			for i, s := range seen {
				for _, tok := range []string{valid, damaged} {
					if strings.Contains(s, tok) {
						t.Errorf("request %d to Bob holds context-guru's token %q:\n%s", i, tok, s)
					}
				}
			}
		})
	}
}
