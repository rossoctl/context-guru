package adjudicate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// The tool must land LAST and be idempotent, because `tools` hashes before system and messages: a tool
// inserted anywhere else, or twice, invalidates the prompt-cache prefix from position zero.
func TestInjectIsByteStableAndIdempotent(t *testing.T) {
	body := []byte(`{"model":"m","tools":[{"name":"Read","description":"d","input_schema":{"type":"object"}}],` +
		`"messages":[{"role":"user","content":"go"}]}`)
	out, ok := Inject("anthropic", body)
	if !ok {
		t.Fatal("did not inject into a request that declares tools")
	}
	tools := gjson.GetBytes(out, "tools").Array()
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	if tools[0].Get("name").String() != "Read" {
		t.Error("the client's own tool moved; its order must be preserved exactly")
	}
	if tools[1].Get("name").String() != ToolName {
		t.Errorf("our tool is not last: %q", tools[1].Get("name").String())
	}
	// The schema must actually parse, or the provider rejects every request carrying it — on every
	// request, since this is injected unconditionally.
	var schema map[string]any
	if err := json.Unmarshal([]byte(tools[1].Get("input_schema").Raw), &schema); err != nil {
		t.Fatalf("input_schema is not valid JSON: %v", err)
	}
	// The verdict fields are extract.Verdict's JSON tags; that is what lets the existing parser read a
	// tool input unchanged, so a rename here silently reverts the fix to the text path.
	props := gjson.GetBytes(out, `tools.1.input_schema.properties.verdicts.items.properties`)
	for _, f := range []string{"i", "needed_by", "quote", "verdict"} {
		if !props.Get(f).Exists() {
			t.Errorf("schema is missing verdict field %q; extract.ParseVerdicts reads that name", f)
		}
	}
	again, ok2 := Inject("anthropic", out)
	if ok2 {
		t.Error("injected twice; a duplicated tool changes the prefix on every turn")
	}
	if string(again) != string(out) {
		t.Error("a second injection altered the body")
	}
	// Byte-stability across calls: the definition is spliced raw precisely so two injections of the
	// same body produce the same bytes rather than a re-marshalled map's key order.
	out2, _ := Inject("anthropic", body)
	if string(out2) != string(out) {
		t.Error("two injections of the same body differ byte-wise; the prefix would flap")
	}
}

func TestResponsesToolAndStrayRepair(t *testing.T) {
	body := []byte(`{"input":[{"role":"user","content":"go"}],"tools":[{"type":"function","name":"Read","parameters":{"type":"object"}}]}`)
	injected, ok := Inject("responses", body)
	if !ok || !HasTool("responses", injected) || gjson.GetBytes(injected, "tools.1.name").String() != ToolName ||
		gjson.GetBytes(injected, "tools.1.parameters.type").String() != "object" {
		t.Fatalf("native Responses declaration was not injected: %s", injected)
	}
	resp := []byte(`{"output":[{"type":"function_call","name":"context_guru_adjudicate","call_id":"c1"}]}`)
	if ids := ResponseCallIDs("responses", resp); len(ids) != 1 || ids[0] != "c1" {
		t.Fatalf("Responses call IDs = %v", ids)
	}
	broken := []byte(`{"input":[{"type":"function_call","name":"context_guru_adjudicate","call_id":"c1"},` +
		`{"type":"function_call_output","call_id":"c1","output":"not found"}]}`)
	repaired, n := AnswerStrayCalls("responses", broken)
	if n != 1 || gjson.GetBytes(repaired, "input.1.output").String() != StrayAnswer {
		t.Fatalf("Responses stray repair failed: %s", repaired)
	}
}

// Two cases where injecting would change what the model believes it can do, or which tool it is
// compelled to call. Both must be refused.
func TestInjectRefusesWhenItWouldPerturbSelection(t *testing.T) {
	noTools := []byte(`{"model":"m","messages":[{"role":"user","content":"go"}]}`)
	if _, ok := Inject("anthropic", noTools); ok {
		t.Error("injected into a request with NO tools; that hands the model its first tool and " +
			"changes what it believes it can do")
	}
	forced := []byte(`{"model":"m","tool_choice":{"type":"tool","name":"Read"},` +
		`"tools":[{"name":"Read","description":"d","input_schema":{"type":"object"}}],` +
		`"messages":[{"role":"user","content":"go"}]}`)
	if _, ok := Inject("anthropic", forced); ok {
		t.Error("injected under a forcing tool_choice; tool selection must never be perturbed")
	}
	// An explicit auto is not forcing, so it must still inject.
	auto := []byte(`{"model":"m","tool_choice":{"type":"auto"},` +
		`"tools":[{"name":"Read","description":"d","input_schema":{"type":"object"}}],` +
		`"messages":[{"role":"user","content":"go"}]}`)
	if _, ok := Inject("anthropic", auto); !ok {
		t.Error("refused an auto tool_choice, which leaves the model free to choose")
	}
}

// A stray call from the AGENT must get a definite answer, and the real tools' results must survive
// untouched. The model does call advertised tools it was told to leave alone -- directly observed with
// the expand tool, which a run called at step 2 -- so this path is load-bearing, not defensive.
func TestAnswerStrayCallsLeavesRealResultsAlone(t *testing.T) {
	before := StrayAnswered()
	body := []byte(`{"model":"m","messages":[
	  {"role":"assistant","content":[
	     {"type":"tool_use","id":"u1","name":"` + ToolName + `","input":{"verdicts":[]}},
	     {"type":"tool_use","id":"u2","name":"Read","input":{"path":"a.py"}}]},
	  {"role":"user","content":[
	     {"type":"tool_result","tool_use_id":"u1","is_error":true,"content":"Tool '` + ToolName + `' not found"},
	     {"type":"tool_result","tool_use_id":"u2","content":"real file contents"}]}
	]}`)
	out, n := AnswerStrayCalls("anthropic", body)
	if n != 1 {
		t.Fatalf("answered %d stray calls, want 1", n)
	}
	s := string(out)
	if strings.Contains(s, "not found") {
		t.Error("the client's dead-end refusal reached the model; the point is to replace it")
	}
	if !strings.Contains(s, "runs automatically") {
		t.Error("no substitute answer was written")
	}
	if !strings.Contains(s, "real file contents") {
		t.Error("a REAL tool's result was overwritten; only our own calls may be touched")
	}
	if gjson.GetBytes(out, "messages.1.content.0.is_error").Bool() {
		t.Error("is_error survived, so the model reads a failure alongside the answer to that call")
	}
	if got := StrayAnswered() - before; got != 1 {
		t.Errorf("counted %d stray calls, want 1; the rate is the signal for whether the tool's "+
			"description is working", got)
	}
	// OpenAI dialect: a role=tool message answering one call.
	oa := []byte(`{"model":"m","messages":[
	  {"role":"assistant","tool_calls":[{"id":"c1","function":{"name":"` + ToolName + `","arguments":"{}"}}]},
	  {"role":"tool","tool_call_id":"c1","content":"Tool not found"}]}`)
	out2, n2 := AnswerStrayCalls("openai", oa)
	if n2 != 1 || !strings.Contains(string(out2), "runs automatically") {
		t.Errorf("OpenAI dialect not handled: answered=%d body=%.180s", n2, out2)
	}
}

// A body with no calls to our tool must come back byte-identical: this runs on every request, and a
// gratuitous rewrite would change the prefix and cost a cache write for nothing.
func TestAnswerStrayCallsIsANoOpWhenUninvolved(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
	  {"role":"assistant","content":[{"type":"tool_use","id":"u9","name":"Read","input":{}}]},
	  {"role":"user","content":[{"type":"tool_result","tool_use_id":"u9","content":"data"}]}]}`)
	out, n := AnswerStrayCalls("anthropic", body)
	if n != 0 {
		t.Errorf("answered %d calls on a body that never called our tool", n)
	}
	if string(out) != string(body) {
		t.Error("body was rewritten with nothing to do; that costs a cache write for nothing")
	}
}

// HasTool is the ADVERTISE test: a host must answer stray calls exactly when it is true, so it has to
// distinguish our tool from a client tool by the same shape in both dialects.
func TestHasTool(t *testing.T) {
	oa, ok := Inject("openai", []byte(`{"tools":[{"type":"function","function":{"name":"Read"}}]}`))
	if !ok {
		t.Fatal("openai injection refused on a request that declares tools")
	}
	if !HasTool("openai", oa) {
		t.Error("HasTool did not see the tool we just injected in the openai dialect")
	}
	if HasTool("anthropic", []byte(`{"tools":[{"name":"Read"}]}`)) {
		t.Error("HasTool matched a request that declares only the client's own tools")
	}
}
