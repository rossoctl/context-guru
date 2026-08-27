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
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/rossoctl/context-guru/components"
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

// AsModel is components.Remodeler: same endpoint, same credential, different model.
func (a Anthropic) AsModel(id string) components.Model { a.Model = id; return a }

// WithMaxTokens is components.Budgeter: same call, larger reply budget. See the interface for why a
// caller asking a long question has to be able to raise this.
func (a Anthropic) WithMaxTokens(n int) components.Model { a.MaxTokens = n; return a }

func (a Anthropic) Complete(ctx context.Context, prompt string) (string, error) {
	return a.CompleteSystem(ctx, "", prompt)
}

// CompleteSystem sends the invariant instructions as a stable `system` block carrying
// a `cache_control` breakpoint, and the per-call variable part as the user message. On
// a repeated call the preamble bills at the cache-READ rate instead of fresh input.
//
// MEASURED CAVEAT (this is why the caller must not assume a win): a breakpoint below
// the model's MINIMUM CACHEABLE PREFIX is silently ignored — no error, no cache entry,
// `cache_creation_input_tokens: 0`. That minimum is 4096 PROVIDER tokens on claude-haiku-4-5 and
// 1024 on claude-sonnet-5, while the extractor's invariant preamble is 1,893 o200k tokens. So on
// the CHEAP model (haiku) the preamble ALONE still caches nothing, and on the agent model
// (model.source: incoming, the default) it does. Split anyway — it is free, correct, and wins on
// the source that can win.
//
// The preamble PLUS the conversation context (extract.Cfg.CacheContext, on a multi-candidate
// request) does clear the floor and does cache on haiku, and that only started working once the
// comparison was fixed to convert units — see minCacheableO200k. Verified against the gateway:
// haiku 1.5k => write=0 read=0; haiku 3,673 o200k => write=4,217 then read=4,217; sonnet 1.5k =>
// write then read.
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
		// The upstream's own message, clipped. A bare status is undiagnosable: a 401 can be a
		// wrong auth scheme, an expired key, a model the key may not use, or a gateway policy,
		// and those have nothing in common. Clipped and body-only — the response body of an
		// auth failure does not echo the credential, but the REQUEST headers would, so this
		// deliberately reads the body and never the request.
		return "", fmt.Errorf("cheapmodel: status %d: %s", resp.StatusCode, clipErrBody(resp.Body))
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
	recordUsageCache(ctx, a.Model, out.Usage.InputTokens, out.Usage.OutputTokens,
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

// clipErrBody reads at most errBodyCap bytes of an error response for the message, so a
// gateway that answers with a whole HTML page cannot flood a log line.
func clipErrBody(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, errBodyCap))
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "(empty body)"
	}
	return strings.Join(strings.Fields(s), " ")
}

const errBodyCap = 512

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
// NOTE WHAT THE CACHED REGION IS HERE, because it is not what the rest of this file deals with.
// systemBlocks places OUR OWN cache_control on a system prefix we built, and is therefore subject to
// the provider's minimum cacheable size (4,096 provider tokens on haiku-class). This method places no
// breakpoint at all: the marks are the ones the AGENT's own request already carried, and the region
// they cover is the transcript. That clears any minimum by orders of magnitude — the measurement
// below read 19,595 tokens — so the size floor that governs systemBlocks does not apply, and neither
// does its model-family asymmetry.
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
	maxTok := a.MaxTokens
	if maxTok == 0 {
		maxTok = PrefixAskMaxTokens
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
	// The caller's own credential and scheme, exactly as Complete sends it — the prefix belongs to
	// this caller's cache namespace, so presenting a different credential would read nothing.
	if a.AuthScheme == "bearer" {
		req.Header.Set("Authorization", "Bearer "+a.APIKey)
	} else {
		req.Header.Set("x-api-key", a.APIKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", u, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", u, fmt.Errorf("cheapmodel: prefixed status %d: %s", resp.StatusCode, clipErrBody(resp.Body))
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
	recordUsageCache(ctx, a.Model, out.Usage.InputTokens, out.Usage.OutputTokens,
		out.Usage.CacheCreationTok, out.Usage.CacheReadTok)
	for _, c := range out.Content {
		if c.Text != "" {
			return c.Text, u, nil
		}
	}
	return "", u, nil
}
