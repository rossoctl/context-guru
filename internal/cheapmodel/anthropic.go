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
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
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
	base := a.BaseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}
	maxTok := a.MaxTokens
	if maxTok == 0 {
		maxTok = 2048
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
	if system != "" {
		payload["system"] = []any{map[string]any{
			"type": "text", "text": system,
			"cache_control": map[string]any{"type": "ephemeral"},
		}}
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
	recordUsageCache(out.Usage.InputTokens, out.Usage.OutputTokens,
		out.Usage.CacheCreationTok, out.Usage.CacheReadTok)
	// Return the first content block that carries text. A leading non-text block
	// (e.g. "thinking") has an empty Text, so we skip it rather than returning "".
	for _, c := range out.Content {
		if c.Text != "" {
			return c.Text, nil
		}
	}
	return "", nil
}

// PrefixUsage is what one CompletePrefixed call cost, straight from the provider's usage block.
// Returned rather than only recorded because the CALLER has to gate on it: a prefix ask whose whole
// justification is the cache read must be able to see that the read happened.
type PrefixUsage struct {
	CacheRead  int
	CacheWrite int
	Fresh      int
	Output     int
}

// CompletePrefixed sends `ask` as a trailing user message appended to prefixBody — an ENTIRE
// previously-sent Anthropic request — so the provider reads the prompt cache that body populated
// instead of being charged fresh for the transcript again.
//
// Measured against the live gateway (docs/experiments/loca/iter019/results.md §2):
//
//   - appending a trailing user message to a byte-identical prefix reads the whole prefix from
//     cache and writes nothing: 19,595 read / 0 created.
//   - `tool_choice` is NOT part of the cache key, so forcing it to "none" is free — and necessary,
//     because the prefix carries the agent's tools and the model will otherwise answer with a
//     tool_use instead of the verdicts.
//   - `tools` ARE part of the key. They are therefore left exactly as the prefix had them; dropping
//     them read a different, smaller entry (19,129) i.e. a separate cache line and a fresh write.
//   - this route REJECTS assistant prefill ("the conversation must end with a user message"), which
//     the appended user message satisfies by construction — but it means prefixBody must not be
//     extended any other way.
//
// Everything else about the body is preserved untouched, because every byte before the appended
// message is prefix and any edit to it costs the cache read this method exists for. `stream` is the
// one exception: the caller wants a single JSON answer, and a streamed response is not that.
func (a Anthropic) CompletePrefixed(ctx context.Context, prefixBody []byte, ask string) (string, PrefixUsage, error) {
	var u PrefixUsage
	if !gjson.GetBytes(prefixBody, "messages").IsArray() {
		return "", u, fmt.Errorf("cheapmodel: prefix body has no messages array")
	}
	n := len(gjson.GetBytes(prefixBody, "messages").Array())
	body, err := sjson.SetBytes(prefixBody, "messages."+strconv.Itoa(n),
		map[string]any{"role": "user", "content": ask})
	if err != nil {
		return "", u, err
	}
	// tool_choice: free (not in the cache key) and required, or the model answers with a tool_use.
	if body, err = sjson.SetBytes(body, "tool_choice", map[string]any{"type": "none"}); err != nil {
		return "", u, err
	}
	// 16k, not the 2048 default, and this is a correctness matter rather than generosity.
	//
	// MEASURED: with 2048 this path produced UNPARSEABLE replies on ~70% of calls (24 of 34). Two
	// things stack up. The request model runs adaptive thinking, which consumes the output budget
	// before any text is emitted -- at max_tokens 900 a probe returned thinking blocks and no text at
	// all. And a verdict array over a 12-item batch, each entry carrying an obligation label and a
	// VERBATIM quote, is simply long. The array was being cut mid-flight, leaving no closing bracket,
	// so the parse failed and the caller changed nothing -- indistinguishable in the counters from a
	// model that declined to act, which is exactly how it was misread for three iterations.
	//
	// Output tokens bill as GENERATED, not as budgeted, so a ceiling this high costs nothing until it
	// is used. An operator who wants a tighter bound sets MaxTokens explicitly.
	maxTok := a.MaxTokens
	if maxTok == 0 {
		maxTok = 16000
	}
	if body, err = sjson.SetBytes(body, "max_tokens", maxTok); err != nil {
		return "", u, err
	}
	body, _ = sjson.DeleteBytes(body, "stream")

	base := a.BaseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}
	client := a.Client
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(base, "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", u, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	if a.APIKey != "" {
		req.Header.Set("x-api-key", a.APIKey)
		req.Header.Set("authorization", "Bearer "+a.APIKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", u, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", u, fmt.Errorf("cheapmodel: prefixed status %d", resp.StatusCode)
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
		return "", u, err
	}
	u = PrefixUsage{CacheRead: out.Usage.CacheReadTok, CacheWrite: out.Usage.CacheCreationTok,
		Fresh: out.Usage.InputTokens, Output: out.Usage.OutputTokens}
	recordUsageCache(out.Usage.InputTokens, out.Usage.OutputTokens,
		out.Usage.CacheCreationTok, out.Usage.CacheReadTok)
	for _, c := range out.Content {
		if c.Text != "" {
			return c.Text, u, nil
		}
	}
	return "", u, nil
}
