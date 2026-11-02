package proxy

import "testing"

func TestFidMissByFamily(t *testing.T) {
	for model, want := range map[string]float64{
		"claude-opus-5-5": 0.009, "claude-sonnet-5": 0.013, "claude-haiku-4-5": 0.017,
		"claude-opus-4-6": 0.028, "claude-fable-5-1": 0.028,
	} {
		if got := fidMiss(model); got != want {
			t.Errorf("fidMiss(%q) = %v, want %v", model, got, want)
		}
	}
}
