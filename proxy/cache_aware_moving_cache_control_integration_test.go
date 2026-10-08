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
)

// PR #402 re-review, Note (Low), now fixed rather than merely documented: components.SpanHash
// used to hash a message's `cache_control` block along with its content. Claude Code (and every
// client that moves a single ephemeral breakpoint forward to keep the newest message cacheable)
// re-sends an EARLIER turn's last message with the marker gone — the content is byte-identical,
// only the breakpoint moved on to a later message. At keep_last_turns: 2 or less the span this
// component is about to summarize (msgs[:end]) reaches exactly that message, so the hash of
// msgs[:end] (now unmarked) stopped matching the hash of what proxy's sentStash actually holds
// for the PREVIOUS turn (marked, because it was that turn's last message) — a byte difference
// that carries no content, yet made cache_aware_summarizer decline EVERY commission as
// stale_prefix. Fixed by stripping cache_control before SpanHash (components/component.go).
//
// Three real HTTP requests per session through the actual Mux()/serve() path, matching the
// reviewer's own probe in cache_aware_stale_prefix_integration_test.go: turn 1 (3 messages, below
// trigger.min_messages so it only primes the stash), turn 2 (5 messages), turn 3 (7 messages,
// reaching min_messages). keep_last_turns: 2 makes turn 3's span end at exactly index 5 — turn
// 2's own message count — so a correctly-stashed turn 2 covers it exactly, EXCEPT that turn 2's
// stashed copy of message index 4 carries the cache_control breakpoint and turn 3's own copy of
// that same message does not, because by turn 3 the marker has moved on to message index 6.
func TestCacheAwareClaudeCodeStyleMovingCacheControlIsNotStale(t *testing.T) {
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

	cfg, err := config.LoadBytes([]byte("pipeline: [cache_aware_summarizer]\n" +
		"components:\n  cache_aware_summarizer:\n    keep_last_turns: 2\n    min_tokens: 1\n" +
		"    trigger:\n      min_messages: 7\n      min_request_tokens: 1\n      min_request_frac: 0\n"))
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

	session := "it-moving-cache-control-" + strings.ReplaceAll(t.Name(), "/", "-")

	// textBlock renders one message with its text as a single content block, marked with an
	// ephemeral cache_control breakpoint when marked is true — the shape Claude Code actually
	// sends (a block-level directive; a plain string content cannot carry one at all).
	textBlock := func(role, text string, marked bool) any {
		block := map[string]any{"type": "text", "text": text}
		if marked {
			block["cache_control"] = map[string]any{"type": "ephemeral"}
		}
		return map[string]any{"role": role, "content": []any{block}}
	}

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

	// turn 1: the marker sits on the last (3rd) message.
	post([]any{
		textBlock("user", "MARK-T1: fix the failing tests", false),
		textBlock("assistant", "working on it", false),
		textBlock("user", "keep going", true),
	})

	// turn 2: turn 1's marker is GONE (moved forward, Claude-Code style) and the new last
	// message (5th) carries it instead.
	post([]any{
		textBlock("user", "MARK-T1: fix the failing tests", false),
		textBlock("assistant", "working on it", false),
		textBlock("user", "keep going", false),
		textBlock("assistant", "ran them again", false),
		textBlock("user", "MARK-T2: making progress", true),
	})

	// turn 3: turn 2's marker is gone too, and the new last (7th) message carries it. The span
	// this turn would summarize (msgs[:5]) is byte-identical in CONTENT to what turn 2 forwarded
	// and proxy's sentStash holds — only the breakpoint's position differs.
	post([]any{
		textBlock("user", "MARK-T1: fix the failing tests", false),
		textBlock("assistant", "working on it", false),
		textBlock("user", "keep going", false),
		textBlock("assistant", "ran them again", false),
		textBlock("user", "MARK-T2: making progress", false),
		textBlock("assistant", "trying a different approach", false),
		textBlock("user", "any luck", true),
	})

	if !offload.WaitForSummaryForTest(session, 5*time.Second) {
		t.Fatal("turn 3's commissioned (or declined) summary never resolved")
	}

	mu.Lock()
	defer mu.Unlock()
	var asked []string
	for _, b := range bodies {
		if strings.Contains(b, "OPERATOR INSTRUCTION") || strings.Contains(b, "Output ONLY the summary") {
			asked = append(asked, b)
		}
	}
	if len(asked) != 1 {
		t.Fatalf("got %d ask calls, want exactly 1 (turn 3 should have commissioned one despite "+
			"the moving cache_control marker); gates may show stale_prefix instead", len(asked))
	}
	if !strings.Contains(asked[0], "MARK-T2") {
		t.Error("the ask call's own prefix did not cover turn 2 — the fixture itself is broken")
	}
}

// The other half of the same fix: stripping cache_control before SpanHash must not turn the hash
// blind to an actual content change. Same three-turn, moving-marker shape as the test above, but
// turn 3's copy of the message the stash is supposed to cover (index 4) has DIFFERENT text —
// standing in for whatever real bug would make the host's stash and the live transcript actually
// disagree. That must still decline as stale_prefix, marker-stripping or not.
func TestCacheAwareRealContentChangeInsideSpanStillDeclines(t *testing.T) {
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

	cfg, err := config.LoadBytes([]byte("pipeline: [cache_aware_summarizer]\n" +
		"components:\n  cache_aware_summarizer:\n    keep_last_turns: 2\n    min_tokens: 1\n" +
		"    trigger:\n      min_messages: 7\n      min_request_tokens: 1\n      min_request_frac: 0\n"))
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

	session := "it-real-content-change-" + strings.ReplaceAll(t.Name(), "/", "-")
	before := offload.CacheAwareSummarizerStalePrefix()

	textBlock := func(role, text string, marked bool) any {
		block := map[string]any{"type": "text", "text": text}
		if marked {
			block["cache_control"] = map[string]any{"type": "ephemeral"}
		}
		return map[string]any{"role": role, "content": []any{block}}
	}

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

	post([]any{
		textBlock("user", "MARK-T1: fix the failing tests", false),
		textBlock("assistant", "working on it", false),
		textBlock("user", "keep going", true),
	})

	post([]any{
		textBlock("user", "MARK-T1: fix the failing tests", false),
		textBlock("assistant", "working on it", false),
		textBlock("user", "keep going", false),
		textBlock("assistant", "ran them again", false),
		textBlock("user", "MARK-T2: making progress", true),
	})

	// Index 4's text here is NOT "MARK-T2: making progress" — a real byte difference inside the
	// span cache_aware_summarizer is about to claim it covers, standing in for a stash gone wrong
	// by some means other than the marker it moved.
	post([]any{
		textBlock("user", "MARK-T1: fix the failing tests", false),
		textBlock("assistant", "working on it", false),
		textBlock("user", "keep going", false),
		textBlock("assistant", "ran them again", false),
		textBlock("user", "MARK-T2: EDITED, not what turn 2 actually sent", false),
		textBlock("assistant", "trying a different approach", false),
		textBlock("user", "any luck", true),
	})

	if !offload.WaitForSummaryForTest(session, 5*time.Second) {
		t.Fatal("turn 3's commissioned (or declined) summary never resolved")
	}

	mu.Lock()
	defer mu.Unlock()
	var asked []string
	for _, b := range bodies {
		if strings.Contains(b, "OPERATOR INSTRUCTION") || strings.Contains(b, "Output ONLY the summary") {
			asked = append(asked, b)
		}
	}
	if len(asked) != 0 {
		t.Fatalf("got %d ask calls; a real content change inside the span must decline BEFORE "+
			"paying for a call that would answer from the wrong transcript: %q", len(asked), asked)
	}
	if got := offload.CacheAwareSummarizerStalePrefix(); got != before+1 {
		t.Errorf("CacheAwareSummarizerStalePrefix did not increment (%d -> %d) for a real content "+
			"change inside the span", before, got)
	}
}
