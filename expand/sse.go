package expand

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/rossoctl/context-guru/schema"
)

// AggregateSSE reconstructs the final provider message JSON from a buffered
// Server-Sent Events stream, so the expand loop can inspect it with ResponseCalls /
// Continuation (which operate on the non-streaming response shape). ok=false when the
// stream can't be reconstructed (unknown dialect / malformed) — the caller then
// streams the bytes through unchanged (fail-open).
//
// It reconstructs the Anthropic Messages stream, the Responses stream (from its terminal event)
// and the Chat Completions stream (wire "openai"). For other dialects it returns ok=false.
func AggregateSSE(provider string, raw []byte) (msg []byte, ok bool) {
	switch {
	case schema.IsResponses(provider):
		return aggregateResponsesSSE(raw)
	case provider == schema.WireOpenAI:
		return aggregateChatSSE(raw)
	case provider == schema.WireAnthropic:
		return aggregateAnthropicSSE(raw)
	}
	return nil, false
}

// Responses' terminal event carries the complete response, including its output
// items. Only a completed response is safe to replay as a continuation prefix;
// partial output-item deltas can omit arguments or opaque reasoning state.
func aggregateResponsesSSE(raw []byte) ([]byte, bool) {
	var out []byte
	schema.SSEData(raw, func(d []byte) bool {
		ev := gjson.ParseBytes(d)
		if ev.Get("type").String() != "response.completed" {
			return true
		}
		resp := ev.Get("response")
		if resp.IsObject() && resp.Get("output").IsArray() {
			out = []byte(resp.Raw)
			return false
		}
		return true
	})
	return out, out != nil
}

// aggregateAnthropicSSE walks the message_start → content_block_* → message_delta
// events and rebuilds a {"type":"message","role":"assistant","content":[...],
// "stop_reason":...} object. Text blocks concatenate their text_delta; tool_use
// blocks concatenate input_json_delta.partial_json into the block's input.
func aggregateAnthropicSSE(raw []byte) ([]byte, bool) {
	type block struct {
		typ   string
		text  strings.Builder // for text blocks (and thinking_delta on thinking blocks)
		sig   strings.Builder // for thinking blocks' signature_delta
		start gjson.Result    // the content_block object from content_block_start
	}
	var tools schema.CallStream // the tool_use blocks: schema.CallStream is the one rebuild of them
	blocks := map[int]*block{}
	maxIdx := -1
	stopReason := ""
	sawStart := false

	schema.SSEData(raw, func(payload []byte) bool {
		tools.Add(payload)
		ev := gjson.ParseBytes(payload)
		switch ev.Get("type").String() {
		case "message_start":
			sawStart = true
		case "content_block_start":
			idx := int(ev.Get("index").Int())
			cb := ev.Get("content_block")
			b := &block{typ: cb.Get("type").String(), start: cb}
			blocks[idx] = b
			if idx > maxIdx {
				maxIdx = idx
			}
		case "content_block_delta":
			idx := int(ev.Get("index").Int())
			b := blocks[idx]
			if b == nil {
				b = &block{}
				blocks[idx] = b
				if idx > maxIdx {
					maxIdx = idx
				}
			}
			d := ev.Get("delta")
			switch d.Get("type").String() {
			case "text_delta":
				b.text.WriteString(d.Get("text").String())
			case "thinking_delta":
				b.text.WriteString(d.Get("thinking").String())
			case "signature_delta":
				b.sig.WriteString(d.Get("signature").String())
			}
		case "message_delta":
			if sr := ev.Get("delta.stop_reason").String(); sr != "" {
				stopReason = sr
			}
		}
		return true
	})
	if !sawStart || maxIdx < 0 {
		return nil, false
	}

	out := `{"type":"message","role":"assistant"}`
	if stopReason != "" {
		out, _ = sjson.Set(out, "stop_reason", stopReason)
	}
	out, _ = sjson.SetRaw(out, "content", "[]")
	for i := 0; i <= maxIdx; i++ {
		b := blocks[i]
		if b == nil {
			continue
		}
		switch b.typ {
		case "text":
			blk, _ := sjson.Set(`{"type":"text"}`, "text", b.text.String())
			out, _ = sjson.SetRaw(out, "content.-1", blk)
		case "thinking":
			// Reconstruct the extended-thinking block from its deltas. Anthropic requires
			// the thinking block AND its signature to be echoed back verbatim in a continued
			// assistant turn; dropping them (or the signature) makes the expand-continuation
			// request invalid and the upstream rejects it. Preserve both.
			blk, _ := sjson.Set(`{"type":"thinking"}`, "thinking", b.text.String())
			if sig := b.sig.String(); sig != "" {
				blk, _ = sjson.Set(blk, "signature", sig)
			} else if s := b.start.Get("signature").String(); s != "" {
				blk, _ = sjson.Set(blk, "signature", s)
			}
			out, _ = sjson.SetRaw(out, "content.-1", blk)
		case "tool_use":
			call, _ := tools.Block(i)
			blk := `{"type":"tool_use"}`
			blk, _ = sjson.Set(blk, "id", call.ID)
			blk, _ = sjson.Set(blk, "name", call.Name)
			input := strings.TrimSpace(call.Args)
			if input == "" {
				input = "{}"
			}
			if !gjson.Valid(input) {
				return nil, false // partial JSON didn't reconstruct — bail, stream through
			}
			blk, _ = sjson.SetRaw(blk, "input", input)
			out, _ = sjson.SetRaw(out, "content.-1", blk)
		default:
			// unknown block type — reproduce the start object verbatim so we don't drop it
			if b.start.Exists() {
				out, _ = sjson.SetRaw(out, "content.-1", b.start.Raw)
			}
		}
	}
	return []byte(out), true
}

// aggregateChatSSE rebuilds a Chat Completions object from its event stream: the first choice's
// text and tool calls (by index, arguments joined), its finish reason and the usage chunk. ok=false
// when the stream holds no chunk.
func aggregateChatSSE(raw []byte) ([]byte, bool) {
	var text, reasoning strings.Builder
	var tools schema.CallStream // the tool calls: schema.CallStream is the one rebuild of them
	finish, usage, id, model := "", "", "", ""
	seen := false
	schema.SSEData(raw, func(d []byte) bool {
		p := string(d)
		if !gjson.Valid(p) {
			return true
		}
		ev := gjson.Parse(p)
		if !ev.Get("choices").Exists() {
			// Not a Chat Completions chunk. One small change from the copy this replaced, which
			// counted any data line: a stream with no chunk now returns ok=false instead of an
			// empty message. serve bails on both, so the client gets the same bytes.
			return true
		}
		seen = true
		if id == "" {
			id, model = ev.Get("id").String(), ev.Get("model").String()
		}
		if u := ev.Get("usage"); u.IsObject() {
			usage = u.Raw
		}
		ch := ev.Get("choices.0")
		if !ch.Exists() {
			return true
		}
		if f := ch.Get("finish_reason").String(); f != "" {
			finish = f
		}
		dl := ch.Get("delta")
		text.WriteString(dl.Get("content").String())
		reasoning.WriteString(dl.Get("reasoning_content").String())
		tools.Add(d)
		return true
	})
	if !seen {
		return nil, false
	}
	msg := `{"role":"assistant"}`
	msg, _ = sjson.Set(msg, "content", text.String())
	if reasoning.Len() > 0 {
		// GLM and Kimi stream their reasoning: the follow-up's assistant turn keeps it.
		msg, _ = sjson.Set(msg, "reasoning_content", reasoning.String())
	}
	for _, c := range tools.Calls() {
		tc, _ := sjson.Set(`{"type":"function","function":{}}`, "id", c.ID)
		tc, _ = sjson.Set(tc, "function.name", c.Name)
		tc, _ = sjson.Set(tc, "function.arguments", c.Args)
		msg, _ = sjson.SetRaw(msg, "tool_calls.-1", tc)
	}
	out := `{"object":"chat.completion","choices":[{"index":0}]}`
	out, _ = sjson.Set(out, "id", id)
	out, _ = sjson.Set(out, "model", model)
	out, _ = sjson.SetRaw(out, "choices.0.message", msg)
	out, _ = sjson.Set(out, "choices.0.finish_reason", finish)
	if usage != "" {
		out, _ = sjson.SetRaw(out, "usage", usage)
	}
	return []byte(out), true
}
