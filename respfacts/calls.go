package respfacts

import (
	"bytes"

	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/agent"
	"github.com/rossoctl/context-guru/schema"
)

// stopReasonPaths are where a terminal reason lives, per dialect and per transport:
//
//	Anthropic  stop_reason              (non-streaming)
//	           delta.stop_reason        (message_delta, the streamed terminal event)
//	OpenAI     choices.0.finish_reason  (both; the streamed final chunk carries it too)
//	Responses  incomplete_details.reason           (non-streaming, status "incomplete")
//	           response.incomplete_details.reason  (the response.incomplete event)
//
// A Responses response that finishes has no stop reason: its status is "completed" for a tool turn
// and for the last turn alike, so the status is not read. Only an incomplete response names why it
// stopped (max_output_tokens, content_filter).
//
// The values are the providers' own vocabulary and are recorded verbatim rather than
// mapped onto one another: `end_turn` and `stop` mean the same thing, but flattening them
// would quietly assert that `pause_turn`, `refusal` and `content_filter` have equivalents
// on the other side, and they do not.
var stopReasonPaths = [...]string{"stop_reason", "delta.stop_reason", "choices.0.finish_reason",
	"incomplete_details.reason", "response.incomplete_details.reason"}

// StopReason reads the terminal reason off a response body. For SSE the terminal reason is in the
// LAST event that carries one, and an earlier chunk's `finish_reason: null` must not win.
//
// Whatever this returns is still client-influenced text by the time it reaches the
// database (an upstream is configurable, so its response is not trusted input), so it is
// re-checked by dash.metaEnum before the insert.
func StopReason(sse bool, body []byte) string {
	if !sse {
		return firstStopReason(string(body))
	}
	last := ""
	schema.SSEData(body, func(d []byte) bool {
		// Every path ends in "reason". A text delta has none, and most events are text deltas, so
		// they cost one byte search and no JSON parse.
		if !bytes.Contains(d, []byte("reason")) {
			return true
		}
		if s := firstStopReasonBytes(d); s != "" {
			last = s
		}
		return true
	})
	return last
}

// ToolUseNames reads every tool_use / function-call block NAME off a response, in the
// order they appear, for issue #424's waitcause package. Three dialects, each read structurally
// (never a regex over tool names) off the same bytes StopReason reads:
//
//	Anthropic Messages   content[].{type: "tool_use", name}         (buffered JSON, or an SSE
//	                      content_block_start event's content_block)
//	OpenAI chat           choices[0].message.tool_calls[].function.name (buffered), or a
//	                      streamed delta.tool_calls[].function.name chunk
//	OpenAI Responses      output[].{type: "function_call", name}    (buffered), or a streamed
//	                      output_item.added / output_item.done / response.completed event
//
// A stream is rebuilt by schema.CallStream, the same rebuild every other reader of a streamed call
// uses. A Responses custom tool call is not counted.
//
// Best-effort by construction: a sniffed stream only has a bounded head+tail window (see
// sniffer), so a tool_use far into a very long response can fall outside it. That under-reports
// own_tool, it never invents one.
func ToolUseNames(sse bool, body []byte) []string { return namesOf(callsOf(sse, body)) }

// callsOf reads the tool calls of a response, a whole body or an event stream, of any dialect.
func callsOf(sse bool, body []byte) []schema.OutputCall {
	if sse {
		return schema.StreamCalls(body)
	}
	// The body names no dialect, so all three are read; a body matches only one of them.
	var calls []schema.OutputCall
	for _, wire := range []string{schema.WireAnthropic, schema.WireOpenAI, schema.WireResponses} {
		calls = append(calls, schema.OutputCalls(wire, body)...)
	}
	return calls
}

// namesOf is the names of the calls the client must run: a Responses custom tool call is not
// counted, and neither is a call with no name.
func namesOf(calls []schema.OutputCall) []string {
	var names []string
	for _, c := range calls {
		if !c.Custom && c.Name != "" {
			names = append(names, c.Name)
		}
	}
	return names
}

// firstStopReason returns the first non-empty stop reason among the known paths.
func firstStopReason(payload string) string {
	for _, p := range stopReasonPaths {
		if r := gjson.Get(payload, p); r.Type == gjson.String && r.Str != "" {
			return r.Str
		}
	}
	return ""
}

// firstStopReasonBytes is firstStopReason for bytes, without a copy of the payload.
func firstStopReasonBytes(payload []byte) string {
	for _, p := range stopReasonPaths {
		if r := gjson.GetBytes(payload, p); r.Type == gjson.String && r.Str != "" {
			return r.Str
		}
	}
	return ""
}

// ToolParams is what the last tool call of a response said about how long its wait may last
// (#1198). Each value has its own "stated" flag, because an absent parameter and a zero are
// different facts. Metadata only: one number and one flag; the rest of the input is dropped.
type ToolParams struct {
	TimeoutMs     int64 // the call's `timeout`, in ms (Claude Code's Bash and others); valid when HasTimeout
	HasTimeout    bool
	Background    bool // the call's `run_in_background`; valid when HasBackground
	HasBackground bool
}

// ToolParamsOf reads ONLY `timeout` (a positive number) and `run_in_background` (a boolean) off a
// tool call's input JSON. Any other key, and any other type, is ignored.
func ToolParamsOf(input string) ToolParams {
	var p ToolParams
	if !gjson.Valid(input) {
		return p
	}
	if t := gjson.Get(input, "timeout"); t.Type == gjson.Number && t.Int() > 0 {
		p.TimeoutMs, p.HasTimeout = t.Int(), true
	}
	if b := gjson.Get(input, "run_in_background"); b.Type == gjson.True || b.Type == gjson.False {
		p.Background, p.HasBackground = b.Bool(), true
	}
	return p
}

// LastToolParams reads the parameters of the LAST tool call of a response, the call
// ToolUseNames reports last (and the wait tool of the row). A buffered response carries the input
// whole, and a stream carries it in pieces that schema.CallStream joins. The caller skips a spliced
// window, whose last call may be missing.
func LastToolParams(sse bool, body []byte) ToolParams { return lastToolParams(callsOf(sse, body)) }

func lastToolParams(calls []schema.OutputCall) ToolParams {
	var last *schema.OutputCall
	for i := range calls {
		if !calls[i].Custom && calls[i].Name != "" {
			last = &calls[i]
		}
	}
	if last == nil {
		return ToolParams{}
	}
	return ToolParamsOf(last.Args)
}

// Spawn is the subagent calls one response made: N of them, and the subagent_type of the last that
// stated one. Only the type is read from their input, never the prompt.
type Spawn struct {
	N    int
	Type string
}

// Spawns counts the subagent calls of a response and reads the subagent_type of the last one.
// c is the client that sent the request. Its rules say which tool is a subagent call, and the zero
// Client gets the fallback rule of every client (see agent.Client.IsSubagentTool). A stream is
// rebuilt by schema.CallStream.
func Spawns(c agent.Client, sse bool, body []byte) Spawn { return spawnsOf(c, callsOf(sse, body)) }

func spawnsOf(c agent.Client, calls []schema.OutputCall) Spawn {
	var sp Spawn
	for _, call := range calls {
		if call.Custom || !c.IsSubagentTool(call.Name) {
			continue
		}
		sp.N++
		if t := gjson.Get(call.Args, c.SubagentTypeField()); t.Type == gjson.String && len(t.Str) <= 64 {
			sp.Type = t.Str
		}
	}
	return sp
}
