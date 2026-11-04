package schema

import (
	"reflect"
	"testing"
)

// One table over the 3 formats: ids, names and arguments, in order, and nothing that is not a call.
func TestOutputCalls(t *testing.T) {
	cases := []struct {
		wire, msg string
		want      []OutputCall
	}{
		{WireAnthropic, `{"content":[{"type":"text","text":"x"},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}},` +
			`{"type":"tool_use","id":"t2","name":"Read","input":{}}]}`,
			[]OutputCall{{ID: "t1", Name: "Bash", Args: `{"command":"ls"}`}, {ID: "t2", Name: "Read", Args: `{}`}}},
		{WireResponses, `{"output":[{"type":"reasoning"},{"type":"function_call","call_id":"c1","name":"f","arguments":"{\"a\":1}"},` +
			`{"type":"message"},{"type":"custom_tool_call","call_id":"c2","name":"g","input":"free text"}]}`,
			[]OutputCall{{ID: "c1", Name: "f", Args: `{"a":1}`}, {ID: "c2", Name: "g", Args: "free text", Custom: true}}},
		{WireOpenAI, `{"choices":[{"message":{"tool_calls":[{"id":"x1","type":"function","function":{"name":"h","arguments":"{}"}},` +
			`{"id":"x2","type":"function","function":{"name":"i","arguments":"{\"b\":2}"}}]}}]}`,
			[]OutputCall{{ID: "x1", Name: "h", Args: `{}`}, {ID: "x2", Name: "i", Args: `{"b":2}`}}},
		{WireOpenAI, `{"choices":[{"message":{"content":"no calls"}}]}`, nil},
	}
	for _, c := range cases {
		if got := OutputCalls(c.wire, []byte(c.msg)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %+v, want %+v", c.wire, got, c.want)
		}
	}
}
