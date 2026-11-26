package apply

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// A Responses `mcp` tool names itself by server_label. The inventory lists it under that label, so a
// removal that names the label has to match it.
func TestFilterRemovesAResponsesMCPToolByServerLabel(t *testing.T) {
	body := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"tools":[` +
		`{"type":"function","name":"Read","description":"read a file","parameters":{"type":"object"}},` +
		`{"type":"mcp","server_label":"deepwiki","server_url":"https://mcp.example/mcp","require_approval":"never"}]}`)
	out, _, n := filterDeclarations(body, []string{"deepwiki"})
	if n != 1 {
		t.Fatalf("removed %d declarations, want 1: %s", n, out)
	}
	if strings.Contains(string(out), "deepwiki") {
		t.Errorf("the mcp tool is still declared: %s", out)
	}
	if got := gjson.GetBytes(out, "tools.#").Int(); got != 1 {
		t.Errorf("tools left = %d, want 1", got)
	}
}

// The entry "mcp", which removed every Responses mcp tool before the label was the name, still does.
func TestFilterRemovesAResponsesMCPToolByItsType(t *testing.T) {
	body := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"tools":[` +
		`{"type":"function","name":"Read","description":"read a file","parameters":{"type":"object"}},` +
		`{"type":"mcp","server_label":"deepwiki","server_url":"https://mcp.example/mcp"},` +
		`{"type":"mcp","server_label":"other","server_url":"https://mcp.example/2"}]}`)
	out, _, n := filterDeclarations(body, []string{"mcp"})
	if n != 2 || strings.Contains(string(out), "server_label") {
		t.Fatalf("removed %d, want both mcp tools: %s", n, out)
	}
}

// An empty entry in the remove list marks nothing. A forced tool keeps the catalogue from emptying, so an
// unguarded empty entry would remove the other tools and show here.
func TestFilterEmptyEntryMarksNothing(t *testing.T) {
	body := []byte(`{"model":"gpt-5","tool_choice":{"name":"Read"},"messages":[{"role":"user","content":"hi"}],"tools":[` +
		`{"type":"function","name":"Read","description":"read a file","parameters":{"type":"object"}},` +
		`{"type":"function","name":"Write","description":"write a file","parameters":{"type":"object"}},` +
		`{"server_label":"deepwiki","server_url":"https://mcp.example/mcp"}]}`)
	if out, _, n := filterDeclarations(body, []string{""}); n != 0 || string(out) != string(body) {
		t.Fatalf("removed %d for an empty entry: %s", n, out)
	}
}
