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

// The size without the deferral is rounded UP. Haiku bills 1.3686 per counted token, so one deferred
// token is 1.3686 billed: from 99,999 that must reach 100,001 (the dear tier), not stop at 100,000.
func TestPriceRoundsTheWithoutSizeUp(t *testing.T) {
	haiku := modelinfo.Price{Input: 1e-6, CacheRead: 0.1e-6, CacheWrite: 1.25e-6, Output: 5e-6}.WithTiers("claude-haiku-5-5")
	r := Request{Model: "claude-haiku-5-5", DeferredTokens: 1, Tier: TierRead, PromptTokens: 99_999}
	m, _ := Price(r, haiku)
	if want := 1.3686 * 0.5e-6; !near(m.Gross, want) {
		t.Fatalf("gross %v want %v", m.Gross, want)
	}
}
