package schema

import "github.com/tidwall/gjson"

// OutputCall is one tool call in a model's response message, as the wire wrote it.
type OutputCall struct {
	ID   string // the call id the result must name: tool_use id, call_id, or tool_calls[].id
	Name string
	// Args is the call's arguments as raw text: Anthropic's input object, a Responses or Chat
	// function call's arguments string, a Responses custom tool call's input string.
	Args string
	// Custom is true for a Responses custom_tool_call: its Args is free text, not JSON.
	Custom bool
}

// OutputCalls reads the tool calls of one response message, in order: Anthropic content tool_use
// blocks, Responses output function_call and custom_tool_call items, or the first Chat Completions
// choice's tool_calls. wire is the dialect (WireAnthropic, WireResponses, else Chat Completions).
// Every reader that splits a response's calls into the host's and the client's reads them here.
func OutputCalls(wire string, msg []byte) []OutputCall {
	var out []OutputCall
	switch {
	case wire == WireAnthropic:
		gjson.GetBytes(msg, "content").ForEach(func(_, b gjson.Result) bool {
			if b.Get("type").String() == "tool_use" {
				out = append(out, OutputCall{ID: b.Get("id").String(), Name: b.Get("name").String(), Args: b.Get("input").Raw})
			}
			return true
		})
	case IsResponses(wire):
		gjson.GetBytes(msg, "output").ForEach(func(_, it gjson.Result) bool {
			switch it.Get("type").String() {
			case "function_call":
				out = append(out, OutputCall{ID: it.Get("call_id").String(), Name: it.Get("name").String(),
					Args: it.Get("arguments").String()})
			case "custom_tool_call":
				out = append(out, OutputCall{ID: it.Get("call_id").String(), Name: it.Get("name").String(),
					Args: it.Get("input").String(), Custom: true})
			}
			return true
		})
	default:
		gjson.GetBytes(msg, "choices.0.message.tool_calls").ForEach(func(_, tc gjson.Result) bool {
			out = append(out, OutputCall{ID: tc.Get("id").String(), Name: tc.Get("function.name").String(),
				Args: tc.Get("function.arguments").String()})
			return true
		})
	}
	return out
}
