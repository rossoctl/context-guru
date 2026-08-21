package offload

import (
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/schema"
)

// PARALLEL TOOL CALLS. Live traffic that the provider rejected carried ONE assistant message with
// TWO tool_use blocks (see the capture digest in docs/experiments/loca/iter011/results.md). Anthropic
// requires every result for a parallel call to arrive in the message immediately after, and bifrost
// represents each result as its own ChatMessageRoleTool message. So the question is whether a
// perfectly ordinary parallel exchange is considered valid by our own validator -- and if not,
// whether that is the validator being wrong or the wire shape being wrong.
func TestValidateShapeOnParallelToolCalls(t *testing.T) {
	a, b := "call_a", "call_b"
	nm := "Read"
	msgs := []bschemas.ChatMessage{
		{Role: bschemas.ChatMessageRoleUser,
			Content: &bschemas.ChatMessageContent{ContentStr: sp("do two things")}},
		{Role: bschemas.ChatMessageRoleAssistant,
			Content: &bschemas.ChatMessageContent{ContentStr: sp("calling both")},
			ChatAssistantMessage: &bschemas.ChatAssistantMessage{
				ToolCalls: []bschemas.ChatAssistantMessageToolCall{
					{ID: &a, Function: bschemas.ChatAssistantMessageToolCallFunction{Name: &nm, Arguments: `{}`}},
					{ID: &b, Function: bschemas.ChatAssistantMessageToolCallFunction{Name: &nm, Arguments: `{}`}},
				}}},
		{Role: bschemas.ChatMessageRoleTool,
			Content:         &bschemas.ChatMessageContent{ContentStr: sp("result a")},
			ChatToolMessage: &bschemas.ChatToolMessage{ToolCallID: &a}},
		{Role: bschemas.ChatMessageRoleTool,
			Content:         &bschemas.ChatMessageContent{ContentStr: sp("result b")},
			ChatToolMessage: &bschemas.ChatToolMessage{ToolCallID: &b}},
	}
	v := schema.ValidateShape(msgs)
	for _, x := range v {
		t.Logf("violation: %s", x)
	}
	if len(v) != 0 {
		t.Errorf("ValidateShape rejects an ordinary PARALLEL tool exchange (%d violations). "+
			"Either the rule only inspects msgs[i+1] for a single result -- so it cannot see the "+
			"second result one message later -- or the wire shape is genuinely wrong. This is the "+
			"shape live traffic was rejected on.", len(v))
	}
}
