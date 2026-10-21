package proxy

import (
	"bytes"
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
)

// varyingFiller returns n bytes of varied text, roughly n/8 words of the form "word1234 " —
// NOT strings.Repeat of one byte. A long run of one repeated byte is a pathological input for
// BPE merge (github.com/tiktoken-go/tokenizer, which apply.attemptedTokens runs on every
// request): the merge loop over a single giant run of identical pairs blew well past this
// test's budget on a 1.6 MB all-'x' payload. Varied short "words" tokenize the way real text
// does and cost nothing unusual.
func varyingFiller(n int) string {
	var b strings.Builder
	b.Grow(n + 16)
	for b.Len() < n {
		fmt.Fprintf(&b, "word%d ", b.Len())
	}
	return b.String()[:n]
}

// PR #402 review, finding 1 (High): the PrefixAsk path answers from WHATEVER body proxy's
// sentStash holds for a session, and a body over its 1.5 MB cap used to be skipped while the
// OLDER body stayed in place — so the next commission silently answered from a transcript that
// did not cover the span it was about to claim as summarized. The checkpoint it wrote still
// recorded the CURRENT (correct) CoveredCount/CoveredHash, because those come from the real
// transcript the component itself holds — only the SUMMARY TEXT was wrong, built from a stale
// read nothing else could see. The fix: sentStash.put now DELETES a session's entry rather than
// keeping an old one when it skips an oversized body, so the next ask gets ErrNoPrefix (routine,
// "nothing to read yet") instead of a wrong answer dressed as a right one; and the stale-prefix
// guard (components.PrefixCoverage, checked at call time in cache_aware_summarizer.go) catches
// the other way a stash can undercover a span, for a client that does not go through the
// delete-on-skip path at all.
//
// Three real HTTP requests per session, run through the actual Mux()/serve() path (the reviewer
// confirmed the bug this way, not through a unit test), mirroring the reviewer's own probe:
//
//	turn 1 (3 messages): below trigger.min_messages (7) — declines at the sized gate, forwarded
//	                      unchanged, and stashed as-is (3 messages).
//	turn 2 (5 messages): adds the test's own payload as its last message — 10 KB (control) or
//	                     1.6 MB (over sentStash's cap). Still below min_messages: declines, is
//	                     forwarded unchanged, and h.sent.put sees this turn's own (possibly
//	                     oversized) body.
//	turn 3 (7 messages): reaches min_messages. keep_last_turns: 2 makes the span this turn would
//	                     summarize end exactly at index 5 — i.e. exactly turn 2's own message
//	                     count, so a correctly-stashed turn 2 covers it exactly.
//
// Every request (the three real turns AND, when one is made, the PrefixAsk completion) lands on
// the same mock upstream, so the ask call (if any) is told apart from a real turn by its own
// instruction text.
func TestCacheAwareStalePrefixThroughARealOversizedTurn(t *testing.T) {
	run := func(t *testing.T, turn2Payload string) (asked []string, declinedNoPrefix, declinedStale int64) {
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

		// No "/": t.Name() includes the subtest path ("Test.../control_10KB"), and session.
		// safeExplicit rejects that character — a rejected explicit id silently falls back to
		// the content hash, which is a DIFFERENT key than the literal string this test would
		// then poll with offload.WaitForSummaryForTest.
		session := "it-stale-prefix-" + strings.ReplaceAll(t.Name(), "/", "-")
		post := func(messages []any) {
			t.Helper()
			body, _ := json.Marshal(map[string]any{
				"model": "aws/claude-sonnet-5", "max_tokens": 64,
				"system": []any{map[string]any{"type": "text", "text": "you are claude code",
					"cache_control": map[string]any{"type": "ephemeral"}}},
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

		t1 := []any{
			map[string]any{"role": "user", "content": "MARK-T1: fix the failing tests"},
			map[string]any{"role": "assistant", "content": "working on it"},
			map[string]any{"role": "user", "content": "keep going"},
		}
		post(t1)

		t2 := append(append([]any{}, t1...),
			map[string]any{"role": "assistant", "content": "ran them again"},
			map[string]any{"role": "user", "content": "MARK-T2: " + turn2Payload})
		post(t2)

		t3 := append(append([]any{}, t2...),
			map[string]any{"role": "assistant", "content": "trying a different approach"},
			map[string]any{"role": "user", "content": "any luck"})
		post(t3)

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
		return asked, offload.CacheAwareSummarizerNoPrefix(), offload.CacheAwareSummarizerStalePrefix()
	}

	t.Run("control_10KB", func(t *testing.T) {
		before := offload.CacheAwareSummarizerNoPrefix()
		asked, noPrefix, _ := run(t, varyingFiller(10_000))
		if len(asked) != 1 {
			t.Fatalf("got %d ask calls, want exactly 1 (turn 3 should have commissioned one)", len(asked))
		}
		if !strings.Contains(asked[0], "MARK-T2") {
			t.Error("the ask call's own prefix did not cover turn 2 — the control itself is broken")
		}
		if noPrefix != before {
			t.Errorf("a 10 KB turn 2 was wrongly declined as having no prefix (count went from %d to %d)",
				before, noPrefix)
		}
	})

	t.Run("oversized_1.6MB", func(t *testing.T) {
		noPrefixBefore := offload.CacheAwareSummarizerNoPrefix()
		asked, noPrefix, _ := run(t, varyingFiller(1_600_000))
		if len(asked) != 0 {
			t.Fatalf("got %d ask calls; an oversized turn 2 must decline BEFORE paying for a call "+
				"that could only answer from turn 1 while claiming to cover turn 2 too: %q",
				len(asked), asked)
		}
		if noPrefix != noPrefixBefore+1 {
			t.Errorf("CacheAwareSummarizerNoPrefix did not increment (%d -> %d) — the oversized "+
				"turn should leave NOTHING stashed (sentStash.put deletes rather than keeps an "+
				"old body), which is what makes Ask's decline the routine kind rather than a "+
				"silent wrong answer", noPrefixBefore, noPrefix)
		}
	})
}
