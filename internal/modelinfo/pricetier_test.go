package modelinfo

import (
	"math"
	"reflect"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) <= 1e-12*math.Max(1, math.Abs(b)) }

func TestPromptTokensCountsInputReadsAndWrites(t *testing.T) {
	if got := PromptTokens(10, 200, 3000); got != 3210 {
		t.Errorf("PromptTokens = %d, want 3210", got)
	}
}

func TestAtNoTiersReturnsItself(t *testing.T) {
	p := Price{Input: 1e-6, Output: 2e-6, CacheRead: 1e-7, CacheWrite: 1.25e-6, ReadFrac: 0.1}
	for _, n := range []int64{0, 1, 5_000_000} {
		if got := p.At(n); !reflect.DeepEqual(got, p) {
			t.Errorf("At(%d) = %+v, want %+v", n, got, p)
		}
	}
}

// Haiku 5.5: $0.10 in, $0.50 out, $0.125 5m write, $0.01 read per MTok. Above 100,000 tokens all x5.
func TestAtHaiku55(t *testing.T) {
	base := Price{Input: 0.10e-6, Output: 0.50e-6, CacheRead: 0.01e-6, CacheWrite: 0.125e-6}.Resolved("claude-haiku-5-5").WithTiers("claude-haiku-5-5")
	if !base.HasTiers() {
		t.Fatalf("Tiers = %+v, want a built-in tier", base.Tiers)
	}
	for _, tc := range []struct {
		n    int64
		mult float64
	}{{0, 1}, {99_999, 1}, {100_000, 1}, {100_001, 5}, {900_000, 5}} {
		got := base.At(tc.n)
		want := Price{Input: 0.10e-6 * tc.mult, Output: 0.50e-6 * tc.mult, CacheRead: 0.01e-6 * tc.mult, CacheWrite: 0.125e-6 * tc.mult}
		if !near(got.Input, want.Input) || !near(got.Output, want.Output) || !near(got.CacheRead, want.CacheRead) || !near(got.CacheWrite, want.CacheWrite) {
			t.Errorf("At(%d) = %+v, want %+v", tc.n, got, want)
		}
		if got.HasTiers() {
			t.Errorf("At(%d) kept tiers", tc.n)
		}
	}
	// The 1-hour write follows Input: $0.20 -> $1.00 per MTok above the line.
	if got := base.At(100_000).CacheWrite1h(); !near(got, 0.20e-6) {
		t.Errorf("1h write at line = %g, want 2e-7", got)
	}
	if got := base.At(100_001).CacheWrite1h(); !near(got, 1.0e-6) {
		t.Errorf("1h write above line = %g, want 1e-6", got)
	}
}

// gpt-5.6: 2x input, read and write; 1.5x output above 272,000.
func TestAtGPT56(t *testing.T) {
	base := Price{Input: 4e-6, Output: 20e-6, CacheRead: 0.4e-6, CacheWrite: 5e-6}.WithTiers("gpt-5.6")
	if got := base.At(272_000); got.Input != 4e-6 || got.Output != 20e-6 {
		t.Errorf("At(272000) = %+v, want base", got)
	}
	got := base.At(272_001)
	if !near(got.Input, 8e-6) || !near(got.Output, 30e-6) || !near(got.CacheRead, 0.8e-6) || !near(got.CacheWrite, 10e-6) {
		t.Errorf("At(272001) = %+v, want 8/30/0.8/10 per MTok", got)
	}
}

// A price that is not Resolved keeps its cache-rate fallbacks: they follow the scaled Input.
func TestAtKeepsCacheFallbacksConsistent(t *testing.T) {
	p := Price{Input: 1e-6, ReadFrac: 0.1, WriteFrac: 1.25,
		Tiers: [MaxTiers]PriceTier{{AbovePromptTokens: 100, InputMult: 2, ReadMult: 2, WriteMult: 2}}}
	hi := p.At(101)
	if !near(hi.Input, 2e-6) {
		t.Fatalf("Input = %g", hi.Input)
	}
	if got := hi.CacheReadFor("gpt-4o"); !near(got, 0.2e-6) {
		t.Errorf("CacheReadFor above = %g, want 2e-7", got)
	}
	if got := hi.CacheWrite5mFor("gpt-4o"); !near(got, 2.5e-6) {
		t.Errorf("CacheWrite5mFor above = %g, want 2.5e-6", got)
	}
}

func TestAtStatedTierRateWinsOverMultiple(t *testing.T) {
	p := Price{Input: 1e-6, Output: 4e-6, Tiers: [MaxTiers]PriceTier{{AbovePromptTokens: 10, Input: 3e-6, InputMult: 9, OutputMult: 2}}}
	got := p.At(11)
	if got.Input != 3e-6 || !near(got.Output, 8e-6) {
		t.Errorf("At = %+v, want Input 3e-6 and Output 8e-6", got)
	}
}

func TestAtPicksHighestLineBelowPrompt(t *testing.T) {
	p := Price{Input: 1, Tiers: [MaxTiers]PriceTier{{AbovePromptTokens: 500, InputMult: 4}, {AbovePromptTokens: 100, InputMult: 2}}}
	for n, want := range map[int64]float64{100: 1, 101: 2, 500: 2, 501: 4} {
		if got := p.At(n).Input; got != want {
			t.Errorf("At(%d).Input = %g, want %g", n, got, want)
		}
	}
}

func TestBuiltinTiersByModel(t *testing.T) {
	for model, want := range map[string]bool{
		"gpt-5.4": true, "gpt-5.5": true, "gpt-5.6-sol": true, "azure/gpt-5.6-luna": true, "gpt-6.1-sol": true,
		"gpt-5.5-codex": true, "gpt-5.4-pro": true,
		"gpt-5.3-codex": false, "gpt-5": false, "gpt-4o": false, "gpt-5.4-mini": false, "gpt-5.4-nano": false,
		"claude-haiku-5-5": true, "aws/claude-haiku-5-5": true, "us.anthropic.claude-haiku-5-5-v1:0": true,
		"claude-haiku-4-5": false, "claude-sonnet-5": false, "claude-opus-5": false, "": false,
	} {
		if _, got := BuiltinTier(model); got != want {
			t.Errorf("BuiltinTier(%q) has tier = %v, want %v", model, got, want)
		}
	}
}

func TestWithTiersKeepsAStatedTier(t *testing.T) {
	stated := [MaxTiers]PriceTier{{AbovePromptTokens: 200_000, Input: 9}}
	got := Price{Input: 1, Tiers: stated}.WithTiers("gpt-5.5")
	if got.Tiers != stated {
		t.Errorf("Tiers = %+v, want the stated tier", got.Tiers)
	}
}

// Price must stay comparable: the packages that alias it compare and key maps by it.
var _ = Price{} == Price{}

func TestSharedGPTVersionParser(t *testing.T) {
	for model, want := range map[string][2]bool{ // {5.4+, 5.6+}
		"gpt-5.4": {true, false}, "gpt-5.5": {true, false}, "gpt-5.6": {true, true}, "gpt-5.10": {true, true},
		"gpt-6": {true, true}, "gpt-5": {false, false}, "gpt-5.": {false, false}, "gpt-4o": {false, false},
		"azure/gpt-5.6-luna": {true, true}, "gpt-5.3/gpt-5.4": {true, false}, "nogpt": {false, false},
	} {
		if got := [2]bool{GPT54OrLater(model), GPT56OrLater(model)}; got != want {
			t.Errorf("%q: {54,56} = %v, want %v", model, got, want)
		}
	}
}

func TestTablePriceGetsBuiltinTier(t *testing.T) {
	tb, err := ParseTable([]byte("models:\n  - match: gpt-5.5\n    in: 5\n    out: 30\n  - match: claude-sonnet-5\n    in: 3\n    out: 15\n"))
	if err != nil {
		t.Fatal(err)
	}
	p, ok := tb.Price(nil, "gpt-5.5")
	if !ok || !p.HasTiers() || !near(p.At(272_001).Output, 45e-6) {
		t.Errorf("gpt-5.5 from the operator file: %+v, want the built-in tier (output 45/MTok above 272k)", p)
	}
	if p, _ := tb.Price(nil, "claude-sonnet-5"); p.HasTiers() {
		t.Errorf("claude-sonnet-5 must stay flat, got %+v", p.Tiers)
	}
}

func TestTableFlatKeyTurnsTiersOff(t *testing.T) {
	tb, err := ParseTable([]byte(`models:
  - match: gpt-5.5
    in: 5
    out: 30
    flat: true
  - match: gpt-5.6*
    in: 4
    out: 20
  - match: azure/gpt-5.5*
    in: 5
    out: 30
    flat: true
  - match: claude-haiku-5-5
    in: 0.1
    out: 0.5
    flat: false
`))
	if err != nil {
		t.Fatal(err)
	}
	for model, wantTier := range map[string]bool{
		"gpt-5.5":          false, // flat: true
		"gpt-5.6-sol":      true,  // key absent: built-in tier; a flat neighbour does not leak
		"azure/gpt-5.5-x":  false, // flat family entry
		"claude-haiku-5-5": true,  // flat: false
	} {
		p, ok := tb.Price(nil, model)
		if !ok || p.HasTiers() != wantTier {
			t.Errorf("%s: ok=%v HasTiers=%v, want %v", model, ok, p.HasTiers(), wantTier)
		}
	}
}

func TestTableFlatKeyMustBeABool(t *testing.T) {
	if _, err := ParseTable([]byte("models:\n  - match: gpt-5.5\n    in: 5\n    out: 30\n    flat: maybe\n")); err == nil {
		t.Error("flat: maybe loaded; want an error")
	}
}
