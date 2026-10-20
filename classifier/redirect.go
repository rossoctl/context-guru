package classifier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dispatch"
	"github.com/rossoctl/context-guru/routing"
)

// Defaults for the retry loop. The agent waits on a classifier verdict, so a slow route must not
// hold it: the TOTAL budget is short, and the attempts share it.
const (
	DefaultAttempts = 3
	DefaultBudget   = 8 * time.Second
	DefaultBackoff  = 100 * time.Millisecond
)

// Options tune the retry loop. Zero values mean the defaults; Backoff < 0 means none (tests).
type Options struct {
	Attempts int
	Budget   time.Duration
	Backoff  time.Duration
}

func (o Options) norm() Options {
	if o.Attempts <= 0 {
		o.Attempts = DefaultAttempts
	}
	if o.Budget <= 0 {
		o.Budget = DefaultBudget
	}
	if o.Backoff == 0 {
		o.Backoff = DefaultBackoff
	}
	if o.Backoff < 0 {
		o.Backoff = 0
	}
	return o
}

// Usage is the routed call's billed tokens, in the Anthropic tiers (fresh and cache-read are
// disjoint, as Anthropic reports them).
type Usage struct {
	Fresh, CacheRead, CacheWrite, Output int64
}

// Result is a successful redirect.
type Result struct {
	// Body is an Anthropic Messages response, ready to write to the client.
	Body  []byte
	Usage Usage
	// Attempts is how many calls it took.
	Attempts int
}

// ErrInaccessible means the routed model did not give a usable answer within the attempts and the
// budget. The host forwards the original request. It wraps the last cause.
var ErrInaccessible = errors.New("classifier: routed model inaccessible")

// Redirect sends body (an Anthropic /v1/messages request) to route and returns the answer as an
// Anthropic response. Any failure, including a panic in conversion, returns an error and the
// host falls back. A verdict (allow or block) is just text: it is never a failure.
func Redirect(ctx context.Context, d dispatch.Dispatcher, route routing.Route, body []byte, opt Options) (res Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			res, err = Result{}, fmt.Errorf("classifier: recovered: %v", r)
		}
	}()
	opt = opt.norm()
	if d == nil {
		return Result{}, errors.New("classifier: no dispatcher")
	}
	req, hdr, prov, err := buildRequest(route, body)
	if err != nil {
		return Result{}, err // our own conversion failed; retrying cannot help
	}
	stops := stopSequences(body)

	ctx, cancel := context.WithTimeout(ctx, opt.Budget)
	defer cancel()
	var last error
	for attempt := 1; attempt <= opt.Attempts; attempt++ {
		if attempt > 1 && opt.Backoff > 0 {
			select {
			case <-time.After(opt.Backoff):
			case <-ctx.Done():
			}
		}
		if ctx.Err() != nil {
			last = ctx.Err()
			break
		}
		resp, derr := d.Do(ctx, dispatch.Request{Purpose: dispatch.PurposeRouted, Ref: route.Ref,
			Provider: prov, Method: http.MethodPost, Path: route.Path, Header: hdr, Body: req})
		if derr != nil {
			last = derr
			continue
		}
		if resp.Status < 200 || resp.Status > 299 {
			// 5xx, 401/403, 404, 429 and any other non-2xx: the route cannot be used.
			last = fmt.Errorf("status %d", resp.Status)
			continue
		}
		out, usage, perr := parseAnswer(route, resp.Body, stops)
		if perr != nil {
			last = perr
			continue
		}
		return Result{Body: out, Usage: usage, Attempts: attempt}, nil
	}
	return Result{}, fmt.Errorf("%w: %v", ErrInaccessible, last)
}

func stopSequences(body []byte) []string {
	var out []string
	gjson.GetBytes(body, "stop_sequences").ForEach(func(_, v gjson.Result) bool {
		if v.Type == gjson.String && v.Str != "" {
			out = append(out, v.Str)
		}
		return true
	})
	return out
}

// buildRequest converts the Anthropic request for the row's dialect.
func buildRequest(route routing.Route, body []byte) (req []byte, hdr http.Header, prov bschemas.ModelProvider, err error) {
	hdr = http.Header{"Content-Type": {"application/json"}}
	if route.Dialect == config.DialectAnthropic {
		// Same shape: only the model changes. Streaming is off by detection.
		req, err = sjson.SetBytes(body, "model", route.Model)
		hdr.Set("anthropic-version", "2023-06-01")
		return req, hdr, bschemas.Anthropic, err
	}
	req, err = anthropicToOpenAI(route.Model, body)
	return req, hdr, bschemas.OpenAI, err
}

// anthropicToOpenAI builds an OpenAI chat-completions request: system blocks become ONE system
// message, content blocks become text. A block that is not text (an image, a tool call) cannot be
// carried faithfully, so it is an error and the original request goes the normal way.
func anthropicToOpenAI(model string, body []byte) ([]byte, error) {
	var msgs []map[string]any
	if sys := gjson.GetBytes(body, "system"); sys.Exists() {
		text, err := blocksText(sys)
		if err != nil {
			return nil, fmt.Errorf("system: %w", err)
		}
		if text != "" {
			msgs = append(msgs, map[string]any{"role": "system", "content": text})
		}
	}
	var convErr error
	gjson.GetBytes(body, "messages").ForEach(func(_, m gjson.Result) bool {
		role := m.Get("role").String()
		if role != "user" && role != "assistant" {
			convErr = fmt.Errorf("message role %q", role)
			return false
		}
		text, err := blocksText(m.Get("content"))
		if err != nil {
			convErr = fmt.Errorf("message: %w", err)
			return false
		}
		msgs = append(msgs, map[string]any{"role": role, "content": text})
		return true
	})
	if convErr != nil {
		return nil, convErr
	}
	if len(msgs) == 0 {
		return nil, errors.New("no messages")
	}
	out := map[string]any{"model": model, "messages": msgs, "stream": false}
	if mt := gjson.GetBytes(body, "max_tokens"); mt.Exists() {
		out["max_tokens"] = mt.Int()
	}
	if t := gjson.GetBytes(body, "temperature"); t.Type == gjson.Number {
		out["temperature"] = t.Float()
	}
	// stop_sequences are NOT sent: some endpoints cap or reject `stop`, and the cut is made on the
	// answer instead (parseAnswer), which also yields the Anthropic stop_sequence field.
	return json.Marshal(out)
}

// blocksText joins text blocks with a blank line. A bare string is one block.
func blocksText(v gjson.Result) (string, error) {
	if v.Type == gjson.String {
		return v.Str, nil
	}
	if !v.IsArray() {
		return "", errors.New("content is neither string nor blocks")
	}
	var parts []string
	var err error
	v.ForEach(func(_, b gjson.Result) bool {
		if b.Get("type").String() != "text" {
			err = fmt.Errorf("block type %q is not text", b.Get("type").String())
			return false
		}
		parts = append(parts, b.Get("text").String())
		return true
	})
	return strings.Join(parts, "\n\n"), err
}

// parseAnswer turns the routed model's answer into an Anthropic response. Empty or unparseable
// text is an error (the caller retries, then falls back).
func parseAnswer(route routing.Route, raw []byte, stops []string) ([]byte, Usage, error) {
	if !gjson.ValidBytes(raw) {
		return nil, Usage{}, errors.New("answer is not JSON")
	}
	var text, stopReason string
	var u Usage
	if route.Dialect == config.DialectAnthropic {
		gjson.GetBytes(raw, "content").ForEach(func(_, b gjson.Result) bool {
			if b.Get("type").String() == "text" {
				text += b.Get("text").String()
			}
			return true
		})
		stopReason = gjson.GetBytes(raw, "stop_reason").String()
		u = Usage{
			Fresh:      gjson.GetBytes(raw, "usage.input_tokens").Int(),
			CacheRead:  gjson.GetBytes(raw, "usage.cache_read_input_tokens").Int(),
			CacheWrite: gjson.GetBytes(raw, "usage.cache_creation_input_tokens").Int(),
			Output:     gjson.GetBytes(raw, "usage.output_tokens").Int(),
		}
	} else {
		c := gjson.GetBytes(raw, "choices.0.message.content")
		if c.IsArray() { // content parts
			c.ForEach(func(_, p gjson.Result) bool { text += p.Get("text").String(); return true })
		} else {
			text = c.String()
		}
		if gjson.GetBytes(raw, "choices.0.finish_reason").String() == "length" {
			stopReason = "max_tokens"
		}
		prompt := gjson.GetBytes(raw, "usage.prompt_tokens").Int()
		cached := gjson.GetBytes(raw, "usage.prompt_tokens_details.cached_tokens").Int()
		// OpenAI counts cached tokens INSIDE prompt_tokens; Anthropic reports the tiers apart.
		if cached > prompt {
			cached = prompt
		}
		u = Usage{Fresh: prompt - cached, CacheRead: cached,
			Output: gjson.GetBytes(raw, "usage.completion_tokens").Int()}
	}
	if strings.TrimSpace(text) == "" {
		return nil, Usage{}, errors.New("answer has no text")
	}
	stopSeq := ""
	if route.Dialect != config.DialectAnthropic {
		// Cut at the earliest stop sequence, as Anthropic would have.
		cut := -1
		for _, s := range stops {
			if i := strings.Index(text, s); i >= 0 && (cut < 0 || i < cut) {
				cut, stopSeq = i, s
			}
		}
		if cut >= 0 {
			text = text[:cut]
			stopReason = "stop_sequence"
		}
	} else if stopReason == "stop_sequence" {
		stopSeq = gjson.GetBytes(raw, "stop_sequence").String()
	}
	if stopReason == "" {
		stopReason = "end_turn"
	}
	resp := map[string]any{
		"id":    "msg_cg_" + fmt.Sprintf("%d", time.Now().UnixNano()),
		"type":  "message",
		"role":  "assistant",
		"model": route.Model,
		"content": []map[string]any{
			{"type": "text", "text": text},
		},
		"stop_reason": stopReason,
		"usage": map[string]any{
			"input_tokens":                u.Fresh,
			"cache_read_input_tokens":     u.CacheRead,
			"cache_creation_input_tokens": u.CacheWrite,
			"output_tokens":               u.Output,
		},
	}
	if stopReason == "stop_sequence" {
		resp["stop_sequence"] = stopSeq
	} else {
		resp["stop_sequence"] = nil
	}
	out, err := json.Marshal(resp)
	return out, u, err
}
