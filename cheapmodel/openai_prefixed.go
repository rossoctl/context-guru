package cheapmodel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/rossoctl/context-guru/internal/adjudicate"
	"github.com/rossoctl/context-guru/internal/providerusage"
)

// CompletePrefixedResponses asks the incoming Responses model one question over
// the exact request body previously accepted upstream. The only prompt change is
// one trailing user item, preserving the implicit-cache prefix and opaque input.
func (o OpenAI) CompletePrefixedResponses(ctx context.Context, prefixBody []byte, ask string) (string, PrefixUsage, error) {
	var usage PrefixUsage
	if !gjson.GetBytes(prefixBody, "input").IsArray() {
		return "", usage, fmt.Errorf("cheapmodel: Responses prefix has no input array")
	}
	body, err := sjson.SetBytes(prefixBody, "input.-1", map[string]any{"role": "user", "content": ask})
	if err != nil {
		return "", usage, err
	}
	maxTok := o.MaxTokens
	if maxTok == 0 {
		maxTok = PrefixAskMaxTokens
	}
	// Unlike anthropic.go's CompletePrefixed, no fix needed here: Responses' reasoning control is
	// a qualitative effort level, not a token budget, so there's no field max_output_tokens could contradict.
	if body, err = sjson.SetBytes(body, "max_output_tokens", maxTok); err != nil {
		return "", usage, err
	}
	body, _ = sjson.DeleteBytes(body, "stream")
	respBody, err := send(ctx, o.D, o.Ref, bschemas.OpenAI, "/v1/responses", "prefixed Responses ",
		http.Header{"Content-Type": {"application/json"}}, body)
	if err != nil {
		return "", usage, err
	}
	var out struct {
		Output []struct {
			Type      string `json:"type"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", usage, err
	}
	tiers, _ := providerusage.Parse(gjson.GetBytes(respBody, "usage"))
	usage.CacheRead, usage.CacheWrite = int(tiers.CacheRead), int(tiers.CacheWrite)
	usage.Fresh, usage.Output = int(tiers.FreshInput), int(tiers.Output)
	recordUsageCache(ctx, o.Model, usage.Fresh, usage.Output, usage.CacheWrite, usage.CacheRead)
	var text strings.Builder
	for _, item := range out.Output {
		if item.Type == "function_call" && item.Name == adjudicate.ToolName && item.Arguments != "" {
			usage.ViaTool = true
			return item.Arguments, usage, nil
		}
	}
	for _, item := range out.Output {
		if item.Type != "message" {
			continue
		}
		for _, block := range item.Content {
			if block.Type == "output_text" {
				text.WriteString(block.Text)
			}
		}
	}
	return text.String(), usage, nil
}

// CompletePrefixedChat asks the incoming Chat Completions model one question over the exact request
// body previously accepted upstream. The only prompt change is one trailing user message, so the
// provider's automatic prefix cache (it matches the longest shared prefix) is read, not re-written.
//
// tools and tool_choice stay exactly as the prefix had them: tools are part of the cache key, and the
// model may call context_guru_adjudicate when the request declares it. The reply is read from that
// call when present and from message.content otherwise, so the method works with or without the
// declaration. The cache READ is judged by the caller from the returned usage
// (prompt_tokens_details.cached_tokens).
//
// The path and credential are the incoming client's own (o.Ref and /v1/chat/completions). A route
// with another path or auth scheme fails here with a non-200 and the caller fails open.
func (o OpenAI) CompletePrefixedChat(ctx context.Context, prefixBody []byte, ask string) (string, PrefixUsage, error) {
	var usage PrefixUsage
	if !gjson.GetBytes(prefixBody, "messages").IsArray() {
		return "", usage, fmt.Errorf("cheapmodel: Chat prefix has no messages array")
	}
	body, err := sjson.SetBytes(prefixBody, "messages.-1", map[string]any{"role": "user", "content": ask})
	if err != nil {
		return "", usage, err
	}
	maxTok := o.MaxTokens
	if maxTok == 0 {
		maxTok = PrefixAskMaxTokens
	}
	// Reasoning models reject max_tokens; keep the field name the client used.
	field := "max_tokens"
	if gjson.GetBytes(prefixBody, "max_completion_tokens").Exists() {
		field = "max_completion_tokens"
	}
	if body, err = sjson.SetBytes(body, field, maxTok); err != nil {
		return "", usage, err
	}
	body, _ = sjson.DeleteBytes(body, "stream")
	body, _ = sjson.DeleteBytes(body, "stream_options")
	respBody, err := send(ctx, o.D, o.Ref, bschemas.OpenAI, "/v1/chat/completions", "prefixed Chat ",
		http.Header{"Content-Type": {"application/json"}}, body)
	if err != nil {
		return "", usage, err
	}
	if !gjson.ValidBytes(respBody) {
		return "", usage, fmt.Errorf("cheapmodel: prefixed Chat reply is not JSON")
	}
	tiers, _ := providerusage.Parse(gjson.GetBytes(respBody, "usage"))
	usage.CacheRead, usage.CacheWrite = int(tiers.CacheRead), int(tiers.CacheWrite)
	usage.Fresh, usage.Output = int(tiers.FreshInput), int(tiers.Output)
	recordUsageCache(ctx, o.Model, usage.Fresh, usage.Output, usage.CacheWrite, usage.CacheRead)
	msg := gjson.GetBytes(respBody, "choices.0.message")
	for _, tc := range msg.Get("tool_calls").Array() {
		if args := tc.Get("function.arguments").String(); tc.Get("function.name").String() == adjudicate.ToolName && args != "" {
			usage.ViaTool = true
			return args, usage, nil
		}
	}
	return msg.Get("content").String(), usage, nil
}
