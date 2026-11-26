package respfacts

import (
	"strings"

	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/agent"
	"github.com/rossoctl/context-guru/schema"
)

// stopReasonPaths are where a terminal reason lives, per dialect and per transport:
//
//	Anthropic  stop_reason              (non-streaming)
//	           delta.stop_reason        (message_delta, the streamed terminal event)
//	OpenAI     choices.0.finish_reason  (both; the streamed final chunk carries it too)
//
// The values are the providers' own vocabulary and are recorded verbatim rather than
// mapped onto one another: `end_turn` and `stop` mean the same thing, but flattening them
// would quietly assert that `pause_turn`, `refusal` and `content_filter` have equivalents
// on the other side, and they do not.
var stopReasonPaths = [...]string{"stop_reason", "delta.stop_reason", "choices.0.finish_reason"}

// StopReason reads the terminal reason off a response body. For SSE the events
// are scanned newest-first: the terminal reason is in the LAST event that carries one, and
// an earlier chunk's `finish_reason: null` must not win.
//
// Whatever this returns is still client-influenced text by the time it reaches the
// database (an upstream is configurable, so its response is not trusted input), so it is
// re-checked by dash.metaEnum before the insert.
func StopReason(sse bool, body []byte) string {
	if !sse {
		return firstStopReason(string(body))
	}
	lines := strings.Split(string(body), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		if s := firstStopReason(payload); s != "" {
			return s
		}
	}
	return ""
}

// ToolUseNames reads every tool_use / function-call block NAME off a response, in the
// order they appear, for issue #424's waitcause package. Three dialects, each read structurally
// (never a regex over tool names) off the same bytes StopReason reads:
//
//	Anthropic Messages   content[].{type: "tool_use", name}         (buffered JSON, or an SSE
//	                      content_block_start event's content_block)
//	OpenAI chat           choices[0].message.tool_calls[].function.name (buffered), or a
//	                      streamed delta.tool_calls[].function.name chunk
//	OpenAI Responses      output[].{type: "function_call", name}    (buffered); not read from
//	                      an SSE stream — Responses event names and nesting are not the same as
//	                      Anthropic's and this proxy does not splice that transport (see
//	                      proxy.go's own comment where it tests schema.IsResponses(wire)), so a streamed
//	                      Responses tool call reads as if the response had none, same honest gap
//	                      as a sniffer-truncated Anthropic stream.
//
// Best-effort by construction: a sniffed stream only has a bounded head+tail window (see
// sniffer), so a tool_use far into a very long response can fall outside it. That under-reports
// own_tool, it never invents one.
func ToolUseNames(sse bool, body []byte) []string {
	if !sse {
		return bufferedToolUseNames(body)
	}
	var names []string
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		// Anthropic: content_block_start carries the block once, at its start.
		if cb := gjson.Get(payload, "content_block"); cb.Get("type").String() == "tool_use" {
			if n := cb.Get("name").String(); n != "" {
				names = append(names, n)
			}
			continue
		}
		// OpenAI chat: a streamed tool call's NAME arrives once, on the chunk that opens it;
		// later chunks for the same call carry only an arguments delta with no name.
		for _, tc := range gjson.Get(payload, "choices.0.delta.tool_calls").Array() {
			if n := tc.Get("function.name").String(); n != "" {
				names = append(names, n)
			}
		}
	}
	return names
}

// bufferedToolUseNames reads tool_use/function-call names off a whole, non-streamed response
// body (or a sniffed window of one — see ToolUseNames).
func bufferedToolUseNames(body []byte) []string {
	var names []string
	// The body names no dialect, so all three are read; a body matches only one of them. A
	// Responses custom tool call is not counted, as before.
	for _, wire := range []string{schema.WireAnthropic, schema.WireOpenAI, schema.WireResponses} {
		for _, c := range schema.OutputCalls(wire, body) {
			if !c.Custom && c.Name != "" {
				names = append(names, c.Name)
			}
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
// ToolUseNames reports last (and the wait tool of the row). A buffered response of any
// dialect carries the input whole. A streamed Anthropic response carries it as input_json_delta
// pieces, joined here per content block. A streamed OpenAI response is not read: that is a gap,
// and the value stays unstated rather than guessed. The caller skips a spliced window, whose last
// call may be missing.
func LastToolParams(sse bool, body []byte) ToolParams {
	if !sse {
		var last *schema.OutputCall
		calls := []schema.OutputCall{}
		for _, wire := range []string{schema.WireAnthropic, schema.WireOpenAI, schema.WireResponses} {
			calls = append(calls, schema.OutputCalls(wire, body)...)
		}
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
	var (
		cur   = -1 // content-block index of the last named tool_use
		input strings.Builder
		start string // the block's input as content_block_start gave it, "{}" when streamed
	)
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		if cb := gjson.Get(payload, "content_block"); cb.Get("type").String() == "tool_use" {
			if cb.Get("name").String() != "" {
				cur = int(gjson.Get(payload, "index").Int())
				input.Reset()
				start = cb.Get("input").Raw
			}
			continue
		}
		if cur >= 0 && gjson.Get(payload, "delta.type").String() == "input_json_delta" &&
			int(gjson.Get(payload, "index").Int()) == cur {
			input.WriteString(gjson.Get(payload, "delta.partial_json").String())
		}
	}
	if cur < 0 {
		return ToolParams{}
	}
	if input.Len() > 0 {
		return ToolParamsOf(input.String())
	}
	return ToolParamsOf(start)
}

// Spawn is the subagent calls one response made: N of them, and the subagent_type of the last that
// stated one. Only the type is read from their input, never the prompt.
type Spawn struct {
	N    int
	Type string
}

// spawnClient is the client whose subagent tools Spawns reads. Spawns has no request at hand, so
// it takes the zero Client, which gets the fallback rule of every client: Claude Code's Agent and
// Task (agent.Client.IsSubagentTool).
var spawnClient agent.Client

// Spawns counts the subagent calls of a response and reads the subagent_type of the last one.
// A streamed Anthropic response joins each call's input_json_delta pieces. Other streams are not read.
func Spawns(sse bool, body []byte) Spawn {
	var sp Spawn
	typeOf := func(input string) string {
		if t := gjson.Get(input, spawnClient.SubagentTypeField()); t.Type == gjson.String && len(t.Str) <= 64 {
			return t.Str
		}
		return ""
	}
	if !sse {
		for _, wire := range []string{schema.WireAnthropic, schema.WireOpenAI, schema.WireResponses} {
			for _, c := range schema.OutputCalls(wire, body) {
				if !c.Custom && spawnClient.IsSubagentTool(c.Name) {
					sp.N++
					if t := typeOf(c.Args); t != "" {
						sp.Type = t
					}
				}
			}
		}
		return sp
	}
	inputs := map[int]*strings.Builder{}
	var order []int
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		idx := int(gjson.Get(payload, "index").Int())
		if cb := gjson.Get(payload, "content_block"); cb.Get("type").String() == "tool_use" {
			if spawnClient.IsSubagentTool(cb.Get("name").String()) {
				inputs[idx] = &strings.Builder{}
				order = append(order, idx)
				if raw := cb.Get("input").Raw; raw != "" && raw != "{}" {
					inputs[idx].WriteString(raw)
				}
			}
			continue
		}
		if b := inputs[idx]; b != nil && gjson.Get(payload, "delta.type").String() == "input_json_delta" {
			b.WriteString(gjson.Get(payload, "delta.partial_json").String())
		}
	}
	for _, i := range order {
		sp.N++
		if t := typeOf(inputs[i].String()); t != "" {
			sp.Type = t
		}
	}
	return sp
}
