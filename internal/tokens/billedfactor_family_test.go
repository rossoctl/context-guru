package tokens_test

import (
	"testing"

	"github.com/rossoctl/context-guru/internal/tokens"
)

func TestBilledDeltaFactorByFamily(t *testing.T) {
	for model, want := range map[string]struct {
		f float64
		m bool
	}{
		"claude-haiku-4-5":    {1.3686, true},
		"aws/claude-sonnet-5": {1.6857, true},
		"claude-opus-5[1m]":   {1.6857, true},
		"claude-fable-5-1":    {1.0, false}, // not measured: unchanged
		"gpt-5":               {1.0, false},
	} {
		if f, m := tokens.BilledDeltaFactor(model); f != want.f || m != want.m {
			t.Errorf("%q = %v,%v want %v,%v", model, f, m, want.f, want.m)
		}
	}
}
