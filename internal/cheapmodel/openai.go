package cheapmodel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"strings"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/tokens"
	"github.com/rossoctl/context-guru/schema"
)

// OpenAI calls a small OpenAI chat-completions model with a single user prompt and
// returns the content of the first choice. It implements engine.Model.
type OpenAI struct {
	BaseURL   string // default https://api.openai.com
	APIKey    string
	Model     string // e.g. gpt-4o-mini
	MaxTokens int    // default 2048
	Client    *http.Client
}

// AsModel is components.Remodeler: same endpoint, same credential, different model.
func (o OpenAI) AsModel(id string) components.Model { o.Model = id; return o }

// WithMaxTokens is components.Budgeter: same call, larger reply budget. See the interface for why a
// caller asking a long question has to be able to raise this.
func (o OpenAI) WithMaxTokens(n int) components.Model { o.MaxTokens = n; return o }

func (o OpenAI) Complete(ctx context.Context, prompt string) (string, error) {
	return o.CompleteSystem(ctx, "", prompt)
}

// CompleteSystem puts the invariant instructions in a leading `system` message. OpenAI
// has no explicit cache breakpoints — caching is automatic on a shared prefix — so there
// is nothing to mark; a stable leading system message IS the cacheable-prefix idiom here.
// The split is therefore honest on both backends: same call shape, provider-appropriate
// mechanism, and no `cache_control` field invented for an API that would reject it.
// CompleteBlocks joins the ordered blocks into that one leading system message. There is
// no per-block mechanism to preserve here: OpenAI caches automatically on the longest
// shared prefix, and two system messages would cache exactly as one does. Keeping the
// method means the extractor gets the same content on both backends and only the caching
// mechanism differs (see extract.SystemBlocksModel).
func (o OpenAI) CompleteBlocks(ctx context.Context, system []string, prompt string) (string, error) {
	kept := make([]string, 0, len(system))
	for _, b := range system {
		if strings.TrimSpace(b) != "" {
			kept = append(kept, b)
		}
	}
	return o.CompleteSystem(ctx, strings.Join(kept, "\n\n"), prompt)
}

func (o OpenAI) CompleteSystem(ctx context.Context, system, prompt string) (string, error) {
	msgs := []any{}
	if system != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": system})
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": prompt})
	return o.post(ctx, msgs)
}

// cacheControlPrefixEnd returns the index of the last PREFIX message — the one whose final
// content block should carry a cache_control breakpoint — or -1 to send no mark at all.
//
// WHY A MARK IS NEEDED, AND ONLY HERE. An Anthropic-family model has no automatic prefix
// caching: without an explicit breakpoint the summarization call re-prefills the entire
// conversation, which is precisely the cost CompleteMessages exists to remove. Measured on
// aws/claude-opus-4-7 through a LiteLLM gateway, same ~9.4k-token conversation both ways:
//
//	appended instruction, no breakpoint   fresh=9616 write=0    read=0     (8.2x)
//	appended instruction, breakpoint      fresh=237  write=0    read=9378  (1.0x)
//
// A real OpenAI-shaped endpoint is the opposite case twice over: it caches automatically, so
// the mark buys nothing, and it rejects an unrecognised field inside a content part, so
// sending one would break the call. Hence the substring test on the served id — the same test
// minCacheablePrefix already makes, and conservative in the same direction: an alias that
// hides the real model gets no mark and behaves exactly as it does today.
//
// THE BREAKPOINT ENDS THE PREFIX, it does not end the REQUEST. components.MessagesModel's
// contract is that the caller appends exactly one fresh message, so the cacheable prefix is
// everything before it. Marking the final message instead would write a fresh entry on every
// turn and read nothing — the failure this is meant to remove, with the premium added.
func cacheControlPrefixEnd(model string, msgs []bschemas.ChatMessage) int {
	if len(msgs) < 2 || !strings.Contains(strings.ToLower(model), "claude") {
		return -1
	}
	prefix := msgs[:len(msgs)-1]
	n := 0
	for _, m := range prefix {
		// A MARK ALREADY IN THE PREFIX IS THE CALLER'S, and it is better placed than ours: it is
		// the breakpoint the parent request's cache entry was actually created under, so it is the
		// one a read has to match. bschemas.ChatContentBlock carries CacheControl and the content
		// passthrough preserves it, so an agent that caches its own prompt (Claude Code does)
		// arrives here already marked. Adding a second mark buys nothing and spends one of the
		// provider's FOUR per-request breakpoint slots — a fifth is a 400 on the whole call, which
		// on this path means a wasted summarization and a reverted component.
		if hasCacheControl(m) {
			return -1
		}
		n += tokens.Count(schema.MessageText(m))
	}
	// Below the model's floor the provider ignores the mark silently, so the write premium would
	// be paid for an entry nothing can read. CacheablePrefix owns that table.
	if !CacheablePrefix(model, n) {
		return -1
	}
	// Walk back to the last message that can actually CARRY a mark. An assistant turn that is
	// purely tool calls has no content at all, and a breakpoint needs a content block to attach
	// to. Without this the index could name such a message and the request would go out with no
	// mark — silently, which is the exact failure this change exists to remove. Marking one turn
	// earlier keeps a shorter prefix, which is the safe direction: less is cached, nothing breaks.
	for i := len(prefix) - 1; i >= 0; i-- {
		if _, ok := markedContent(prefix[i].Content); ok {
			return i
		}
	}
	return -1
}

// hasCacheControl reports whether this message already carries a breakpoint the caller placed.
func hasCacheControl(m bschemas.ChatMessage) bool {
	if m.Content == nil {
		return false
	}
	for _, blk := range m.Content.ContentBlocks {
		if blk.CacheControl != nil {
			return true
		}
	}
	return false
}

// markedContent re-renders content as a block array carrying a cache_control breakpoint on its
// LAST block, via a JSON round-trip of the original so every field survives — including block
// kinds this file does not model. Rebuilding blocks field-by-field would silently drop an image
// source or a provider extension, and a dropped field changes the prefix, which loses the cache
// read the mark was added to get.
//
// A bare string becomes a single text block. That is the same content to the provider — string
// and one text block render identically — and it is the only shape a breakpoint can attach to.
func markedContent(c *bschemas.ChatMessageContent) (any, bool) {
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, false
	}
	cc := map[string]any{"type": "ephemeral"}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err == nil && len(blocks) > 0 {
		blocks[len(blocks)-1]["cache_control"] = cc
		return blocks, true
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil && str != "" {
		return []any{map[string]any{"type": "text", "text": str, "cache_control": cc}}, true
	}
	return nil, false
}

// THE MAPPING IS AN EXPLICIT ALLOWLIST, and both halves of that are load-bearing.
//
// CONTENT IS PASSED THROUGH AS-IS, not flattened to text. m.Content is a *ChatMessageContent
// whose MarshalJSON emits either a bare string or the ORIGINAL block array, via MarshalSorted
// (deterministic key order — non-deterministic serialization is itself a silent cache
// invalidator). Flattening blocks to a string would change the rendered prompt and forfeit the
// prefix match this method exists to get.
//
// EVERY OTHER FIELD IS NAMED EXPLICITLY, because marshalling the struct whole leaks fields
// that are not part of an OpenAI chat-completions REQUEST. Measured:
//
//	{"role":"assistant","reasoning":"...","tool_calls":[{"index":0,"id":"call_1",
//	 "function":{"name":null,"arguments":""}}]}
//
// Three defects in one line. `reasoning` is not a request field at all (and a thinking model
// puts one on most assistant turns). `index` is a STREAMING-DELTA field. `"name":null` is
// invalid where the backend requires a string. ChatAssistantMessage also carries Refusal,
// Audio, ReasoningDetails and Annotations, and ChatMessage carries Name — all with omitempty,
// so all silently present the moment they are set.
//
// system is prepended only when non-empty. Pass "" to send the messages EXACTLY as given,
// which is what a cache-reuse caller wants: a leading block the parent request did not have
// changes the prefix in the costliest position.
func (o OpenAI) CompleteMessages(ctx context.Context, system string, msgs []bschemas.ChatMessage) (string, error) {
	wire := make([]any, 0, len(msgs)+1)
	if system != "" {
		wire = append(wire, map[string]any{"role": "system", "content": system})
	}
	markAt := cacheControlPrefixEnd(o.Model, msgs)
	for i := range msgs {
		m := msgs[i]
		e := map[string]any{"role": string(m.Role)}
		// Content verbatim, including block structure. Omitted when absent: an assistant
		// message that is purely tool calls legitimately has none.
		if m.Content != nil {
			e["content"] = m.Content
			// The one deliberate exception to "verbatim": the prefix's last block carries the
			// breakpoint. Metadata only — the rendered content is unchanged, so the bytes the
			// provider hashes are the same ones the agent's own turn cached.
			if i == markAt {
				if marked, ok := markedContent(m.Content); ok {
					e["content"] = marked
				}
			}
		}
		// `name` is request-legal on OpenAI (bifrost tags it "for chat completions"). Passed through
		// rather than dropped: if inbound traffic sets it, omitting it makes this request diverge
		// from the parent's rendered prefix — the exact byte-identity this method exists for.
		if m.Name != nil && *m.Name != "" {
			e["name"] = *m.Name
		}
		if m.ChatToolMessage != nil && m.ChatToolMessage.ToolCallID != nil {
			e["tool_call_id"] = *m.ChatToolMessage.ToolCallID
		}
		if m.ChatAssistantMessage != nil && len(m.ChatAssistantMessage.ToolCalls) > 0 {
			calls := make([]any, 0, len(m.ChatAssistantMessage.ToolCalls))
			for _, tc := range m.ChatAssistantMessage.ToolCalls {
				fn := map[string]any{"arguments": tc.Function.Arguments}
				// Only set name when present: "name": null is invalid, and a nil name means
				// this was a streaming fragment that should never have reached a request.
				if tc.Function.Name != nil {
					fn["name"] = *tc.Function.Name
				}
				call := map[string]any{"function": fn}
				if tc.ID != nil {
					call["id"] = *tc.ID
				}
				// type defaults to "function"; the backend requires it.
				if tc.Type != nil && *tc.Type != "" {
					call["type"] = *tc.Type
				} else {
					call["type"] = "function"
				}
				calls = append(calls, call)
			}
			e["tool_calls"] = calls
		}
		wire = append(wire, e)
	}
	return o.post(ctx, wire)
}

// post is the shared transport: body, auth, status check, usage accounting, first choice.
// Extracted so CompleteSystem and CompleteMessages cannot drift apart — the second one exists
// to preserve a cached prefix, and a divergent request builder is exactly how that is lost.
func (o OpenAI) post(ctx context.Context, msgs []any) (string, error) {
	base := o.BaseURL
	if base == "" {
		base = "https://api.openai.com"
	}
	maxTok := o.MaxTokens
	if maxTok == 0 {
		maxTok = DefaultMaxTokens
	}
	client := o.Client
	if client == nil {
		client = http.DefaultClient
	}
	reqBody, _ := json.Marshal(map[string]any{
		"model":      o.Model,
		"max_tokens": maxTok,
		"messages":   msgs,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(base, "/")+"/v1/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+o.APIKey)

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
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	// OpenAI reports automatic prefix caching as cached_tokens, and counts them INSIDE
	// prompt_tokens (unlike Anthropic, which reports the tiers disjointly). Subtract so
	// the "fresh input" figure means the same thing on both backends.
	cached := out.Usage.PromptTokensDetails.CachedTokens
	recordUsageCache(ctx, o.Model, out.Usage.PromptTokens-cached, out.Usage.CompletionTokens, 0, cached)
	if len(out.Choices) == 0 {
		return "", nil
	}
	return out.Choices[0].Message.Content, nil
}
