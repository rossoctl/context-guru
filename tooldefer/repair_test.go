package tooldefer

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// ── #871 defect 1: a tool_reference to a tool `tools` lacks ─────────────

// searchTurn is an assistant turn in which our search loaded tool.
func searchTurn(tool string) string {
	return `{"role":"assistant","content":[{"type":"server_tool_use","id":"srvtoolu_1","name":"tool_search_tool_bm25","input":{"query":"` + tool + `","limit":1}},` +
		`{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_1","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"` + tool + `"}]}},` +
		`{"type":"text","text":"ok"}]}`
}

// A resumed transcript whose search loaded a tool of a removed MCP server gets a deferred stub for
// it, appended after the declared tools; the transcript keeps its bytes.
func TestRepairRefsAddsADeferredStub(t *testing.T) {
	b := body([]string{decl("Bash"), searchTool}, "", searchTurn("mcp__gone__lookup"), `{"role":"user","content":"again"}`)
	out, added, ok := RepairRefs(b)
	if !ok || strings.Join(added, ",") != "mcp__gone__lookup" {
		t.Fatalf("ok=%v added=%v", ok, added)
	}
	if got := names(out); got != "Bash,tool_search_tool_bm25,mcp__gone__lookup*" {
		t.Fatalf("tools = %s", got)
	}
	stub := gjson.GetBytes(out, "tools.2")
	if stub.Get("description").String() == "" || stub.Get("input_schema.type").String() != "object" {
		t.Fatalf("stub = %s", stub.Raw)
	}
	if gjson.GetBytes(out, "messages").Raw != gjson.GetBytes(b, "messages").Raw {
		t.Fatal("the repair changed the transcript")
	}
	// Claude Code's own search (a tool_reference in a tool_result) is repaired the same way.
	cc := body([]string{decl("Bash")}, "", `{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"ToolSearch","input":{}}]}`,
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"tool_reference","tool_name":"mcp__gone__x"}]}]}`)
	if out, _, ok := RepairRefs(cc); !ok || names(out) != "Bash,mcp__gone__x*" {
		t.Fatalf("Claude Code reference: ok=%v tools=%s", ok, names(out))
	}
}

// Nothing to repair: every reference is declared, or there is none.
func TestRepairRefsLeavesAValidBodyAlone(t *testing.T) {
	b := body([]string{decl("Bash"), decl("Monitor"), searchTool}, "", searchTurn("Monitor"))
	if out, _, ok := RepairRefs(b); ok || string(out) != string(b) {
		t.Fatal("repaired a body whose references are all declared")
	}
	plain := body([]string{decl("Bash")}, "")
	if _, _, ok := RepairRefs(plain); ok {
		t.Fatal("repaired a body with no reference")
	}
}

func TestMissingRef(t *testing.T) {
	e := `{"type":"error","error":{"type":"invalid_request_error","message":"litellm.BadRequestError: BedrockException - {\"message\":\"Tool reference 'mcp__ops__search_runbooks' not found in available tools\"}"}}`
	if got := MissingRef([]byte(e)); got != "mcp__ops__search_runbooks" {
		t.Fatalf("MissingRef = %q", got)
	}
	if got := MissingRef([]byte(`{"error":{"message":"tools.2.custom.defer_loading: Extra inputs are not permitted"}}`)); got != "" {
		t.Fatalf("MissingRef on another error = %q", got)
	}
}
