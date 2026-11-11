package proxy

import (
	"bytes"
	"encoding/json"
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
	"gopkg.in/yaml.v3"
)

// PR #414's review of this follow-up: the stale-prefix guard compares
// cache_aware_summarizer's OWN view of the span (req.Input as the PIPELINE received it, before
// any component ran) against proxy's sentStash — which, before this fix, held only the FORWARDED
// body, i.e. req.Input AFTER every component in the pipeline had its turn. Those two differ
// whenever a component that runs AFTER cache_aware_summarizer rewrites a message — xhigh runs
// textclean right after it, and textclean strips terminal noise (ANSI escapes) from tool output.
// Live xhigh measured 5 of 5 commissions declining as stale_prefix, 0 ever committed. Fixed by
// stashing the PRE-pipeline body alongside the forwarded one (proxy/prefixask.go's sentEntry) and
// checking coverage against that instead.
//
// Both tests below build a tool-result message containing an ANSI color code, long enough to
// clear textclean's own floor (min_tokens: 50, textclean.go), so textclean actually rewrites it
// on every turn it is forwarded on — which is what makes `forwarded` and the pipeline's own
// pre-mutation view diverge. The CLIENT always resends its own original (ANSI-bearing) history,
// never the proxy's internally cleaned copy, so cache_aware_summarizer's own span hash is stable
// turn to turn; only a `forwarded`-based coverage check sees it move.
func ansiToolOutput() string {
	return "\x1b[31mERROR\x1b[0m " + strings.Repeat("filler word ", 60)
}

// pipelineOrderFixture runs the same three-turn shape as the other cache_aware integration tests
// (3, 5, 7 messages; keep_last_turns: 2 makes turn 3's span end at index 5, exactly turn 2's own
// message count) through a caller-supplied pipeline config, and reports how many ask calls the
// mock upstream actually saw.
func pipelineOrderFixture(t *testing.T, configYAML string) (asked []string) {
	t.Helper()
	var mu sync.Mutex
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"aws/claude-sonnet-5",` +
			`"content":[{"type":"text","text":"<summary>covers turn 1 and 2.</summary>"}],` +
			`"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":8,` +
			`"cache_read_input_tokens":10,"cache_creation_input_tokens":0}}`))
	}))
	defer upstream.Close()

	cfg, err := config.LoadBytes([]byte(configYAML))
	if err != nil {
		t.Fatalf("config.LoadBytes: %v", err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatalf("cfg.Build: %v", err)
	}
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{AnthropicUpstream: upstream.URL})
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	session := "it-pipeline-order-" + strings.ReplaceAll(t.Name(), "/", "-")

	post := func(messages []any) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{
			"model": "aws/claude-sonnet-5", "max_tokens": 64,
			"system":   "you are claude code",
			"messages": messages,
		})
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
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

	toolResult := map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": "tu_1", "content": ansiToolOutput()},
	}}

	post([]any{
		map[string]any{"role": "user", "content": "MARK-T1: fix the failing tests"},
		map[string]any{"role": "assistant", "content": "working on it"},
		toolResult,
	})
	post([]any{
		map[string]any{"role": "user", "content": "MARK-T1: fix the failing tests"},
		map[string]any{"role": "assistant", "content": "working on it"},
		toolResult,
		map[string]any{"role": "assistant", "content": "ran them again"},
		map[string]any{"role": "user", "content": "MARK-T2: making progress"},
	})
	post([]any{
		map[string]any{"role": "user", "content": "MARK-T1: fix the failing tests"},
		map[string]any{"role": "assistant", "content": "working on it"},
		toolResult,
		map[string]any{"role": "assistant", "content": "ran them again"},
		map[string]any{"role": "user", "content": "MARK-T2: making progress"},
		map[string]any{"role": "assistant", "content": "trying a different approach"},
		map[string]any{"role": "user", "content": "any luck"},
	})

	if !offload.WaitForSummaryForTest(session, 5*time.Second) {
		t.Fatal("turn 3's commissioned (or declined) summary never resolved")
	}

	mu.Lock()
	defer mu.Unlock()
	for _, b := range bodies {
		if strings.Contains(b, "OPERATOR INSTRUCTION") || strings.Contains(b, "Output ONLY the summary") {
			asked = append(asked, b)
		}
	}
	return asked
}

// The minimal reproduction: cache_aware_summarizer followed by exactly the one component whose
// rewrite exposes the bug.
// presetWithLowTrigger loads `preset` and returns a config that keeps the preset's OWN
// cache_aware_summarizer block (model.source, cache_state, …) and only lowers its trigger so a
// three-turn fixture reaches it. A plain `components:` override would replace the preset's block
// as a whole (applyPreset: merged[k] = v), so the test would check its own settings, not the
// preset's (PR #456 review).
func presetWithLowTrigger(t *testing.T, preset string) string {
	t.Helper()
	cfg, err := config.LoadBytes([]byte("preset: " + preset + "\n"))
	if err != nil {
		t.Fatalf("load preset %s: %v", preset, err)
	}
	node, ok := cfg.Components["cache_aware_summarizer"]
	if !ok {
		t.Fatalf("preset %s carries no cache_aware_summarizer block", preset)
	}
	block := map[string]any{}
	if err := node.Decode(&block); err != nil {
		t.Fatal(err)
	}
	block["keep_last_turns"] = 2
	block["min_tokens"] = 1
	trig, _ := block["trigger"].(map[string]any)
	if trig == nil {
		trig = map[string]any{}
	}
	trig["min_messages"] = 7
	trig["min_request_tokens"] = 1
	trig["min_request_frac"] = 0
	block["trigger"] = trig
	out, err := yaml.Marshal(map[string]any{"preset": preset,
		"components": map[string]any{"cache_aware_summarizer": block}})
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestCacheAwarePipelineOrderMinimalTextCleanCase(t *testing.T) {
	asked := pipelineOrderFixture(t, "pipeline: [cache_aware_summarizer, textclean]\n"+
		"components:\n  cache_aware_summarizer:\n    keep_last_turns: 2\n    min_tokens: 1\n"+
		"    trigger:\n      min_messages: 7\n      min_request_tokens: 1\n      min_request_frac: 0\n")
	if len(asked) != 1 {
		t.Fatalf("got %d ask calls, want exactly 1 (turn 3 should have committed a summary despite "+
			"textclean rewriting the ANSI tool output on every turn it was forwarded on)", len(asked))
	}
	if !strings.Contains(asked[0], "MARK-T2") {
		t.Error("the ask call's own prefix did not cover turn 2 — the fixture itself is broken")
	}
}

// The shipped `xhigh` preset, loaded by name (PR #414 switched it to cache_aware_summarizer, first
// in the pipeline). Only the summarizer's trigger is lowered so a three-turn fixture reaches it; the
// pipeline, and the summarizer's own settings other than its trigger, come from the preset. If the stale-prefix
// guard's basis regresses to the forwarded body, this reproduces the live #414 finding: 0
// commissions, every one declining as stale_prefix.
func TestXHighPresetCommitsACacheAwareSummary(t *testing.T) {
	asked := pipelineOrderFixture(t, presetWithLowTrigger(t, "xhigh"))
	if len(asked) != 1 {
		t.Fatalf("got %d ask calls, want exactly 1 (the xhigh preset should have "+
			"committed a summary on turn 3)", len(asked))
	}
	if !strings.Contains(asked[0], "MARK-T2") {
		t.Error("the ask call's own prefix did not cover turn 2 — the fixture itself is broken")
	}
}

// `high` runs the same summarizer as xhigh at cache_state `any` (#417), so it must commit a
// summary on the same fixture. `medium` pins cache_state: pre_expiry, so on these warm turns it
// must NOT ask (it waits for the pre-expiry window); its reserve path is covered by the
// pre_expiry tests in components/offload and keepalive/substitute_test.go.
func TestHighPresetCommitsACacheAwareSummary(t *testing.T) {
	asked := pipelineOrderFixture(t, presetWithLowTrigger(t, "high"))
	if len(asked) != 1 {
		t.Fatalf("got %d ask calls, want exactly 1 (the high preset should have committed a summary on turn 3)", len(asked))
	}
	if !strings.Contains(asked[0], "MARK-T2") {
		t.Error("the ask call's own prefix did not cover turn 2")
	}
}

func TestMediumPresetDoesNotSummarizeOnAWarmTurn(t *testing.T) {
	before := offload.CacheAwareSummarizerPrefixAskUsed()
	asked := pipelineOrderFixture(t, presetWithLowTrigger(t, "medium"))
	if len(asked) != 0 {
		t.Fatalf("got %d ask calls, want 0: medium (pre_expiry) must not summarize on a warm turn", len(asked))
	}
	// Positive half: the summarizer DID run and reach its model step, then stopped at the
	// cache_state gate. Without this, a medium with no summarizer at all would also pass.
	if offload.CacheAwareSummarizerPrefixAskUsed() == before {
		t.Fatal("cache_aware_summarizer never reached its model step — medium is not running it, " +
			"so 0 ask calls proves nothing")
	}
}
