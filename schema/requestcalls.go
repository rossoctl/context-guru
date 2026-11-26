package schema

import (
	"strconv"

	"github.com/tidwall/gjson"
)

// RequestCall is one tool call in a REQUEST transcript: a call the model made on an earlier turn, as
// the client sent it back. OutputCalls reads the same thing in a response.
type RequestCall struct {
	ID   string // the id the result names: tool_use id, call_id, or tool_calls[].id
	Name string
	// Custom is true for a Responses custom_tool_call: its Args is free text, not JSON.
	Custom bool
	// At is the index of the transcript element that holds the call: the message, or the input item.
	At int
	// Raw is the call: the tool_use block, the tool_calls element, or the Responses item. Args reads
	// it on demand, so a caller that only needs ids and names does not pay for the arguments.
	Raw  gjson.Result
	wire string
}

// Args is the call's arguments as raw text: Anthropic's input object, a Chat or Responses function
// call's arguments string, a Responses custom tool call's input string.
func (c RequestCall) Args() string {
	switch {
	case c.wire == WireAnthropic:
		return c.Raw.Get("input").Raw
	case IsResponses(c.wire) && c.Custom:
		return c.Raw.Get("input").String()
	case IsResponses(c.wire):
		return c.Raw.Get("arguments").String()
	}
	return c.Raw.Get("function.arguments").String()
}

// RequestResult is one tool result in a request transcript.
type RequestResult struct {
	CallID string // the id of the call the result answers
	// At is the index of the transcript element, and Block the index of the result block in its
	// content, or -1 when the element is the result itself (a Chat tool message, a Responses output).
	At, Block int
	// Raw is the result element or block.
	Raw gjson.Result
}

// Path is the gjson path of the result, under the transcript's own array name ("messages" or "input").
func (r RequestResult) Path(wire string) string {
	p := TranscriptField(wire) + "." + strconv.Itoa(r.At)
	if r.Block >= 0 {
		p += ".content." + strconv.Itoa(r.Block)
	}
	return p
}

// TranscriptField is the name of the array that holds the conversation: input on Responses, else messages.
func TranscriptField(wire string) string {
	if IsResponses(wire) {
		return "input"
	}
	return "messages"
}

// RequestCalls reads the tool calls of a request transcript, in order: Anthropic tool_use content
// blocks, Chat Completions tool_calls, or Responses function_call and custom_tool_call items. A call
// with no id is left out: an empty id would match a result that names no call, which belongs to
// someone else.
func RequestCalls(wire string, body []byte) []RequestCall {
	var out []RequestCall
	EachRequestCall(wire, body, func(c RequestCall) { out = append(out, c) })
	return out
}

// EachRequestCall is RequestCalls without the slice, for a caller on the request path.
func EachRequestCall(wire string, body []byte, fn func(RequestCall)) {
	emit := func(c RequestCall) {
		if c.ID != "" {
			fn(c)
		}
	}
	gjson.GetBytes(body, TranscriptField(wire)).ForEach(func(k, m gjson.Result) bool {
		at := int(k.Int())
		switch {
		case wire == WireAnthropic:
			m.Get("content").ForEach(func(_, b gjson.Result) bool {
				if b.Get("type").String() == "tool_use" {
					emit(RequestCall{ID: b.Get("id").String(), Name: b.Get("name").String(), At: at, Raw: b, wire: wire})
				}
				return true
			})
		case IsResponses(wire):
			switch m.Get("type").String() {
			case "function_call":
				emit(RequestCall{ID: m.Get("call_id").String(), Name: m.Get("name").String(), At: at, Raw: m, wire: wire})
			case "custom_tool_call":
				emit(RequestCall{ID: m.Get("call_id").String(), Name: m.Get("name").String(), Custom: true, At: at, Raw: m, wire: wire})
			}
		default:
			m.Get("tool_calls").ForEach(func(_, tc gjson.Result) bool {
				emit(RequestCall{ID: tc.Get("id").String(), Name: tc.Get("function.name").String(), At: at, Raw: tc, wire: wire})
				return true
			})
		}
		return true
	})
}

// RequestResults reads the tool results of a request transcript, in order: Anthropic tool_result
// content blocks, Chat Completions tool messages, or Responses function_call_output and
// custom_tool_call_output items. A result with no call id is left out, for the reason RequestCalls
// leaves out a call with none.
func RequestResults(wire string, body []byte) []RequestResult {
	var out []RequestResult
	EachRequestResult(wire, body, func(r RequestResult) { out = append(out, r) })
	return out
}

// EachRequestResult is RequestResults without the slice, for a caller on the request path.
func EachRequestResult(wire string, body []byte, fn func(RequestResult)) {
	emit := func(r RequestResult) {
		if r.CallID != "" {
			fn(r)
		}
	}
	gjson.GetBytes(body, TranscriptField(wire)).ForEach(func(k, m gjson.Result) bool {
		at := int(k.Int())
		switch {
		case wire == WireAnthropic:
			bi := 0
			m.Get("content").ForEach(func(_, b gjson.Result) bool {
				if b.Get("type").String() == "tool_result" {
					emit(RequestResult{CallID: b.Get("tool_use_id").String(), At: at, Block: bi, Raw: b})
				}
				bi++
				return true
			})
		case IsResponses(wire):
			switch m.Get("type").String() {
			case "function_call_output", "custom_tool_call_output":
				emit(RequestResult{CallID: m.Get("call_id").String(), At: at, Block: -1, Raw: m})
			}
		default:
			if m.Get("role").String() == "tool" {
				emit(RequestResult{CallID: m.Get("tool_call_id").String(), At: at, Block: -1, Raw: m})
			}
		}
		return true
	})
}
