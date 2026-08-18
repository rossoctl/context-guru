// Package cheapmodel provides a minimal Anthropic Messages client used as the
// engine's injected extraction model. It implements engine.Model. (An OpenAI variant
// is a straightforward follow-up; the canonical model is Anthropic-shaped, so the
// Anthropic client is the natural first.)
package cheapmodel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Anthropic calls a small Anthropic model with a single user prompt and returns the
// text of the first content block.
type Anthropic struct {
	BaseURL   string // default https://api.anthropic.com
	APIKey    string
	Model     string // e.g. claude-haiku-4-5
	MaxTokens int    // default 2048
	Client    *http.Client
	// AuthScheme selects how the API key is sent. "" or "x-api-key" sends the
	// x-api-key header (Anthropic default). "bearer" sends
	// Authorization: Bearer <APIKey> instead, for gateways (e.g. an IBM LiteLLM
	// Anthropic-compatible endpoint) that authenticate with a bearer token. The
	// anthropic-version header is sent in both cases.
	AuthScheme string
}

func (a Anthropic) Complete(ctx context.Context, prompt string) (string, error) {
	return a.CompleteSystem(ctx, "", prompt)
}

// CompleteSystem sends the invariant instructions as a stable `system` block carrying
// a `cache_control` breakpoint, and the per-call variable part as the user message. On
// a repeated call the preamble bills at the cache-READ rate instead of fresh input.
//
// MEASURED CAVEAT (this is why the caller must not assume a win): a breakpoint below
// the model's MINIMUM CACHEABLE PREFIX is silently ignored — no error, no cache entry,
// `cache_creation_input_tokens: 0`. That minimum is 4096 tokens on claude-haiku-4-5 and
// 1024 on claude-sonnet-5, while the extractor's invariant preamble is ~1463 tokens. So
// on the CHEAP model (haiku) this split provably caches NOTHING, and on the agent model
// (model.source: incoming, the default) it does. Split anyway — it is free, correct, and
// wins on the source that can win — but price the gate on cache_read being ZERO.
// Verified against the gateway: haiku 1.5k => write=0 read=0; sonnet 1.5k => write then read.
func (a Anthropic) CompleteSystem(ctx context.Context, system, prompt string) (string, error) {
	if system == "" {
		return a.CompleteBlocks(ctx, nil, prompt)
	}
	return a.CompleteBlocks(ctx, []string{system}, prompt)
}

// CompleteBlocks sends the invariant instructions as ORDERED system blocks and the
// per-call part as the user message. It exists so a caller whose preamble has a
// deployment-wide half and a per-configuration half can keep them as separate blocks
// rather than one joined string (see extract.SystemBlocksModel).
//
// THE BREAKPOINT IS CONDITIONAL, and that is the point. A cache_control below the model's
// minimum cacheable prefix is not an error and not a no-op you can see — the response
// simply reports cache_creation_input_tokens: 0. Measured against the gateway: a
// ~1.5k-token prefix on claude-haiku-4-5 gives write=0 read=0, while the same prefix on
// claude-sonnet-5 caches. Asking for a cache we cannot get is a request field with no
// effect at best; asking for one we get but never read is a 1.25x write we pay for once
// and waste. So place the mark only when the blocks actually clear the floor.
//
// One breakpoint, on the last block, not one per block: nested marks on a two-block
// prefix would create a second cache entry whose only benefit is being shared between
// configurations that differ in the second block, and it is paid for with another write.
// Revisit if two levels are ever measured to be in flight at once often enough to matter.
func (a Anthropic) CompleteBlocks(ctx context.Context, system []string, prompt string) (string, error) {
	base := a.BaseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}
	maxTok := a.MaxTokens
	if maxTok == 0 {
		maxTok = DefaultMaxTokens
	}
	client := a.Client
	if client == nil {
		client = http.DefaultClient
	}
	payload := map[string]any{
		"model":      a.Model,
		"max_tokens": maxTok,
		"messages":   []any{map[string]any{"role": "user", "content": prompt}},
	}
	blocks, releasePrefix := systemBlocks(system, a.Model)
	// Released here so every exit path (transport error, non-200, decode failure) frees the
	// write slot; the success path calls it again with the real outcome and wins, because
	// release is idempotent and first-call-wins.
	defer releasePrefix(false, false)
	if len(blocks) > 0 {
		payload["system"] = blocks
	}
	reqBody, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(base, "/")+"/v1/messages", bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	if a.AuthScheme == "bearer" {
		req.Header.Set("Authorization", "Bearer "+a.APIKey)
	} else {
		req.Header.Set("x-api-key", a.APIKey)
	}
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("cheapmodel: status %d", resp.StatusCode)
	}
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens      int `json:"input_tokens"`
			OutputTokens     int `json:"output_tokens"`
			CacheCreationTok int `json:"cache_creation_input_tokens"`
			CacheReadTok     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	// track CG component LLM cost, split by cache tier so /stats can show whether the
	// preamble breakpoint actually caches (read>0) or is silently ignored (read==0).
	recordUsageCache(ctx, out.Usage.InputTokens, out.Usage.OutputTokens,
		out.Usage.CacheCreationTok, out.Usage.CacheReadTok)
	// Tell the prefix bookkeeping what actually happened, so the next call knows whether a
	// breakpoint would be a read (worth it) or another write (not).
	releasePrefix(out.Usage.CacheCreationTok > 0, out.Usage.CacheReadTok > 0)
	// Return the first content block that carries text. A leading non-text block
	// (e.g. "thinking") has an empty Text, so we skip it rather than returning "".
	for _, c := range out.Content {
		if c.Text != "" {
			return c.Text, nil
		}
	}
	return "", nil
}
