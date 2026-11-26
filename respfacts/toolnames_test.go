package respfacts

import "testing"

// TestResponseToolUseNames covers issue #424's "every provider" requirement for the
// waitcause.ClassifyWaitingFor input: Anthropic Messages, OpenAI chat, and OpenAI Responses,
// buffered and (where this proxy splices/sniffs it) streamed.
func TestResponseToolUseNamesAnthropicBuffered(t *testing.T) {
	body := []byte(`{"id":"msg_1","content":[
		{"type":"text","text":"let me check"},
		{"type":"tool_use","id":"call1","name":"Read","input":{"path":"x"}},
		{"type":"tool_use","id":"call2","name":"Bash","input":{"command":"ls"}}
	],"stop_reason":"tool_use"}`)
	got := ToolUseNames(false, body)
	if len(got) != 2 || got[0] != "Read" || got[1] != "Bash" {
		t.Fatalf("got %v, want [Read Bash]", got)
	}
}

func TestResponseToolUseNamesAnthropicNone(t *testing.T) {
	body := []byte(`{"id":"msg_1","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}`)
	if got := ToolUseNames(false, body); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

func TestResponseToolUseNamesAnthropicSSE(t *testing.T) {
	stream := []byte(
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5}}}\n\n" +
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call1\",\"name\":\"Bash\"}}\n\n" +
			"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":3}}\n\n")
	got := ToolUseNames(true, stream)
	if len(got) != 1 || got[0] != "Bash" {
		t.Fatalf("got %v, want [Bash]", got)
	}
}

func TestResponseToolUseNamesOpenAIChatBuffered(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant",
		"tool_calls":[{"id":"call1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
		"finish_reason":"tool_calls"}]}`)
	got := ToolUseNames(false, body)
	if len(got) != 1 || got[0] != "lookup" {
		t.Fatalf("got %v, want [lookup]", got)
	}
}

func TestResponseToolUseNamesOpenAIChatSSE(t *testing.T) {
	stream := []byte(
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call1\",\"function\":{\"name\":\"lookup\",\"arguments\":\"\"}}]}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{}\"}}]}}]}\n\n" +
			"data: [DONE]\n\n")
	got := ToolUseNames(true, stream)
	if len(got) != 1 || got[0] != "lookup" {
		t.Fatalf("got %v, want [lookup]", got)
	}
}

func TestResponseToolUseNamesResponsesBuffered(t *testing.T) {
	body := []byte(`{"output":[
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]},
		{"type":"function_call","call_id":"call1","name":"lookup","arguments":"{}"}
	]}`)
	got := ToolUseNames(false, body)
	if len(got) != 1 || got[0] != "lookup" {
		t.Fatalf("got %v, want [lookup]", got)
	}
}

// A streamed Responses call is named, from the event that opens it or from the terminal event (#1484).
func TestToolUseNamesResponsesStream(t *testing.T) {
	ev := func(s string) string { return "data: " + s + "\n\n" }
	opened := ev(`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"c","name":"shell","arguments":""}}`) +
		ev(`{"type":"response.output_item.added","output_index":1,"item":{"type":"custom_tool_call","call_id":"d","name":"apply_patch","input":""}}`)
	if got := ToolUseNames(true, []byte(opened)); len(got) != 1 || got[0] != "shell" {
		t.Errorf("opened: %v, want [shell] (a custom tool call is not counted)", got)
	}
	done := ev(`{"type":"response.completed","response":{"output":[{"type":"function_call","call_id":"c","name":"read","arguments":"{}"}]}}`)
	if got := ToolUseNames(true, []byte(done)); len(got) != 1 || got[0] != "read" {
		t.Errorf("completed: %v, want [read]", got)
	}
}

// A stream with CRLF framing reads the same as one with LF (#1499).
func TestToolUseNamesCRLFStream(t *testing.T) {
	stream := "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"a\",\"name\":\"Bash\"}}\r\n\r\n"
	if got := ToolUseNames(true, []byte(stream)); len(got) != 1 || got[0] != "Bash" {
		t.Errorf("got %v", got)
	}
}
