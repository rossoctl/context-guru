package adjudicate

import (
	"testing"

	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/schema"
)

// A Responses custom_tool_call to our tool is answered like a function_call, on the request side and
// in the response ids, as expand answers it.
func TestAnswerStrayCallsAnswersACustomToolCall(t *testing.T) {
	body := []byte(`{"input":[{"type":"custom_tool_call","call_id":"c1","name":"` + ToolName + `","input":"{}"},
{"type":"custom_tool_call_output","call_id":"c1","output":"tool not found"}]}`)
	out, n := AnswerStrayCalls(schema.WireResponses, body)
	if n != 1 || gjson.GetBytes(out, "input.1.output").String() != StrayAnswer {
		t.Fatalf("answered %d: %s", n, out)
	}
}

func TestResponseCallIDsReadsACustomToolCall(t *testing.T) {
	resp := []byte(`{"output":[{"type":"custom_tool_call","call_id":"c1","name":"` + ToolName + `","input":"{}"},
{"type":"function_call","call_id":"f1","name":"` + ToolName + `","arguments":"{}"},
{"type":"function_call","call_id":"f2","name":"Other","arguments":"{}"}]}`)
	ids := ResponseCallIDs(schema.WireResponses, resp)
	if len(ids) != 2 || ids[0] != "c1" || ids[1] != "f1" {
		t.Errorf("ids = %v, want [c1 f1]", ids)
	}
}
