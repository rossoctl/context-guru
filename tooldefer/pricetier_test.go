package tooldefer

import (
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

func gpt56() modelinfo.Price {
	return modelinfo.Price{Input: 5e-6, CacheRead: 0.5e-6, CacheWrite: 5e-6, Output: 30e-6}.WithTiers("gpt-5.6")
}

// Each side of the saving bills at its own prompt size. The model is unknown to the tokenizer table,
// so the billed factor is 1 and the weights are plain token counts.
func TestPriceSidesInOppositeTiers(t *testing.T) {
	// The request sent has 270,000 tokens (base). Without the deferral it had 270,000 + 5,000 - 100 = 274,900 (2x).
	r := Request{Model: "x", DeferredTokens: 5000, ListTokens: 100, Tier: TierRead, PromptTokens: 270_000}
	m, ok := Price(r, gpt56())
	if !ok {
		t.Fatal("unpriced")
	}
	if want := 5000 * 1e-6; !near(m.Gross, want) { // read at 2x
		t.Fatalf("gross %v want %v", m.Gross, want)
	}
	if want := 100 * 0.5e-6; !near(m.Cost, want) { // read at base
		t.Fatalf("cost %v want %v", m.Cost, want)
	}
}

func TestPriceBothAboveAndBelowTheLine(t *testing.T) {
	above := Request{Model: "x", DeferredTokens: 5000, ListTokens: 100, Tier: TierRead, PromptTokens: 400_000}
	m, _ := Price(above, gpt56())
	if !near(m.Gross, 5000*1e-6) || !near(m.Cost, 100*1e-6) {
		t.Fatalf("above: %+v", m)
	}
	below := Request{Model: "x", DeferredTokens: 5000, ListTokens: 100, Tier: TierRead, PromptTokens: 100_000}
	m, _ = Price(below, gpt56())
	if !near(m.Gross, 5000*0.5e-6) || !near(m.Cost, 100*0.5e-6) {
		t.Fatalf("below: %+v", m)
	}
	// Unknown size: the base rates, as before.
	unknown := Request{Model: "x", DeferredTokens: 5000, ListTokens: 100, Tier: TierRead}
	m, _ = Price(unknown, gpt56())
	if !near(m.Gross, 5000*0.5e-6) {
		t.Fatalf("unknown: %+v", m)
	}
}
