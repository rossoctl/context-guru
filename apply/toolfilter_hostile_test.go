package apply

import (
	"encoding/json"
	"strings"
	"testing"

	_ "github.com/rossoctl/context-guru/components/all"
	"github.com/rossoctl/context-guru/config"
)

// The removal list is TENANT-SUPPLIED, and it decides what we stop sending upstream. A name
// is therefore validated when the configuration is WRITTEN, not when a request is filtered:
// a glob would make the set of removed tools grow on its own, and a metacharacter or a quote
// would only be discovered by whatever it broke. Every case below must be a rejected
// configuration, not a filter that matches nothing.
func TestToolfilterRefusesHostileNames(t *testing.T) {
	for _, n := range []string{`a"b`, `mcp__*`, `*`, `Read|Write`, `Read.*`, `Re ad`, "Ré",
		strings.Repeat("A", 129), `{"x":1}`, `Read]`, "Re\nad"} {
		if err := config.Validate(cfgWithRemove(t, n)); err == nil {
			t.Errorf("accepted %q as a declaration name", n)
		}
	}
	// And the legitimate charset still passes, or the feature is unusable.
	for _, n := range []string{"Read", "mcp__plugin_context7_context7__query-docs",
		"mcp__github", "apps/web:deploy", "superpowers:writing-plans"} {
		if err := config.Validate(cfgWithRemove(t, n)); err != nil {
			t.Errorf("refused legitimate name %q: %v", n, err)
		}
	}
}

func cfgWithRemove(t *testing.T, name string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{ // JSON is a subset of YAML, and it quotes for us
		"pipeline":   []string{"toolfilter"},
		"components": map[string]any{"toolfilter": map[string]any{"remove": []string{name}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The splice may only ever REMOVE. A duplicate name, a 10,000-element catalogue and a
// removal that would empty the array are all shapes a caller can send, and none of them may
// produce a body we did not mean to send: invalid JSON, an altered survivor, or an empty
// `tools` (which some providers reject outright).
func TestFilterSpliceOnHostileCatalogue(t *testing.T) {
	var tools []map[string]any
	for i := 0; i < 10000; i++ {
		tools = append(tools, map[string]any{"name": "dup", "description": "x",
			"input_schema": map[string]any{"type": "object"}})
	}
	tools = append(tools, map[string]any{"name": "Keep", "description": "kept"})
	body, err := json.Marshal(map[string]any{"tools": tools, "system": "nothing here",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	out, _, n := filterDeclarations(body, []string{"dup"})
	var got struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("splice produced invalid JSON: %v", err)
	}
	if n != 10000 || len(got.Tools) != 1 || got.Tools[0]["name"] != "Keep" ||
		got.Tools[0]["description"] != "kept" {
		t.Fatalf("removed %d, survivors %v: the splice altered what it kept", n, got.Tools)
	}
	// A name that matches nothing must leave the body BYTE-identical: any edit to `tools`
	// re-anchors the whole cached prefix.
	if out2, tok, n2 := filterDeclarations(body, []string{"nosuchtool"}); string(out2) != string(body) ||
		tok != 0 || n2 != 0 {
		t.Error("a no-match removal list did not leave the body byte-identical")
	}
	// Removing everything must DECLINE rather than send `tools: []`.
	if out3, _, n3 := filterDeclarations(body, []string{"dup", "Keep"}); n3 != 0 ||
		string(out3) != string(body) {
		t.Error("the filter emptied the tools array")
	}
}
