package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
)

// WHICH MODEL EACH CALL USES (#491, David). A GLM-only experiment must cost $0, so with the pipeline
// [extract_llm_sweep] and the keep-alive on, EVERY call the proxy makes upstream must carry the request's
// own model: the forwarded request, the sweep's ask (cold and warm), and the keep-alive ping.
//
// The audit of the code paths is in the PR body ("Which model each call uses"). This test is the check
// that does not rely on reading: a sweep-only proxy on a model that is NOT Claude, an upstream that
// records the `model` of every body it receives, and a failure for any model but the request's.
const glmModel = "glm-5-test"

// lastMessage is the raw JSON of the last element of the body's messages array.
func lastMessage(body []byte) string {
	msgs := gjson.GetBytes(body, "messages").Array()
	if len(msgs) == 0 {
		return ""
	}
	return msgs[len(msgs)-1].Raw
}

func TestEveryCallAfterASweepOnlyProxyOnANonClaudeModelCarriesTheRequestsModel(t *testing.T) {
	// A model that is not Claude-named is not held by the keep-alive unless it is named here (relay.go,
	// claudeModel). The live GLM run must set this, or no ping and so no sweep question happens at all.
	t.Setenv("CONTEXT_GURU_KEEPALIVE_MODELS", "glm")
	var mu sync.Mutex
	var seen []string // "<path> model=<model> purpose-ish note"
	record := func(r *http.Request, body []byte) {
		mu.Lock()
		kind := "request"
		if gjson.GetBytes(body, "messages.#").Int() > 0 {
			last := lastMessage(body)
			switch {
			case strings.Contains(last, "TOOL OUTPUTS UNDER CONSIDERATION"):
				kind = "sweep-question"
			case gjson.GetBytes(body, "max_tokens").Int() <= 8:
				kind = "ping"
			}
		}
		last := lastMessage(body)
		_ = last
		seen = append(seen, fmt.Sprintf("%s %s model=%q maxtok=%d", kind, r.URL.Path, gjson.GetBytes(body, "model").String(), gjson.GetBytes(body, "max_tokens").Int()))
		mu.Unlock()
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		record(r, body)
		w.Header().Set("Content-Type", "application/json")
		// A reply every call can use: a verdict array for the sweep's ask, harmless text for the rest,
		// with a cache read on every response so the keeper keeps the entry and pings it.
		w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"` + glmModel + `",` +
			`"content":[{"type":"text","text":"[{\"i\":0,\"needed_by\":\"none\",\"quote\":\"\",\"verdict\":\"drop\"}]"}],` +
			`"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":8,` +
			`"cache_read_input_tokens":100,"cache_creation_input_tokens":0}}`))
	}))
	defer upstream.Close()

	cfg, err := config.LoadBytes([]byte("pipeline: [extract_llm_sweep]\n" +
		"components:\n  extract_llm_sweep:\n    min_tokens: 20\n    warm: true\n    warm_every_n: 1\n" +
		"    warm_min_age_turns: 1\n    warm_min_mass_frac: 0.01\n"))
	if err != nil {
		t.Fatal(err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := dash.NewRecorder(dash.Options{
		DBPath: filepath.Join(t.TempDir(), "d.db"), BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{
		AnthropicUpstream: upstream.URL, Dashboard: rec,
		Cache: CachePolicy{KeepAlive: true, Idle: 2 * time.Second, MaxPings: 2, MaxUSDPerPing: 1, MinPrefixTokens: 0},
	})
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	const session = "model-audit-glm"
	post := func(extra int) {
		t.Helper()
		msgs := []any{map[string]any{"role": "user", "content": "fix the failing tests"}}
		for i := 0; i < 6; i++ {
			id := fmt.Sprintf("tu_%d", i)
			msgs = append(msgs,
				map[string]any{"role": "assistant", "content": []any{map[string]any{
					"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": "go test " + id}}}},
				map[string]any{"role": "user", "content": []any{map[string]any{
					"type": "tool_result", "tool_use_id": id,
					"content": strings.Repeat("plain record "+id+" of the audit log\n", 60)}}})
		}
		for i := 0; i < extra; i++ {
			msgs = append(msgs, map[string]any{"role": "assistant", "content": "ok"},
				map[string]any{"role": "user", "content": "continue"})
		}
		body, _ := json.Marshal(map[string]any{"model": glmModel, "max_tokens": 64,
			"system": []any{map[string]any{"type": "text", "text": "you are an agent",
				"cache_control": map[string]any{"type": "ephemeral"}}},
			"messages": msgs})
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-context-guru-session", session)
		req.Header.Set("x-api-key", "sk-test-key")
		req.Header.Set("anthropic-version", "2023-06-01")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("proxy returned %d", resp.StatusCode)
		}
	}
	for i := 0; i < 4; i++ {
		post(i)
	}
	// Let the real keep-alive ticker run: it pings the idle thread, and the sweep's question may stand
	// in for a ping.
	time.Sleep(6 * time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 5 {
		t.Fatalf("only %d upstream calls were seen (%v): the fixture did not exercise the proxy", len(seen), seen)
	}
	var pings, questions int
	for _, s := range seen {
		if strings.HasPrefix(s, "ping ") {
			questions += 0
		}
		if !strings.Contains(s, fmt.Sprintf("model=%q", glmModel)) {
			t.Errorf("an outgoing call used a model other than the request's %q: %s", glmModel, s)
		}
		if strings.Contains(strings.ToLower(s), "claude") {
			t.Errorf("a Claude model appeared in an outgoing call: %s", s)
		}
		if strings.HasPrefix(s, "ping ") {
			pings++
		}
		if strings.HasPrefix(s, "sweep-question ") {
			questions++
		}
	}
	// The sweep's question must have run (it is the call that reads the cached prefix with the request's
	// own model). A ping may or may not follow: the question STANDS IN for a ping by design, so the two
	// are not both required, but a run with neither proves nothing about the keep-alive path.
	if questions == 0 {
		t.Errorf("saw no sweep question (%v): the sweep's own call was not exercised", seen)
	}
	t.Logf("calls: %d forwarded-or-other, %d pings, %d sweep questions: %v", len(seen), pings, questions, seen)
}

// Without CONTEXT_GURU_KEEPALIVE_MODELS naming the model, a non-Claude model on a relay is not pinged and
// the sweep never gets a thread to ask on: the run would test nothing, and costs nothing. This pins that
// behavior so the live run's setup is not a surprise.
func TestANonClaudeModelOnARelayIsNotPingedUnlessNamed(t *testing.T) {
	t.Setenv("CONTEXT_GURU_KEEPALIVE_MODELS", "")
	var mu sync.Mutex
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],` +
			`"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2,"cache_read_input_tokens":100}}`))
	}))
	defer upstream.Close()
	cfg, _ := config.LoadBytes([]byte("pipeline: [extract_llm_sweep]\ncomponents:\n  extract_llm_sweep:\n    min_tokens: 20\n"))
	agg := metrics.NewAggregator()
	pipe, _ := cfg.Build(agg)
	rec, err := dash.NewRecorder(dash.Options{DBPath: filepath.Join(t.TempDir(), "d.db"), BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{AnthropicUpstream: upstream.URL, Dashboard: rec,
		Cache: CachePolicy{KeepAlive: true, Idle: 2 * time.Second, MaxPings: 2, MaxUSDPerPing: 1}})
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	for i := 0; i < 2; i++ {
		body, _ := json.Marshal(map[string]any{"model": glmModel, "max_tokens": 64,
			"system":   []any{map[string]any{"type": "text", "text": "you are an agent", "cache_control": map[string]any{"type": "ephemeral"}}},
			"messages": []any{map[string]any{"role": "user", "content": strings.Repeat("hello ", 50+i)}}})
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-context-guru-session", "glm-unnamed")
		req.Header.Set("x-api-key", "sk-test-key")
		req.Header.Set("anthropic-version", "2023-06-01")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	time.Sleep(5 * time.Second)
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("saw %d upstream calls, want only the 2 forwarded requests: an unnamed non-Claude model must not be pinged", calls)
	}
}
