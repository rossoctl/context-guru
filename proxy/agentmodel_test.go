package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/agent"
	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
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
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy returned %d", resp.StatusCode)
	}
	// summarize sends the summary call in the background, after the proxy answers, so calls read now
	// can miss it: a summary call a test wants, or an operator's key a test must not see. The call
	// is in flight from before the response until the upstream has answered it. The registry is
	// process-wide, so a summary another test leaves running also holds this wait.
	if !offload.WaitForAllSummariesForTest(10 * time.Second) {
		t.Fatal("a detached summary call was still running after 10s")
	}
	var summaries []seenCall
	for _, c := range calls() {
		if gjson.GetBytes(c.body, "max_tokens").Exists() {
			summaries = append(summaries, c)
		}
	}
	return summaries
}

// On Bob's route, the summary call goes to Bob's model path with Bob's auth scheme. Before the
// agent's ModelTarget it went to /v1/chat/completions with Bearer, which Bob does not serve, so
// every LLM component failed open and saved nothing on Bob. The model stays the request's own.
func TestBobSummaryCallUsesBobsModelPathAndApikey(t *testing.T) {
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
		t.Errorf("summary call auth is not the caller's key with Bob's Apikey scheme")
	}
}

// Every route but Bob's keeps the host default: /v1/chat/completions and Bearer.
func TestOpenAISummaryCallTargetIsUnchanged(t *testing.T) {
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
		t.Errorf("summary call auth is not the caller's key with Bearer")
	}
}

// targeted is a test agent on the OpenAI route with a fixed ModelTarget, or one that panics.
type targeted struct {
	agent.Base
	target agent.ModelTarget
	panics bool
}

func (targeted) Paths() []string { return []string{"/v1/chat/completions"} }
func (a targeted) ModelTarget() agent.ModelTarget {
	if a.panics {
		panic(panicCanary)
	}
	return a.target
}

// summaryCallWith runs one summarizable request on the OpenAI route, with a as its agent, and
// returns the one summary call the upstream got.
func summaryCallWith(t *testing.T, a agent.Agent) seenCall {
	t.Helper()
	up, calls := summaryUpstream(t)
	h := summaryHandler(t, "incoming", Options{OpenAIUpstream: up.URL})
	reg := agent.NewRegistry()
	if err := reg.Register(a); err != nil {
		t.Fatal(err)
	}
	h.agentFor = reg.ForPath
	got := postSummarizable(t, h, "/openai/v1/chat/completions", "gpt-x", "Bearer openai-caller-key", calls)
	if len(got) != 1 {
		t.Fatalf("upstream got %d summary calls, want 1; all calls: %d", len(got), len(calls()))
	}
	return got[0]
}

// A panic in the agent's ModelTarget must not fail the request or lose the summary: the call
// keeps the host default. The log names the types only, never the panic message.
func TestPanickingAgentModelTargetFailsOpenToTheHostDefault(t *testing.T) {
	logs := captureDefaultLog(t)

	got := summaryCallWith(t, targeted{panics: true})

	if got.path != "/v1/chat/completions" || got.auth != "Bearer openai-caller-key" {
		t.Errorf("summary call = %s with auth scheme %q, want the host default", got.path, authScheme(got.auth))
	}
	out := logs.String()
	if !strings.Contains(out, "recovered from panic in the agent's model target") {
		t.Fatalf("the recovery was not logged where this test reads:\n%s", out)
	}
	if strings.Contains(out, panicCanary) {
		t.Fatalf("the log holds the agent's panic message:\n%s", out)
	}
}

// A target the host cannot use safely is ignored, field by field: a path that is not absolute, or a
// scheme that is not one HTTP token (a space or a line break could forge a header).
func TestMalformedAgentModelTargetKeepsTheHostDefault(t *testing.T) {
	for name, target := range map[string]agent.ModelTarget{
		"relative path":         {Path: "inference/v1/chat/completions"},
		"absolute URL":          {Path: "//evil.example/v1/chat/completions"},
		"path with a query":     {Path: "/v1/chat/completions?x=1"},
		"path with a fragment":  {Path: "/v1/chat/completions#x"},
		"path with a space":     {Path: "/v1/chat completions"},
		"scheme with a space":   {AuthScheme: "Api key"},
		"scheme with a newline": {AuthScheme: "Apikey\r\nX-Evil: 1"},
	} {
		t.Run(name, func(t *testing.T) {
			logs := captureDefaultLog(t)

			got := summaryCallWith(t, targeted{target: target})

			field := "path"
			if target.AuthScheme != "" {
				field = "auth_scheme"
			}
			if out := logs.String(); !strings.Contains(out, "agent's model target has a field the host cannot use") ||
				!strings.Contains(out, "field="+field) {
				t.Errorf("the dropped %s was not logged:\n%s", field, out)
			}
			if got.path != "/v1/chat/completions" || got.auth != "Bearer openai-caller-key" {
				t.Errorf("summary call = %s with auth scheme %q, want the host default", got.path, authScheme(got.auth))
			}
		})
	}
}

// authScheme is the first word of an Authorization value, for a failure message: never the key.
func authScheme(v string) string {
	if f := strings.Fields(v); len(f) > 0 {
		return f[0]
	}
	return ""
}

// The operator's OpenAI key must never go to another provider's server. On Bob's route the agent
// names its own target, so a request with no key of its own makes no model call, and the
// operator's key is in no request the upstream gets.
func TestBobRouteNeverSendsTheOperatorsOpenAIKey(t *testing.T) {
	const operatorKey = "sk-operator-openai-CANARY"
	up, calls := summaryUpstream(t)
	h := summaryHandler(t, "incoming", Options{BobUpstream: up.URL, OpenAIKey: operatorKey})

	got := postSummarizable(t, h, bobModelPath, "premium-ide", "", calls)

	if len(got) != 0 {
		t.Errorf("Bob's upstream got %d summary calls without a caller key, want 0", len(got))
	}
	for _, c := range calls() {
		if strings.Contains(c.auth, operatorKey) || strings.Contains(string(c.body), operatorKey) {
			t.Fatalf("a request to Bob's upstream (%s) carries the operator's OpenAI key", c.path)
		}
	}
}

// The OpenAI route keeps its fallback to the operator's key: that key belongs to that upstream.
func TestOpenAIRouteStillFallsBackToTheOperatorsKey(t *testing.T) {
	up, calls := summaryUpstream(t)
	h := summaryHandler(t, "incoming", Options{OpenAIUpstream: up.URL, OpenAIKey: "sk-operator-openai"})

	got := postSummarizable(t, h, "/openai/v1/chat/completions", "gpt-x", "", calls)

	if len(got) != 1 || got[0].auth != "Bearer sk-operator-openai" {
		t.Fatalf("summary calls = %d, want 1 with the operator's key as Bearer", len(got))
	}
}

// The key rule must hold when the agent misbehaves, not only when its target is valid: an agent
// that names a target, even one the host drops, or whose ModelTarget panics, owns its route's auth.
// Its route never falls back to the operator's OpenAI key.
func TestAMisbehavingAgentRouteNeverSendsTheOperatorsOpenAIKey(t *testing.T) {
	const operatorKey = "sk-operator-openai-CANARY"
	for name, a := range map[string]agent.Agent{
		"panics":          targeted{panics: true},
		"every field bad": targeted{target: agent.ModelTarget{Path: "relative", AuthScheme: "Api key"}},
	} {
		t.Run(name, func(t *testing.T) {
			up, calls := summaryUpstream(t)
			h := summaryHandler(t, "incoming", Options{OpenAIUpstream: up.URL, OpenAIKey: operatorKey})
			reg := agent.NewRegistry()
			if err := reg.Register(a); err != nil {
				t.Fatal(err)
			}
			h.agentFor = reg.ForPath

			got := postSummarizable(t, h, "/openai/v1/chat/completions", "gpt-x", "", calls)

			for _, c := range got {
				if strings.Contains(c.auth, operatorKey) {
					t.Fatalf("a summary call on the agent's route carries the operator's OpenAI key")
				}
			}
			if len(got) != 0 {
				t.Errorf("summary calls = %d without a caller key, want 0", len(got))
			}
		})
	}
}
