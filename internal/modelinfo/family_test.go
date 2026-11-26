package modelinfo

import "testing"

func TestFamily(t *testing.T) {
	for in, want := range map[string]string{
		"claude-opus-5":                   FamilyOpus,
		"AWS/Claude-Sonnet-4-6":           FamilySonnet,
		"us.anthropic.claude-haiku-4-5":   FamilyHaiku,
		"claude-fable-5-1":                FamilyFable,
		"claude-fable-5[1m]":              FamilyFable,
		" vertex_ai/claude-opus-4-8[1m] ": FamilyOpus,
		"gpt-5":                           "",
		"":                                "",
	} {
		if got := Family(in); got != want {
			t.Errorf("Family(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsClaude(t *testing.T) {
	for in, want := range map[string]bool{
		"claude-fable-5": true, "anthropic.x": true, "AWS/Claude-Opus-5": true,
		"opus": false, "gpt-5": false, "": false,
	} {
		if got := IsClaude(in); got != want {
			t.Errorf("IsClaude(%q) = %v, want %v", in, got, want)
		}
	}
}
