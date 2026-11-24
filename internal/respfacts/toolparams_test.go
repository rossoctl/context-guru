package respfacts

import (
	"testing"
)

func TestToolParamsOf(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  ToolParams
	}{
		{"both", `{"command":"make","timeout":600000,"run_in_background":true}`, ToolParams{600000, true, true, true}},
		{"timeout only", `{"command":"ls","timeout":120000}`, ToolParams{TimeoutMs: 120000, HasTimeout: true}},
		{"background false is stated", `{"run_in_background":false}`, ToolParams{HasBackground: true}},
		{"neither", `{"file_path":"/x"}`, ToolParams{}},
		{"timeout as a string is not a number", `{"timeout":"600000"}`, ToolParams{}},
		{"zero or negative timeout is not stated", `{"timeout":0}`, ToolParams{}},
		{"background as a string", `{"run_in_background":"true"}`, ToolParams{}},
		{"not JSON", `{"timeout":`, ToolParams{}},
		{"empty", ``, ToolParams{}},
	} {
		if got := ToolParamsOf(tc.input); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestResponseLastToolParamsBuffered(t *testing.T) {
	anth := []byte(`{"content":[
		{"type":"tool_use","id":"a","name":"Bash","input":{"command":"x","timeout":1000}},
		{"type":"tool_use","id":"b","name":"Bash","input":{"command":"y","timeout":600000,"run_in_background":true}}
	],"stop_reason":"tool_use"}`)
	if got := LastToolParams(false, anth); got != (ToolParams{600000, true, true, true}) {
		t.Errorf("anthropic: %+v", got)
	}
	oa := []byte(`{"choices":[{"message":{"tool_calls":[{"id":"c","type":"function","function":{"name":"shell","arguments":"{\"timeout\":30000}"}}]}}]}`)
	if got := LastToolParams(false, oa); got != (ToolParams{TimeoutMs: 30000, HasTimeout: true}) {
		t.Errorf("openai chat: %+v", got)
	}
	none := []byte(`{"content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}`)
	if got := LastToolParams(false, none); got != (ToolParams{}) {
		t.Errorf("no call: %+v", got)
	}
}

func TestResponseLastToolParamsStreamed(t *testing.T) {
	ev := func(s string) string { return "data: " + s + "\n\n" }
	stream := ev(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"a","name":"Read","input":{}}}`) +
		ev(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"timeout\":5}"}}`) +
		ev(`{"type":"content_block_stop","index":0}`) +
		ev(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"b","name":"Bash","input":{}}}`) +
		ev(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"make all\", \"time"}}`) +
		ev(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"out\":900000,\"run_in_back"}}`) +
		ev(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"ground\":true}"}}`) +
		ev(`{"type":"content_block_stop","index":1}`) +
		ev(`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`)
	if got := LastToolParams(true, []byte(stream)); got != (ToolParams{900000, true, true, true}) {
		t.Errorf("streamed: %+v", got)
	}
	// A call with no input at all: unstated, not zero.
	empty := ev(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"a","name":"TaskList","input":{}}}`)
	if got := LastToolParams(true, []byte(empty)); got != (ToolParams{}) {
		t.Errorf("empty input: %+v", got)
	}
	// An OpenAI stream is not read: a gap, never a guess.
	oa := ev(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"shell","arguments":"{\"timeout\":5}"}}]}}]}`)
	if got := LastToolParams(true, []byte(oa)); got != (ToolParams{}) {
		t.Errorf("openai stream: %+v", got)
	}
}

func TestToolParamsKeepNothingElse(t *testing.T) {
	got := ToolParamsOf(`{"command":"curl -H 'Authorization: sk-secret'","timeout":1000}`)
	if got != (ToolParams{TimeoutMs: 1000, HasTimeout: true}) {
		t.Fatalf("got %+v", got)
	}
}

func TestResponseSpawns(t *testing.T) {
	buf := []byte(`{"content":[
		{"type":"tool_use","id":"a","name":"Agent","input":{"subagent_type":"Explore","prompt":"secret text"}},
		{"type":"tool_use","id":"b","name":"Agent","input":{"subagent_type":"my-reviewer","prompt":"x"}},
		{"type":"tool_use","id":"c","name":"Bash","input":{"command":"ls"}}]}`)
	if got := Spawns(false, buf); got != (Spawn{N: 2, Type: "my-reviewer"}) {
		t.Errorf("buffered: %+v", got)
	}
	ev := func(s string) string { return "data: " + s + "\n\n" }
	stream := ev(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"a","name":"Task","input":{}}}`) +
		ev(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"subagent_type\":\"Pla"}}`) +
		ev(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"n\",\"prompt\":\"p\"}"}}`) +
		ev(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"b","name":"Bash","input":{}}}`)
	if got := Spawns(true, []byte(stream)); got != (Spawn{N: 1, Type: "Plan"}) {
		t.Errorf("streamed: %+v", got)
	}
}
