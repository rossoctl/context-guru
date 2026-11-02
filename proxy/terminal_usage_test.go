package proxy

import "testing"

// #857: a spliced sniff window may hide the true last tool call, so the usage says so.
func TestToolUseIncompleteFollowsSpliced(t *testing.T) {
	body := []byte(`{"id":"m","content":[{"type":"tool_use","id":"c","name":"StructuredOutput","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)
	if u, _, _ := responseUsageWhy("application/json", body, false); u.ToolUseIncomplete {
		t.Error("a whole response must not be marked incomplete")
	}
	if u, _, _ := responseUsageWhy("application/json", body, true); !u.ToolUseIncomplete {
		t.Error("a spliced window must be marked incomplete")
	}
}
