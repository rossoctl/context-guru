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
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/adjudicate"
	"github.com/rossoctl/context-guru/internal/logging"
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
	// CacheWrite1h is the SUBSET of CacheWrite billed at the one-hour write premium rather than
	// the default five-minute rate — never an addition to CacheWrite. See
	// components.PrefixUsage.CacheWrite1h, which this converts to directly (components/
	// prefixask.go), so the two types must keep identical field names, types and order.
	CacheWrite1h int
	Fresh        int
	Output       int
	// ViaTool: the answer came back as a tool_use for adjudicate.ToolName, not as reply text. See
	// components.PrefixUsage.ViaTool -- the field exists so the caller can COUNT which shape it got,
	// because the text parser accepts both and therefore hides the difference.
	ViaTool bool
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
//
//   - NO `tool_choice` IS SET, and a structured-answer tool IS declared. Those two halves are ONE
//     change: the earlier comment here claimed forcing "none" was free and necessary "because the
//     model will otherwise answer with a tool_use instead of the verdicts", and it was RIGHT about
//     the mechanism and WRONG about the remedy. Three arms over the same transcript and ask model,
//     three benchmark passes each, run sequentially:
//
//     tool_choice       tool declared   asks replied   unusable        answered via tool_use
//     {"type":"none"}   no              20             6  (30.0%)      0
//     (omitted)         NO              24             14 (58.3%)      0
//     (omitted)         yes             77             7  (9.1%)       43 (55.8%)
//
//     Fisher two-tailed: row 1 vs row 3 p = 0.0245, row 2 vs row 3 p = 0.0000.
//
//     THE MIDDLE ROW IS THE POINT, and it is the arm that had never been run before: dropping the
//     suppression WITHOUT declaring an answer tool is worse than leaving it in. Freed to call
//     something and offered only the agent's tools plus context_guru_expand, the model calls one of
//     those — observed directly by logging every reply's content blocks, as
//     `thinking,tool_use:context_guru_expand` with NO text block, which this function's text
//     extraction reads as "" and the caller files as unusable. That arm also lost 5 asks to the 90 s
//     llmCallTimeout, against 0 and 1 in the others. So `none` was suppressing a real failure mode;
//     declaring a tool worth calling is what removes the mode instead of trading it for prose.
//
//     Forcing a NAMED tool is separately not free: it wrote a second cache entry (8,378 against the
//     8,268 already cached), so tool_choice DOES participate in the key when it names a tool even
//     though "none" does not. Omitting it entirely reads the prefix for free.
//
//   - `tools` ARE part of the key. They are therefore left exactly as the prefix had them; dropping
//     them read a different, smaller entry (19,129) i.e. a separate cache line and a fresh write.
//
//   - this route REJECTS assistant prefill ("the conversation must end with a user message"), which
//     the ask satisfies by construction: it is always the LAST message insertAskMessage appends,
//     even on the trailing-system-message shape below, which inserts one more message BEFORE it
//     but never after.
//
//   - THE ASK IS NOT ALWAYS THE ONLY THING APPENDED. See insertAskMessage: when prefixBody's own
//     last message is a `role: system` mid-conversation reminder — which Claude Code sends on
//     effectively every turn — appending the ask directly after it is a guaranteed 400 ("role
//     'system' must precede an 'assistant' message or end the array"). insertAskMessage leaves
//     that system message exactly where it is and appends a short synthetic assistant turn plus
//     the ask after it, never touching or reordering a single existing byte.
//
// Everything else about the body is preserved untouched, because every byte before the appended
// message is prefix and any edit to it costs the cache read this method exists for. `stream` is the
// one exception: the caller wants a single JSON answer, and a streamed response is not that.
func (a Anthropic) CompletePrefixed(ctx context.Context, prefixBody []byte, ask string) (string, PrefixUsage, error) {
	var u PrefixUsage
	if !gjson.GetBytes(prefixBody, "messages").IsArray() {
		return "", u, fmt.Errorf("cheapmodel: prefix body has no messages array")
	}
	body, err := insertAskMessage(prefixBody, ask)
	if err != nil {
		return "", u, err
	}
	maxTok := a.MaxTokens
	if maxTok == 0 {
		maxTok = PrefixAskMaxTokens
	}
	var capped bool
	maxTok, capped = thinkingAdjustedMaxTokens(body, maxTok)
	if capped {
		// Signal only -- never a failure path. See thinkingAdjustedMaxTokens's doc comment for
		// the trade-off this branch accepts (a smaller reply allowance than usual).
		logging.From(ctx).Debug("cg.cheapmodel.prefixed_ceiling_applied", "model", a.Model, "max_tokens", maxTok)
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
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		Usage struct {
			InputTokens      int `json:"input_tokens"`
			OutputTokens     int `json:"output_tokens"`
			CacheCreationTok int `json:"cache_creation_input_tokens"`
			CacheReadTok     int `json:"cache_read_input_tokens"`
			// CacheCreation's own ephemeral_1h_input_tokens is the SUBSET of CacheCreationTok
			// billed at the one-hour premium — same field proxy/usage.go's own usage parser reads
			// off the agent's response (cache_creation.ephemeral_1h_input_tokens), read here off
			// this component's OWN call so its cost can be priced at the same per-tier rate.
			CacheCreation struct {
				Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", u, err
	}
	u = PrefixUsage{CacheRead: out.Usage.CacheReadTok, CacheWrite: out.Usage.CacheCreationTok,
		CacheWrite1h: out.Usage.CacheCreation.Ephemeral1h,
		Fresh:        out.Usage.InputTokens, Output: out.Usage.OutputTokens}
	recordUsageCacheWithTTL(ctx, a.Model, out.Usage.InputTokens, out.Usage.OutputTokens,
		out.Usage.CacheCreationTok, out.Usage.CacheReadTok, out.Usage.CacheCreation.Ephemeral1h)
	// A cut-off reply (stop_reason: "max_tokens") is returned as text, not as an error. The sweep
	// detects it and declines at no cost (sweep_reply_truncated); an error here would start a
	// full-price fallback.
	// OUR TOOL'S INPUT BEATS TEXT. When the prefix advertises the structured-answer tool the model uses
	// it of its own accord, and the input arrives already schema-shaped — which removes three failure
	// modes the text path had: prose instead of JSON, verdicts for only part of the batch, and an array
	// cut off by the output budget mid-flight. The raw input is returned as-is, because its field names
	// are the caller's own JSON tags and its `verdicts` array is what the existing parser scans for; the
	// text path below is unchanged and still handles a model that answered in prose anyway.
	//
	// Only OUR tool, by name: the prefix carries the AGENT's tools too, and returning a `Read` call's
	// input would replace a usable prose answer with an argument list that cannot parse.
	for _, c := range out.Content {
		if c.Type == "tool_use" && c.Name == adjudicate.ToolName && len(c.Input) > 0 {
			u.ViaTool = true
			return string(c.Input), u, nil
		}
	}
	for _, c := range out.Content {
		if c.Text != "" {
			return c.Text, u, nil
		}
	}
	return "", u, nil
}

// thinkingAdjustedMaxTokens returns the max_tokens CompletePrefixed must send so the Messages
// API's own invariant holds -- "max_tokens must be greater than thinking.budget_tokens" -- WITHOUT
// touching the thinking block itself.
//
// The thinking block is off limits. Anthropic's prompt-caching docs list the thinking parameters
// (and, in extended mode, the budget) as cache-key material: "Changing thinking parameters
// (switching modes, or changing the budget) invalidates cached message blocks."
// (https://platform.claude.com/docs/en/build-with-claude/prompt-caching, "What invalidates the
// cache"). proxy/keepalive.go:665-669 already leans on the same fact to justify refusing a
// thinking-enabled ping rather than editing the block. CompletePrefixed's entire point is reading
// the AGENT's own cache entry over the transcript it already paid to write, so stripping or
// resizing thinking to dodge the max_tokens error would defeat that -- it would read raw a
// trailing user message against a byte-DIFFERENT prefix and pay fresh for the whole thing, the
// exact cost this method exists to avoid.
//
// So the budget is read, never written: with `thinking.type: "enabled"`, the first candidate is
// budget_tokens + reply, strictly above the budget for any reply > 0 and leaving room for the
// actual answer on top of the thinking spend. `reply` is billed as additional OUTPUT only if the
// model actually produces that many tokens -- same accounting PrefixAskMaxTokens's own doc comment
// already relies on -- so there is no cost to leaving it generous.
//
// THAT CANDIDATE HAS NO CEILING, and a large thinking budget pushes it past the model's own output
// cap -- measured live (PR #406 review): Claude Code with CLAUDE_CODE_MAX_OUTPUT_TOKENS=64000 and
// MAX_THINKING_TOKENS=63999 on claude-haiku-4-5 produced a want of 79999, which the provider
// rejected with "max_tokens: 79999 > 64000, which is the maximum allowed number of output tokens"
// -- the same class of failure this function exists to fix, just with a different message. The
// body's OWN max_tokens is the fallback: this exact 64000/63999 shape was measured live as OK
// (reply "YES", cache_read=16230, cache_write=0), so when budget_tokens is below it and the
// computed want is above it, prefer it over a number that may not clear the cap we cannot see.
//
// TRADE-OFF, stated rather than hidden: on that fallback branch the reply allowance shrinks to
// whatever thinking leaves of the agent's own max_tokens -- as little as ~1 token if the model
// spends the whole budget thinking -- against the generous PrefixAskMaxTokens headroom this
// function gives everywhere else. The alternative would be to learn the model's real output cap and
// use THAT as the ceiling instead of the body's max_tokens, but context-guru has no reachable
// source for it here: internal/modelinfo resolves the INPUT window (Window/Exact), never an output
// cap, and CompletePrefixed takes no components.Ctx to carry one even if modelinfo grew one.
// Plumbing that through is future work, not blocking this fix -- the fallback below is proven valid
// for the shape it was measured against.
//
// `adaptive` thinking carries no budget_tokens and is passed through unchanged, as is anything else
// (disabled, or no thinking block at all): only "enabled" ties max_tokens to a number this call does
// not otherwise know.
//
// `capped` reports whether the fallback branch fired, purely as a SIGNAL for the caller to log --
// it carries no failure of its own and must never be read as one. See the caller for why: the
// smaller reply allowance this branch accepts is already counted elsewhere (sweep_reply_truncated)
// when it actually costs something.
func thinkingAdjustedMaxTokens(body []byte, reply int) (want int, capped bool) {
	if gjson.GetBytes(body, "thinking.type").String() != "enabled" {
		return reply, false
	}
	budget := int(gjson.GetBytes(body, "thinking.budget_tokens").Int())
	want = budget + reply
	if orig := int(gjson.GetBytes(body, "max_tokens").Int()); orig > budget && orig < want {
		return orig, true
	}
	return want, false
}

// insertAskMessage appends `ask` as a new user message to prefixBody's `messages` array. When the
// array's own last message is a `role: system` mid-conversation reminder, it first appends ONE
// short synthetic assistant turn ("Understood.") so the result stays a VALID Anthropic request.
// Nothing already in the array is ever reordered, edited, or removed — only ever appended to.
//
// WHY A TRAILING SYSTEM MESSAGE CANNOT BE FOLLOWED BY THE ASK DIRECTLY. Anthropic's own rule —
// this codebase already enforces it offline, see schema.ValidateShapeFor's RuleSystemPosition and
// systemPositionOK — is that a system-role message away from index 0 must either be IMMEDIATELY
// followed by an assistant message, or END THE ARRAY. Claude Code sends a mid-conversation
// `role: system` reminder as the request's LAST message on effectively every turn
// (docs/components/caching.md: "Claude Code marks its own final message on 466 of 472 measured
// requests"), so appending the ask straight after it is a guaranteed 400 — the provider's own
// error confirms it character-for-character: "role 'system' must precede an 'assistant' message
// or end the array".
//
// WHY NOT INSERT THE ASK BEFORE THE SYSTEM MESSAGE INSTEAD (an earlier version of this fix did).
// That satisfies the position rule too, but it moves the system message's bytes relative to
// everything before it — anything inserted before a cache_control breakpoint changes the byte
// sequence the provider hashes UP TO that breakpoint, which is exactly where Claude Code's own
// entry is keyed (docs/components/caching.md's measurement: the breakpoint sits on the agent's
// own final message, i.e. on this trailing system reminder). Measured live (PR review): with the
// ask inserted before it, the prefix ask got read=0, write=21,652 — a full cache WRITE where the
// read this whole method exists for should have happened. Appending AFTER the system run instead
// changes nothing before or at the breakpoint, so the entry it covers is untouched; measured live
// with that version, read=21,644, write=0 — the full main-agent entry, read rather than rewritten.
//
// WHY A SYNTHETIC ASSISTANT TURN, NOT SOMETHING ELSE. Two consecutive messages of the same role
// are legal on Anthropic (`schema.ValidateShapeFor`, docs/components/summarize.md), so a second
// user message would clear the position rule just as well — but a system message followed by a
// USER message does not change anything about the rule above, which asks for an ASSISTANT message
// or the end of the array; the system message here is in neither position unless something
// assistant-shaped follows it. A short, fixed, content-free string avoids inventing an answer the
// model never gave; "Understood." was the reviewer's own live-tested text (2 of 2 runs: status
// 200, read=21,644, write=0).
//
// OPEN QUESTION FOR A THINKING-ENABLED SESSION, NOT YET VERIFIED LIVE. Anthropic's docs
// (https://platform.claude.com/docs/en/build-with-claude/thinking#thinking-with-tool-use): "In
// extended (manual) mode, the API additionally enforces that the final assistant turn of a
// thinking-enabled request begins with a thinking block. Adaptive mode relaxes this: no assistant
// turn needs to start with one." This synthetic assistant message carries no thinking block. On
// `thinking.type: "adaptive"` (or disabled, or absent) that rule does not apply, so the shape
// above is unaffected. On `thinking.type: "enabled"` (the manual mode PR #406's
// thinkingAdjustedMaxTokens exists for, which Claude Code uses on haiku) it is NOT YET KNOWN
// whether "the final assistant turn" means literally the last assistant message in the array
// regardless of what follows it (our synthetic one), or only an assistant turn an active
// tool-use loop is continuing through (which this one is not — it is followed by a fresh user
// message, not a tool_result). A fabricated thinking block is not an option regardless: the API
// verifies each thinking block's `signature` cryptographically
// (https://platform.claude.com/docs/en/build-with-claude/thinking#thinking-encryption), so one
// this code invents would be rejected, not silently accepted. This needs a live run with
// thinking.type: "enabled" before it can be called settled either way — see the PR for the
// current status of that measurement.
func insertAskMessage(prefixBody []byte, ask string) ([]byte, error) {
	msgs := gjson.GetBytes(prefixBody, "messages").Array()
	parts := make([]string, 0, len(msgs)+2)
	for _, m := range msgs {
		parts = append(parts, m.Raw)
	}
	if len(msgs) > 0 && msgs[len(msgs)-1].Get("role").String() == "system" {
		understoodJSON, err := json.Marshal(map[string]any{"role": "assistant", "content": "Understood."})
		if err != nil {
			return nil, err
		}
		parts = append(parts, string(understoodJSON))
	}
	askJSON, err := json.Marshal(map[string]any{"role": "user", "content": ask})
	if err != nil {
		return nil, err
	}
	parts = append(parts, string(askJSON))
	return sjson.SetRawBytes(prefixBody, "messages", []byte("["+strings.Join(parts, ",")+"]"))
}
