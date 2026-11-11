// Package ratefilltest holds what the per-caller price fixture tests share (issue #856): the
// model list and the check against testdata/ratefill_before.json. Test code only.
package ratefilltest

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"
)

// Models is the set every caller is priced for, before and after the cache-rate
// refactor (issue #856): every tier on the pricing page, the prefix traps, a non-Anthropic id,
// a tier name and an empty model.
var Models = []string{
	"claude-opus-5-5",
	"claude-sonnet-5-5",
	"claude-fable-5-1",
	"claude-mythos-5-1",
	"claude-opus-5",
	"claude-sonnet-5",
	"claude-fable-5",
	"claude-mythos-5",
	"claude-haiku-5-5",
	"claude-haiku-4-5",
	"claude-opus-4-8",
	"claude-sonnet-4-5",
	"claude-opus-5-20261101",
	"claude-opus-5-50",
	"claude-opus-5-1",
	"claude-fable-5-5",
	"claude-opus-5.5",
	"Claude-Sonnet-5-5",
	"claude-opus-5-5[1m]",
	"aws/claude-opus-5-5",
	"aws/claude-sonnet-5",
	"aws/claude-fable-5-1",
	"us.anthropic.claude-mythos-5-1-v1:0",
	"anthropic.claude-sonnet-5-5-v1:0",
	"azure/gpt-5.5",
	"azure/gpt-5.6-luna",
	"gemini-3-flash-preview",
	"gcp/gemini-2.5-pro",
	"premium",
	"",
}

// Check compares got with the fixture. RATEFILL_UPDATE=1 rewrites the fixture; that is
// how the "before" values were made, on origin/main c2a06dd5, before any code changed. The four
// table/*/claude-sonnet-5 rows were re-made by #334 (a bare id takes its qualified entry).
func Check(t *testing.T, got map[string]string) {
	t.Helper()
	const path = "testdata/ratefill_before.json"
	if os.Getenv("RATEFILL_UPDATE") == "1" {
		b, _ := json.MarshalIndent(got, "", "  ")
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]string
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	if len(want) != len(got) {
		t.Errorf("fixture has %d rows, run has %d", len(want), len(got))
	}
	for k, w := range want {
		if g, ok := got[k]; !ok || g != w {
			t.Errorf("%s: got %q, before the refactor it was %q", k, g, w)
		}
	}
}

func F(vs ...float64) string {
	s := ""
	for i, v := range vs {
		if i > 0 {
			s += " "
		}
		s += strconv.FormatFloat(v, 'g', -1, 64)
	}
	return s
}
