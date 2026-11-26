package schema

import "testing"

type want struct {
	ID, Name, Args string
	Custom         bool
	At             int
}

func TestRequestCalls(t *testing.T) {
	anth := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"text","text":"x"},{"type":"tool_use","id":"a","name":"n","input":{"id":"h1"}},{"type":"tool_use","name":"noid","input":{}}]},
{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"r"},{"type":"tool_result","content":"orphan"}]}]}`)
	chat := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","tool_calls":[{"id":"c","function":{"name":"n","arguments":"{\"id\":\"h2\"}"}},{"function":{"name":"noid"}}]},{"role":"tool","tool_call_id":"c","content":"r"},{"role":"tool","content":"orphan"}]}`)
	resp := []byte(`{"input":[{"type":"message","role":"user"},{"type":"function_call","call_id":"f","name":"n","arguments":"{\"id\":\"h3\"}"},{"type":"custom_tool_call","call_id":"u","name":"n","input":"raw"},{"type":"function_call_output","call_id":"f","output":"r"},{"type":"custom_tool_call_output","call_id":"u","output":"r"},{"type":"function_call_output","output":"orphan"}]}`)
	cases := []struct {
		wire    string
		body    []byte
		calls   []want
		results []RequestResult
		paths   []string
	}{
		{WireAnthropic, anth, []want{{ID: "a", Name: "n", Args: `{"id":"h1"}`, At: 1}}, []RequestResult{{CallID: "a", At: 2, Block: 0}}, []string{"messages.2.content.0"}},
		{WireOpenAI, chat, []want{{ID: "c", Name: "n", Args: `{"id":"h2"}`, At: 1}}, []RequestResult{{CallID: "c", At: 2, Block: -1}}, []string{"messages.2"}},
		{WireResponses, resp, []want{{ID: "f", Name: "n", Args: `{"id":"h3"}`, At: 1}, {ID: "u", Name: "n", Args: "raw", Custom: true, At: 2}},
			[]RequestResult{{CallID: "f", At: 3, Block: -1}, {CallID: "u", At: 4, Block: -1}}, []string{"input.3", "input.4"}},
	}
	for _, c := range cases {
		got := RequestCalls(c.wire, c.body)
		if len(got) != len(c.calls) {
			t.Fatalf("%s: calls = %+v", c.wire, got)
		}
		for i := range got {
			g := want{got[i].ID, got[i].Name, got[i].Args(), got[i].Custom, got[i].At}
			if g != c.calls[i] {
				t.Errorf("%s: call %d = %+v, want %+v", c.wire, i, g, c.calls[i])
			}
		}
		res := RequestResults(c.wire, c.body)
		if len(res) != len(c.results) {
			t.Fatalf("%s: results = %+v", c.wire, res)
		}
		for i := range res {
			if res[i].CallID != c.results[i].CallID || res[i].At != c.results[i].At || res[i].Block != c.results[i].Block {
				t.Errorf("%s: result %d = %+v, want %+v", c.wire, i, res[i], c.results[i])
			}
			if p := res[i].Path(c.wire); p != c.paths[i] {
				t.Errorf("%s: path %d = %q, want %q", c.wire, i, p, c.paths[i])
			}
		}
	}
}
