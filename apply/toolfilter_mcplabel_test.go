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
