package schema

import (
	"encoding/json"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
)

// ToolCall is the call that PRODUCED a tool result: the tool's name and its raw
// input arguments (a JSON object, as the wire carried it).
//
// It exists because the premise "a proxy never sees the command that produced an
// output" is false. Both dialects put the call and its result in the same request,
// joined by an id — Anthropic as an assistant `tool_use` block referenced by the
// `tool_result`'s `tool_use_id`, OpenAI as an assistant `tool_calls[]` entry
// referenced by the tool message's `tool_call_id` — so a filter can key on the
// command (`rg -n foo`, `Read /x/y.go`) and not only on the output's shape.
//
// Anthropic's `tool_use` blocks are not modelled by bifrost's chat schema, so the
// host's normalize step lifts them into ToolCalls (see apply.attachToolUse); this
// helper then reads one uniform shape for both dialects.
type ToolCall struct {
	Name string // "Bash", "Read", "Grep", …
	Args string // raw JSON arguments, e.g. `{"command":"rg -n foo"}`; "" when absent
}

// Command renders the call as a single command-ish line: the `command` string for a
// shell tool, else the tool name followed by its arguments. It is the string a
// command-keyed filter selector matches against.
func (t ToolCall) Command() string {
	if cmd := t.argString("command"); cmd != "" {
		return cmd
	}
	if t.Args == "" || t.Args == "{}" {
		return t.Name
	}
	return t.Name + " " + t.Args
}

// argString returns a top-level string argument, or "" when absent/not a string.
// Only the fields a selector needs are decoded, so a 200 KB Write payload is not
// unmarshalled into a map to answer "is this a shell command".
func (t ToolCall) argString(key string) string {
	if !strings.Contains(t.Args, `"`+key+`"`) {
		return ""
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(t.Args), &obj) != nil {
		return ""
	}
	var s string
	if json.Unmarshal(obj[key], &s) != nil {
		return ""
	}
	return s
}

// ToolCalls pairs every tool-result message in req with the call that produced it,
// keyed by the message's index in req.Input. A message whose id matches no call —
// unmatched, missing, or a tool result with no preceding tool_use at all — simply
// has no entry, so a caller reads the zero ToolCall and falls back to whatever it
// did before. Ids are collected as the transcript is walked, so a result can only
// pair with a call that PRECEDES it, and several results in one message (Anthropic
// packs them into one user message; normalize splits them) each pair on their own id.
func ToolCalls(req *schemas.BifrostChatRequest) map[int]ToolCall {
	if req == nil {
		return nil
	}
	var byID map[string]ToolCall
	out := map[int]ToolCall{}
	for i, m := range req.Input {
		if a := m.ChatAssistantMessage; a != nil {
			for _, tc := range a.ToolCalls {
				if tc.ID == nil || *tc.ID == "" {
					continue
				}
				name := ""
				if tc.Function.Name != nil {
					name = *tc.Function.Name
				}
				if byID == nil {
					byID = map[string]ToolCall{}
				}
				byID[*tc.ID] = ToolCall{Name: name, Args: tc.Function.Arguments}
			}
		}
		if m.Role != schemas.ChatMessageRoleTool || m.ChatToolMessage == nil || m.ChatToolMessage.ToolCallID == nil {
			continue
		}
		if tc, ok := byID[*m.ChatToolMessage.ToolCallID]; ok {
			out[i] = tc
		}
	}
	return out
}
