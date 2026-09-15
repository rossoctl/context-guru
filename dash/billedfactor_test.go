package dash

import (
	"math"
	"testing"

	"github.com/rossoctl/context-guru/internal/tokens"
)

// TestBilledFactorReachesEverySavingsFigure is the regression guard for #240. The defect was
// that our own token counts were priced at the provider's rates with no reconciliation, and
// the fix is a measured per-family factor applied to the COUNTERFACTUAL dollars.
//
// It pins the factor at every site it has to reach, because the arithmetic is implemented more
// than once: Event.Price writes baseline_cost_usd and each component's saved_usd, and
// dash/readvalue.go re-derives the same formula at READ time. A fix applied to one while the
// other stayed put is the failure this asserts against.
func TestBilledFactorReachesEverySavingsFigure(t *testing.T) {
	const removed = 10_000

	mk := func(model string) *Event {
		e := &Event{
			TS: 1, SessionID: "s", Model: model,
			TokensBefore: 100_000, TokensAfter: 100_000 - removed,
			SavedUnique: removed, // all of it new content: the whole delta at the write rate
			FreshInput:  10, CacheRead: 0, CacheWrite: 80_000, OutputTokens: 5,
			Components: []CompRow{{Component: "searchfold", SavedGross: removed, SavedUnique: removed}},
		}
		e.Price(ibmSonnet, true)
		return e
	}

	// A measured family: the delta must be the raw arithmetic TIMES the factor, and the factor
	// must be on the row so a reader can divide it back out.
	son := mk("aws/claude-sonnet-5")
	wantF, measured := tokens.BilledDeltaFactor("aws/claude-sonnet-5")
	if !measured {
		t.Fatal("aws/claude-sonnet-5 should be a measured family")
	}
	if son.BilledTokenFactor != wantF || !son.BilledTokenFactorMeasured {
		t.Errorf("factor on the row = %v (measured=%v), want %v (true)",
			son.BilledTokenFactor, son.BilledTokenFactorMeasured, wantF)
	}
	raw := float64(removed) * ibmSonnet.CacheWrite
	if got, want := son.BaselineCostUSD-son.CostUSD, raw*wantF; math.Abs(got-want) > 1e-12 {
		t.Errorf("baseline delta = %.12f, want %.12f (raw %.12f x %.4f)", got, want, raw, wantF)
	}
	if got, want := son.Components[0].SavedUSD, raw*wantF; math.Abs(got-want) > 1e-12 {
		t.Errorf("component saved_usd = %.12f, want %.12f", got, want)
	}
	// The direction of the whole issue: the corrected figure is LARGER, because the provider
	// counts more tokens in the same removed text than our o200k estimate does.
	if son.BaselineCostUSD-son.CostUSD <= raw {
		t.Errorf("the correction must raise the delta on a Claude model: %.12f vs raw %.12f",
			son.BaselineCostUSD-son.CostUSD, raw)
	}

	// An UNMEASURED family gets exactly 1.0 and is flagged as unmeasured — no other family's
	// constant is borrowed, and no correction is invented.
	other := mk("some-model-nobody-measured")
	if other.BilledTokenFactor != 1.0 || other.BilledTokenFactorMeasured {
		t.Errorf("unmeasured family: factor %v measured %v, want 1.0 false",
			other.BilledTokenFactor, other.BilledTokenFactorMeasured)
	}
	if got := other.BaselineCostUSD - other.CostUSD; math.Abs(got-raw) > 1e-12 {
		t.Errorf("unmeasured family delta = %.12f, want the raw %.12f", got, raw)
	}

	// CostUSD is provider-reported and must NOT move: the factor corrects counterfactuals only.
	// If this ever fires, a measured bill is being scaled by an estimate.
	if math.Abs(son.CostUSD-other.CostUSD) > 1e-15 {
		t.Errorf("cost_usd differs by model factor (%.12f vs %.12f) — the factor leaked into a billed figure",
			son.CostUSD, other.CostUSD)
	}
	// And the haiku family must differ from the sonnet/opus one: they were measured as
	// different tokenizers, and collapsing them to one constant is the mistake this prevents.
	hf, _ := tokens.BilledDeltaFactor("claude-haiku-4-5")
	if hf == wantF {
		t.Errorf("haiku and sonnet/opus factors are identical (%v); they were measured apart", hf)
	}
}
