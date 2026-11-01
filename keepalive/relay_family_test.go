package keepalive

import "testing"

func TestClaudeModelFamilies(t *testing.T) {
	t.Setenv("CONTEXT_GURU_KEEPALIVE_MODELS", "my-profile")
	for model, want := range map[string]bool{
		"claude-fable-5": true, "fable-alias": true, "opus": true, "MY-PROFILE-1": true, "gpt-5": false, "": false,
	} {
		if got := claudeModel(model); got != want {
			t.Errorf("claudeModel(%q) = %v, want %v", model, got, want)
		}
	}
}
