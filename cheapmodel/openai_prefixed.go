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
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			Details      struct {
				CachedTokens     int `json:"cached_tokens"`
				CacheWriteTokens int `json:"cache_write_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", usage, err
	}
	usage.CacheRead = out.Usage.Details.CachedTokens
	usage.CacheWrite = out.Usage.Details.CacheWriteTokens
	usage.Fresh = max(0, out.Usage.InputTokens-usage.CacheRead-usage.CacheWrite)
	usage.Output = out.Usage.OutputTokens
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
