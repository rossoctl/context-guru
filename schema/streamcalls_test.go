package schema

import (
	"reflect"
	"strings"
	"testing"
)

func ev(payloads ...string) []byte {
	var b strings.Builder
	for _, p := range payloads {
		b.WriteString("data: " + p + "\n\n")
	}
	return []byte(b.String())
}

func TestStreamCallsAnthropic(t *testing.T) {
	got := StreamCalls(ev(
		`{"type":"message_start","message":{"usage":{"input_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"tool_use"}}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"c1","name":"Bash","input":{}}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"timeout\":"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"5000}"}}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"c2","name":"Agent","input":{"subagent_type":"Plan"}}}`,
		`{"type":"content_block_start","index":3,"content_block":{"type":"server_tool_use","id":"s1","name":"web_search"}}`,
	))
	want := []OutputCall{
		{ID: "c1", Name: "Bash", Args: `{"timeout":5000}`},
		// No delta came, so the arguments are the start input.
		{ID: "c2", Name: "Agent", Args: `{"subagent_type":"Plan"}`},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// A start input and delta pieces are not added together: that would be invalid JSON.
func TestStreamCallsAnthropicDeltasReplaceTheStartInput(t *testing.T) {
	got := StreamCalls(ev(
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"c","name":"Agent","input":{"x":1}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"subagent_type\":\"Plan\"}"}}`,
	))
	if len(got) != 1 || got[0].Args != `{"subagent_type":"Plan"}` {
		t.Errorf("got %+v", got)
	}
}

func TestStreamCallsChat(t *testing.T) {
	got := StreamCalls(ev(
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"a tool_calls word"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"lookup","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c2","function":{"name":"read","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		`[DONE]`,
	))
	want := []OutputCall{
		{ID: "c1", Name: "lookup", Args: `{"q":1}`},
		{ID: "c2", Name: "read", Args: `{}`},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// Some gateways give every parallel call index 0. A second name is a second call.
func TestStreamCallsChatRepeatedIndex(t *testing.T) {
	got := StreamCalls(ev(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"a","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c2","function":{"name":"b","arguments":"{}"}}]}}]}`,
	))
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" || got[1].ID != "c2" {
		t.Errorf("got %+v", got)
	}
}

func TestStreamCallsResponses(t *testing.T) {
	got := StreamCalls(ev(
		`{"type":"response.created","response":{"id":"r"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs"}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"c1","name":"shell","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"timeout"}`,
		`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"_ms\":9}"}`,
		`{"type":"response.output_item.added","output_index":2,"item":{"type":"custom_tool_call","call_id":"c2","name":"apply_patch","input":""}}`,
		`{"type":"response.custom_tool_call_input.done","output_index":2,"input":"*** Begin Patch"}`,
	))
	want := []OutputCall{
		{ID: "c1", Name: "shell", Args: `{"timeout_ms":9}`},
		{ID: "c2", Name: "apply_patch", Args: "*** Begin Patch", Custom: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// The terminal event holds the whole output. A window that lost the middle of the stream still
// reads every call from it, and its arguments win over the pieces.
func TestStreamCallsResponsesCompletedWins(t *testing.T) {
	got := StreamCalls(ev(
		`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"cut"}`,
		`{"type":"response.completed","response":{"output":[`+
			`{"type":"function_call","call_id":"c1","name":"shell","arguments":"{\"timeout\":7}"},`+
			`{"type":"message","content":[]},`+
			`{"type":"function_call","call_id":"c2","name":"read","arguments":"{}"}]}}`,
	))
	want := []OutputCall{
		{ID: "c1", Name: "shell", Args: `{"timeout":7}`},
		{ID: "c2", Name: "read", Args: `{}`},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// The stream rebuild and the whole-message reader agree on the same response.
func TestStreamCallsMatchOutputCalls(t *testing.T) {
	msg := []byte(`{"output":[{"type":"function_call","call_id":"c1","name":"shell","arguments":"{\"a\":1}"}]}`)
	stream := ev(`{"type":"response.completed","response":` + string(msg) + `}`)
	if got, want := StreamCalls(stream), OutputCalls(WireResponses, msg); !reflect.DeepEqual(got, want) {
		t.Errorf("stream %+v, message %+v", got, want)
	}
}

func TestStreamCallsOtherEvents(t *testing.T) {
	if got := StreamCalls(ev(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`, `not json tool_use`)); len(got) != 0 {
		t.Errorf("got %+v", got)
	}
}

// A gateway that sends the SAME name on every chunk of one call has one call, not many.
func TestStreamCallsChatSameNameRepeated(t *testing.T) {
	got := StreamCalls(ev(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"a","arguments":"{\"x\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"a","arguments":"1}"}}]}}]}`,
	))
	if len(got) != 1 || got[0].Name != "a" || got[0].Args != `{"x":1}` {
		t.Errorf("got %+v", got)
	}
}

// A closing event with empty arguments must not erase the pieces.
func TestStreamCallsResponsesEmptyDoneKeepsPieces(t *testing.T) {
	got := StreamCalls(ev(
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"c","name":"shell","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"a\":1}"}`,
		`{"type":"response.function_call_arguments.done","output_index":0,"arguments":""}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"custom_tool_call","call_id":"d","name":"patch","input":""}}`,
		`{"type":"response.custom_tool_call_input.delta","output_index":1,"delta":"text"}`,
		`{"type":"response.custom_tool_call_input.done","output_index":1,"input":""}`,
	))
	if len(got) != 2 || got[0].Args != `{"a":1}` || got[1].Args != "text" {
		t.Errorf("got %+v", got)
	}
}
