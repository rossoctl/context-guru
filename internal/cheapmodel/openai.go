package cheapmodel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/rossoctl/context-guru/components"
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
	msgs := []any{}
	if system != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": system})
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": prompt})
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
