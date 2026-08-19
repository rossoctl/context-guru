package schema

import (
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

func call(id, name, args string) schemas.ChatMessage {
	m := schemas.ChatMessage{Role: schemas.ChatMessageRoleAssistant,
		ChatAssistantMessage: &schemas.ChatAssistantMessage{}}
	tc := schemas.ChatAssistantMessageToolCall{
		Function: schemas.ChatAssistantMessageToolCallFunction{Arguments: args},
	}
	if id != "" {
		tc.ID = &id
	}
	if name != "" {
		tc.Function.Name = &name
	}
	m.ChatAssistantMessage.ToolCalls = append(m.ChatAssistantMessage.ToolCalls, tc)
	return m
}

func result(id, text string) schemas.ChatMessage {
	m := schemas.ChatMessage{Role: schemas.ChatMessageRoleTool}
	SetMessageText(&m, text)
	if id != "" {
		m.ChatToolMessage = &schemas.ChatToolMessage{ToolCallID: &id}
	}
	return m
}

func TestToolCallsPairing(t *testing.T) {
	tests := []struct {
		name  string
		input []schemas.ChatMessage
		want  map[int]string // index -> Command()
	}{{
		name:  "one call, one result",
		input: []schemas.ChatMessage{call("t1", "Bash", `{"command":"rg -n foo"}`), result("t1", "a.go:1:foo")},
		want:  map[int]string{1: "rg -n foo"},
	}, {
		name: "parallel calls in one assistant turn, results in id order",
		input: []schemas.ChatMessage{
			{Role: schemas.ChatMessageRoleAssistant, ChatAssistantMessage: &schemas.ChatAssistantMessage{
				ToolCalls: append(call("t1", "Bash", `{"command":"ls -1 /x"}`).ChatAssistantMessage.ToolCalls,
					call("t2", "Read", `{"file_path":"/x/a.go"}`).ChatAssistantMessage.ToolCalls...)}},
			result("t2", "1\tpackage x"), result("t1", "/x/a.go"),
		},
		want: map[int]string{1: `Read {"file_path":"/x/a.go"}`, 2: "ls -1 /x"},
	}, {
		name:  "unmatched id: no entry, caller falls back to shape",
		input: []schemas.ChatMessage{call("t1", "Bash", `{"command":"ls"}`), result("t9", "x")},
		want:  map[int]string{},
	}, {
		name:  "result with no id at all",
		input: []schemas.ChatMessage{call("t1", "Bash", `{"command":"ls"}`), result("", "x")},
		want:  map[int]string{},
	}, {
		name:  "result with no preceding call",
		input: []schemas.ChatMessage{result("t1", "x"), call("t1", "Bash", `{"command":"ls"}`)},
		want:  map[int]string{},
	}, {
		name:  "call with no id is not indexable",
		input: []schemas.ChatMessage{call("", "Bash", `{"command":"ls"}`), result("t1", "x")},
		want:  map[int]string{},
	}, {
		name:  "no name, no args",
		input: []schemas.ChatMessage{call("t1", "", ""), result("t1", "x")},
		want:  map[int]string{1: ""},
	}, {
		name:  "empty argument object renders as the bare tool name",
		input: []schemas.ChatMessage{call("t1", "TodoRead", `{}`), result("t1", "x")},
		want:  map[int]string{1: "TodoRead"},
	}, {
		name:  "non-JSON arguments do not break the render",
		input: []schemas.ChatMessage{call("t1", "Bash", `{"command":`), result("t1", "x")},
		want:  map[int]string{1: `Bash {"command":`},
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ToolCalls(&schemas.BifrostChatRequest{Input: tc.input})
			if len(got) != len(tc.want) {
				t.Fatalf("paired %v, want %v", render(got), tc.want)
			}
			for i, want := range tc.want {
				if c := got[i].Command(); c != want {
					t.Errorf("index %d: command %q, want %q", i, c, want)
				}
			}
		})
	}
	if ToolCalls(nil) != nil {
		t.Error("nil request must pair nothing")
	}
}

func render(m map[int]ToolCall) map[int]string {
	out := map[int]string{}
	for i, tc := range m {
		out[i] = tc.Command()
	}
	return out
}
