package offload

import (
	"encoding/json"
	"fmt"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/schema"
)

// callMsgs renders one call as the assistant tool_use + the tool result, the pairing
// schema.ToolCalls reads. Appends to msgs and returns it.
func callMsgs(msgs []bschemas.ChatMessage, tool string, args any, out string) []bschemas.ChatMessage {
	id := fmt.Sprintf("toolu_%d", len(msgs))
	var raw string
	switch a := args.(type) {
	case json.RawMessage:
		raw = string(a)
	case string:
		raw = a
	default:
		b, _ := json.Marshal(a)
		raw = string(b)
	}
	name := tool
	msgs = append(msgs, bschemas.ChatMessage{
		Role: bschemas.ChatMessageRoleAssistant,
		ChatAssistantMessage: &bschemas.ChatAssistantMessage{ToolCalls: []bschemas.ChatAssistantMessageToolCall{{
			ID:       &id,
			Function: bschemas.ChatAssistantMessageToolCallFunction{Name: &name, Arguments: raw},
		}}},
	})
	m := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleTool,
		ChatToolMessage: &bschemas.ChatToolMessage{ToolCallID: &id}}
	schema.SetMessageText(&m, out)
	return append(msgs, m)
}

// asstMsg is an assistant message that issued one tool call, i.e. the opening half
// of a step. The tool call is carried in the OpenAI-shaped ToolCalls field, which is
// what serializeStep renders as <call tool="...">.
func asstMsg(text, tool, args string) bschemas.ChatMessage {
	txt, name := text, tool
	return bschemas.ChatMessage{
		Role:    bschemas.ChatMessageRoleAssistant,
		Content: &bschemas.ChatMessageContent{ContentStr: &txt},
		ChatAssistantMessage: &bschemas.ChatAssistantMessage{
			ToolCalls: []bschemas.ChatAssistantMessageToolCall{{
				Function: bschemas.ChatAssistantMessageToolCallFunction{Name: &name, Arguments: args},
			}},
		},
	}
}
