package tokens_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestCountTokensIsNotABillingOracle exists to stop the next person measuring the tokenizer gap
// the way this one first did.
//
// /v1/messages/count_tokens looks like the obvious instrument for "what would the provider count
// for this body". On the IBM LiteLLM gateway it is unreliable in a SIZE-DEPENDENT way, which is
// worse than being plainly wrong, because a small-body sanity check passes.
//
// Measured 2026-09-15. On a small body it is exactly additive:
//
//	messages 1,024 + tools 4,677 + system 902 = 6,603  (full body)
//
// On a real 340 KB Claude Code body it is not. Every variant returns the MESSAGES count alone:
//
//	full = no-tools = no-system = messages-only = 51,569
//	the same tools+system, sent with a one-character message = 35,221
//
// So 35,221 tokens are silently dropped at the sizes compaction actually happens at, and the
// figure lands within a percent of our own o200k count — because what survives is a tiktoken
// count over `messages`. Used as an oracle it compares our estimator against a copy of itself:
// a first pass at #240 got f = 0.9961 that way and would have "refuted" the issue. Billing the
// same body through /v1/messages returned 93,375.
//
// This test therefore asserts the ADDITIVE property on a small body — the one regime where the
// endpoint is trustworthy — and exists to document why the paid /v1/messages path in
// TestBilledPairColdReplay is not optional. Do not calibrate BilledDeltaFactor from this
// endpoint at any size: the regime boundary is undocumented and was not characterised.
func TestCountTokensIsNotABillingOracle(t *testing.T) {
	base, key := os.Getenv("CG_LIVE_URL"), os.Getenv("CG_LIVE_KEY")
	if base == "" || key == "" {
		t.Skip("set CG_LIVE_URL and CG_LIVE_KEY to run this")
	}
	model := os.Getenv("CG_LIVE_MODEL")
	if model == "" {
		model = "claude-haiku-4-5"
	}
	client := &http.Client{Timeout: 60 * time.Second}

	msg := []any{map[string]any{"role": "user", "content": strings.Repeat("count these tokens please. ", 200)}}
	// ~4k tokens of tool declarations and system prompt: far too much to vanish into rounding.
	tools := make([]any, 0, 20)
	for i := 0; i < 20; i++ {
		tools = append(tools, map[string]any{
			"name":        fmt.Sprintf("tool_%d", i),
			"description": strings.Repeat("does a thing with the repository and reports on it. ", 12),
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{
				"path": map[string]any{"type": "string", "description": strings.Repeat("a path. ", 10)}}},
		})
	}
	system := []any{map[string]any{"type": "text", "text": strings.Repeat("You are a careful assistant. ", 150)}}

	full := count(t, client, base, key, map[string]any{
		"model": model, "messages": msg, "tools": tools, "system": system})
	noTools := count(t, client, base, key, map[string]any{
		"model": model, "messages": msg, "system": system})
	noSystem := count(t, client, base, key, map[string]any{
		"model": model, "messages": msg, "tools": tools})

	t.Logf("count_tokens: full=%d  without tools=%d  without system=%d", full, noTools, noSystem)
	if full == 0 || noTools == 0 || noSystem == 0 {
		t.Fatal("count_tokens returned 0 for a non-empty body")
	}
	// On a SMALL body the endpoint must be additive. If this breaks, the endpoint is unreliable
	// at every size and not merely at large ones.
	toolsWorth, systemWorth := full-noTools, full-noSystem
	if toolsWorth <= 0 || systemWorth <= 0 {
		t.Errorf("count_tokens is not additive even on a small body: full=%d noTools=%d noSystem=%d "+
			"(tools worth %d, system worth %d) — it dropped part of the request",
			full, noTools, noSystem, toolsWorth, systemWorth)
	}
	messagesOnly := full - toolsWorth - systemWorth
	if messagesOnly <= 0 {
		t.Errorf("implied messages count is %d; the three parts do not decompose", messagesOnly)
	}
	t.Logf("small-body decomposition: messages=%d tools=%d system=%d (sum=%d, full=%d)",
		messagesOnly, toolsWorth, systemWorth, messagesOnly+toolsWorth+systemWorth, full)
	t.Log("NOTE: additivity here does NOT extend to large bodies — see this test's doc comment. " +
		"Calibrate tokens.BilledDeltaFactor from /v1/messages billing only.")
}

func count(t *testing.T, c *http.Client, base, key string, body map[string]any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, base+"/v1/messages/count_tokens", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("authorization", "Bearer "+key)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("count_tokens HTTP %d: %.200s", resp.StatusCode, raw)
	}
	var v struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v.InputTokens
}
