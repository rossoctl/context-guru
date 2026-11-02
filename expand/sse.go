package expand

import (
	"bufio"
	"bytes"
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
// Only the Anthropic Messages event stream is reconstructed (the streaming coding
// agents in scope use it). For other dialects it returns ok=false.
func AggregateSSE(provider string, raw []byte) (msg []byte, ok bool) {
	if schema.IsResponses(provider) {
		return aggregateResponsesSSE(raw)
	}
	if provider != schema.WireAnthropic {
		return nil, false
	}
	return aggregateAnthropicSSE(raw)
}

// Responses' terminal event carries the complete response, including its output
// items. Only a completed response is safe to replay as a continuation prefix;
// partial output-item deltas can omit arguments or opaque reasoning state.
func aggregateResponsesSSE(raw []byte) ([]byte, bool) {
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		ev := gjson.Parse(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		if ev.Get("type").String() != "response.completed" {
			continue
		}
		resp := ev.Get("response")
		if resp.IsObject() && resp.Get("output").IsArray() {
			return []byte(resp.Raw), true
		}
	}
	return nil, false
}

// aggregateAnthropicSSE walks the message_start → content_block_* → message_delta
// events and rebuilds a {"type":"message","role":"assistant","content":[...],
// "stop_reason":...} object. Text blocks concatenate their text_delta; tool_use
// blocks concatenate input_json_delta.partial_json into the block's input.
func aggregateAnthropicSSE(raw []byte) ([]byte, bool) {
	type block struct {
		typ     string
		text    strings.Builder // for text blocks (and thinking_delta on thinking blocks)
		partial strings.Builder // for tool_use input_json_delta
		sig     strings.Builder // for thinking blocks' signature_delta
		start   gjson.Result    // the content_block object from content_block_start
	}
	blocks := map[int]*block{}
	maxIdx := -1
	stopReason := ""
	sawStart := false

	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "" || payload == "[DONE]" {
			continue
		}
		ev := gjson.Parse(payload)
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
			case "input_json_delta":
				b.partial.WriteString(d.Get("partial_json").String())
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
	}
	if err := sc.Err(); err != nil || !sawStart || maxIdx < 0 {
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
			blk := `{"type":"tool_use"}`
			blk, _ = sjson.Set(blk, "id", b.start.Get("id").String())
			blk, _ = sjson.Set(blk, "name", b.start.Get("name").String())
			input := strings.TrimSpace(b.partial.String())
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
