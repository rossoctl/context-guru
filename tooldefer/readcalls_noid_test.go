package tooldefer

import (
	"testing"

	"github.com/tidwall/gjson"
)

// A tool_use with no id and a tool_result with no tool_use_id are not a pair. Matched on the empty
// string, an unrelated failed result read as "the deferred tool broke".
func TestReadCallsIgnoresAnEmptyID(t *testing.T) {
	body := []byte(`{"messages":[
{"role":"assistant","content":[{"type":"tool_use","name":"mcp__a__b","input":{}}]},
{"role":"user","content":[{"type":"tool_result","is_error":true,"content":"No such tool available: x"}]}]}`)
	broke, direct := ReadCalls(body, map[string]bool{"mcp__a__b": true})
	if broke || len(direct) != 0 {
		t.Errorf("broke=%v direct=%v, want false and none", broke, direct)
	}
}

// The same pair with an id still reads as a break.
func TestReadCallsStillReadsABreak(t *testing.T) {
	body := []byte(`{"messages":[
{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"mcp__a__b","input":{}}]},
{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"No such tool available: x"}]}]}`)
	if broke, _ := ReadCalls(body, map[string]bool{"mcp__a__b": true}); !broke {
		t.Error("a failed result for a deferred tool's call is a break")
	}
}

// Our search's calls on Responses are read in both call forms, as expand and adjudicate read theirs.
func TestRepairSearchResultsReadsACustomToolCall(t *testing.T) {
	body := []byte(`{"input":[{"type":"custom_tool_call","call_id":"c1","name":"` + ClientSearchName + `","input":"{\"names\":[\"a\"]}"},
{"type":"custom_tool_call_output","call_id":"c1","output":"no such tool"}]}`)
	out, asked := repairSearchResults("responses", body, map[string]bool{"a": true}, nil)
	if len(asked) != 1 || asked[0] != "a" || gjson.GetBytes(out, "input.1.output").String() != SearchAnswer([]string{"a"}, map[string]bool{"a": true}) {
		t.Errorf("asked=%v out=%s", asked, out)
	}
}
