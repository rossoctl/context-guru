package predictive

import "testing"

func TestFamilyOf(t *testing.T) {
	for in, want := range map[string]string{
		"claude-opus-5": "opus", "aws/claude-sonnet-4-6": "sonnet", "claude-haiku-4-5[1m]": "haiku",
		"claude-fable-5-1": "other", "gpt-5": "other", "": "other",
	} {
		if got := FamilyOf(in); got != want {
			t.Errorf("FamilyOf(%q) = %q, want %q", in, got, want)
		}
	}
}
