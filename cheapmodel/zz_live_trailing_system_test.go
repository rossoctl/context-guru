package cheapmodel

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/dispatch/dispatchtest"
)

// liveUsage is the subset of a Messages API response's usage block these tests read.
type liveUsage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheCreationTok int `json:"cache_creation_input_tokens"`
	CacheReadTok     int `json:"cache_read_input_tokens"`
}

// postRawRecordUsage sends body EXACTLY as given -- no ask appended, no insertAskMessage, no
// max_tokens rewrite -- the way the MAIN AGENT itself sends it. This is deliberately not
// CompletePrefixed.
//
// Review on an earlier version of this test file found that both calls in it went through
// CompletePrefixed, so the "cache write" the first call made WAS ALREADY the bug under test: the
// second call then read that same, already-wrong entry back, rather than the real entry a main
// agent's own ordinary call would have written. A passing test proved nothing about the bug it
// was meant to catch. This helper is the fix: it is a plain POST, so the entry it writes is the
// one CompletePrefixed is actually supposed to find and read in production.
func postRawRecordUsage(t *testing.T, base, key string, body []byte) liveUsage {
	t.Helper()
	if base == "" {
		base = "https://api.anthropic.com"
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(base, "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	// The live tests calling this all pin a "bearer" cli (see TestLiveCompletePrefixedWith...),
	// matching the credential the prefix ask over the same cli presents.
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("the main agent's own call: status %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Usage liveUsage `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Usage
}

// trailingSystemBody is the shape under test: a stored request whose last message is a
// mid-conversation role:"system" reminder carrying the cache_control breakpoint, the way Claude
// Code sends on effectively every turn (docs/components/caching.md's measurement).
func trailingSystemBody(model string, maxTokens int, thinking map[string]any) []byte {
	convo := strings.Repeat(
		"the handler in src/mod/file.py returns 500 when the payload is missing a trailing newline; "+
			"pytest tests/test_handler.py reports three failures in dispatch\n", 400)
	body := map[string]any{
		"model":      model,
		"max_tokens": maxTokens,
		"messages": []any{
			map[string]any{"role": "user", "content": convo},
			map[string]any{"role": "assistant", "content": "Noted; investigating."},
			map[string]any{"role": "user", "content": "continue"},
			map[string]any{"role": "system", "content": []any{
				map[string]any{"type": "text", "text": "<system-reminder>be careful</system-reminder>",
					"cache_control": map[string]any{"type": "ephemeral"}},
			}},
		},
	}
	if thinking != nil {
		body["thinking"] = thinking
	}
	raw, _ := json.Marshal(body)
	return raw
}

// Live proof, on the PLAIN gateway (never Context Guru), that a prefix ask over a request whose
// own last message is a role:"system" reminder reads the MAIN AGENT's own cache entry rather than
// 400ing (the original defect) or silently turning the read into a write (the cache-loss bug a
// prior version of this fix had — see insertAskMessage's doc comment). Gated like the other live
// tests in this package (CG_LIVE=1 + CG_BASE/CG_TOKEN/CG_MODEL): skipped by default, so the
// ordinary unit suite never needs network access or an API key.
//
// MUST FAIL against the insert-before-the-system-message version of this fix (commit 0e51464):
// that version measured read=0, write=21,652 on this exact shape, because inserting the ask
// before the breakpoint-bearing system message changes the bytes the provider hashes up to that
// breakpoint. It passes against the append-after version: measured read=21,644, write=0.
func TestLiveCompletePrefixedWithTrailingSystemMessage(t *testing.T) {
	if os.Getenv("CG_LIVE") == "" {
		t.Skip("set CG_LIVE=1 CG_BASE=... CG_TOKEN=... CG_MODEL=... to run")
	}
	model := os.Getenv("CG_MODEL")
	if model == "" {
		model = "claude-sonnet-5[1m]"
	}
	base, key := os.Getenv("CG_BASE"), os.Getenv("CG_TOKEN")
	d, ref := dispatchtest.HTTP(base, dispatchtest.BearerAuth(key))
	cli := Anthropic{D: d, Ref: ref, Model: model}
	raw := trailingSystemBody(model, 64, nil)

	// THE MAIN AGENT'S OWN CALL. Establishes the entry CompletePrefixed is supposed to find and
	// read -- a plain POST, nothing this method adds.
	mainUsage := postRawRecordUsage(t, base, key, raw)
	t.Logf("main agent's own call: cache_read=%d cache_write=%d fresh=%d",
		mainUsage.CacheReadTok, mainUsage.CacheCreationTok, mainUsage.InputTokens)
	mainEntryTokens := mainUsage.CacheCreationTok + mainUsage.CacheReadTok
	if mainEntryTokens == 0 {
		t.Fatalf("the main agent's own call wrote or read nothing cacheable; this test proves " +
			"nothing without a real entry for the prefix ask to read")
	}

	// THE PREFIX ASK, over the IDENTICAL raw body. The live assertion that matters: cache_read
	// here must land near mainEntryTokens -- the entry the call ABOVE wrote -- not be zero
	// (the cache-loss bug) and not be explained away by this test's own first call, which is
	// exactly what review found wrong with the previous version of this test.
	_, usage, err := cli.CompletePrefixed(context.Background(), raw, "what does the trailing reminder say?")
	if err != nil {
		t.Fatalf("prefix ask over a trailing-system prefix: %v", err)
	}
	t.Logf("prefix ask: cache_read=%d cache_write=%d fresh=%d output=%d",
		usage.CacheRead, usage.CacheWrite, usage.Fresh, usage.Output)
	if usage.CacheWrite > 0 {
		t.Errorf("the prefix ask WROTE %d tokens instead of reading the main agent's entry -- "+
			"this is the cache-loss bug under review, not the original 400", usage.CacheWrite)
	}
	// Approximate, not exact: provider-side token accounting rounds to internal block
	// boundaries, and the ask itself adds a few fresh tokens on top of the cached entry.
	const tolerance = 0.05
	lo := int(float64(mainEntryTokens) * (1 - tolerance))
	if usage.CacheRead < lo {
		t.Errorf("cache_read=%d, want >= %d (~%d%% of the main agent's own %d-token entry): the "+
			"prefix ask is not reading the entry the main agent wrote",
			usage.CacheRead, lo, int((1-tolerance)*100), mainEntryTokens)
	}
}

// Live proof, SAME SHAPE, with extended (manual) thinking enabled -- the mode Claude Code uses
// per PR #406. This is the open question in insertAskMessage's doc comment: Anthropic's own docs
// say "the final assistant turn of a thinking-enabled request" must begin with a thinking block
// in this mode, and the synthetic assistant turn this fix appends carries none. Whether "final
// assistant turn" means literally the last assistant message regardless of what follows it (this
// one), or only one an active tool-use loop is continuing through (this one is not -- it's
// followed by a fresh user message), is NOT settled by reading the docs alone. This test is the
// live settlement: if it 400s with a thinking-related message, the open question resolves against
// this fix for thinking.type: "enabled" sessions and a different approach is needed there; if it
// succeeds, the question resolves in this fix's favor.
//
// Defaults to Sonnet, NOT Haiku, despite #406's own Haiku example: Haiku 4.5 rejects any
// role:"system" message outright ("role 'system' is not supported on this model"), so no Haiku
// traffic can carry the trailing-system shape this test exists to exercise, with or without
// thinking. The two concerns are independent -- which model shape triggers this bug, and which
// model Claude Code runs extended thinking on -- and Sonnet happens to answer both here.
func TestLiveCompletePrefixedWithTrailingSystemMessageAndThinkingEnabled(t *testing.T) {
	if os.Getenv("CG_LIVE") == "" {
		t.Skip("set CG_LIVE=1 CG_BASE=... CG_TOKEN=... CG_MODEL=... to run")
	}
	model := os.Getenv("CG_MODEL")
	if model == "" {
		model = "claude-sonnet-5[1m]"
	}
	base, key := os.Getenv("CG_BASE"), os.Getenv("CG_TOKEN")
	d, ref := dispatchtest.HTTP(base, dispatchtest.BearerAuth(key))
	cli := Anthropic{D: d, Ref: ref, Model: model}
	// budget_tokens well under max_tokens, satisfying the Messages API's own minimum (1,024) and
	// its "less than max_tokens" rule on the SEED call below.
	raw := trailingSystemBody(model, 4096, map[string]any{"type": "enabled", "budget_tokens": 1024})

	mainUsage := postRawRecordUsage(t, base, key, raw)
	t.Logf("main agent's own call (thinking enabled): cache_read=%d cache_write=%d fresh=%d",
		mainUsage.CacheReadTok, mainUsage.CacheCreationTok, mainUsage.InputTokens)
	if mainUsage.CacheCreationTok+mainUsage.CacheReadTok == 0 {
		t.Fatalf("the main agent's own call wrote or read nothing cacheable")
	}

	_, usage, err := cli.CompletePrefixed(context.Background(), raw, "what does the trailing reminder say?")
	if err != nil {
		t.Fatalf("prefix ask over a trailing-system, thinking-enabled prefix: %v -- if this names "+
			"the thinking block or 'final assistant turn', the open question in "+
			"insertAskMessage's doc comment resolves AGAINST this fix for thinking.type: "+
			"\"enabled\" sessions", err)
	}
	t.Logf("prefix ask (thinking enabled): cache_read=%d cache_write=%d fresh=%d output=%d",
		usage.CacheRead, usage.CacheWrite, usage.Fresh, usage.Output)
	if usage.CacheWrite > 0 {
		t.Errorf("the prefix ask wrote %d tokens instead of reading the main agent's entry", usage.CacheWrite)
	}
}
