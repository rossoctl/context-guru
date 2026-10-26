package offload

import (
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
)

// #723 (a). The fresh span ends before the LAST tool exchange: the assistant turn that asked for the
// tool and its results stay verbatim. No tool result at or after start means no change.
func TestRetreatPastLastToolExchange(t *testing.T) {
	tool := func(id string) bschemas.ChatMessage {
		m := caMsg(bschemas.ChatMessageRoleTool, "out "+id)
		m.ChatToolMessage = &bschemas.ChatToolMessage{ToolCallID: &id}
		return m
	}
	u := func(s string) bschemas.ChatMessage { return caMsg(bschemas.ChatMessageRoleUser, s) }
	a := func(s string) bschemas.ChatMessage { return caMsg(bschemas.ChatMessageRoleAssistant, s) }
	// 0 u, 1 a(call), 2 tool, 3 a, 4 u, 5 a(call), 6 tool, 7 a, 8 u, 9 a, 10 u
	msgs := []bschemas.ChatMessage{u("task"), a("call 1"), tool("t1"), a("read a"), u("read b"),
		a("call 2"), tool("t2"), a("read b"), u("one word"), a("ten"), u("again")}
	if got := retreatPastLastToolExchange(msgs, nil, 1, 9); got != 5 {
		t.Fatalf("end = %d, want 5 (the assistant turn that called the last tool)", got)
	}
	if got := lastKeptToolResultID(msgs, 5); got != "t2" {
		t.Fatalf("anchor = %q, want t2", got)
	}
	// The last exchange is already in the kept tail: nothing to move.
	if got := retreatPastLastToolExchange(msgs, nil, 1, 4); got != 4 {
		t.Fatalf("end = %d, want 4 unchanged", got)
	}
	// No tool result after start.
	plain := []bschemas.ChatMessage{u("task"), a("x"), u("y"), a("z"), u("w")}
	if got := retreatPastLastToolExchange(plain, nil, 1, 3); got != 3 {
		t.Fatalf("end = %d, want 3 unchanged with no tool exchange", got)
	}
	// The only exchange starts at the head: nothing would be left to summarize.
	head := []bschemas.ChatMessage{u("task"), a("call"), tool("t"), a("done"), u("go")}
	if got := retreatPastLastToolExchange(head, nil, 1, 3); got != 3 {
		t.Fatalf("end = %d, want 3 unchanged when the exchange starts at the span start", got)
	}
}
