package predictor

import (
	"context"
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/kvcache"
)

// stubPricer is a minimal operator price list, in per-token units — the same shape
// kvcache's own pricing_test.go uses.
type stubPricer map[string]modelinfo.Price

func (s stubPricer) Price(_ context.Context, model string) (modelinfo.Price, bool) {
	p, ok := s[model]
	return p, ok
}

func TestRegisterRefusesBlankIDVersionOrNilPredictor(t *testing.T) {
	cases := []Registered{
		{ID: "", Version: "v1", Predictor: stubPredictor{}},
		{ID: "x", Version: "", Predictor: stubPredictor{}},
		{ID: "x", Version: "v1", Predictor: nil},
	}
	for i, r := range cases {
		p := NewPredictors()
		if err := p.Register(r); err == nil {
			t.Errorf("case %d: expected an error registering %+v", i, r)
		}
	}
}

func TestRegisterRefusesDuplicateKey(t *testing.T) {
	p := NewPredictors()
	r := Registered{ID: "reuse-model", Version: "v1", Predictor: stubPredictor{p: 0.5, ok: true}}
	if err := p.Register(r); err != nil {
		t.Fatalf("first registration should succeed: %v", err)
	}
	if err := p.Register(r); err == nil {
		t.Fatal("registering the same (ID, Version) twice must be refused — it would let a " +
			"later call silently swap the model a replay is reading")
	}
}

func TestListIsSortedByIDThenVersion(t *testing.T) {
	p := NewPredictors()
	for _, r := range []Registered{
		{ID: "b", Version: "v1", Predictor: stubPredictor{}},
		{ID: "a", Version: "v2", Predictor: stubPredictor{}},
		{ID: "a", Version: "v1", Predictor: stubPredictor{}},
	} {
		if err := p.Register(r); err != nil {
			t.Fatal(err)
		}
	}
	got := p.List()
	want := []string{"a@v1", "a@v2", "b@v1"}
	if len(got) != len(want) {
		t.Fatalf("List() has %d entries, want %d", len(got), len(want))
	}
	for i, r := range got {
		if r.key() != want[i] {
			t.Errorf("List()[%d] = %s, want %s", i, r.key(), want[i])
		}
	}
}

func TestEligibleFiltersToValidated(t *testing.T) {
	p := NewPredictors()
	must := func(r Registered) {
		if err := p.Register(r); err != nil {
			t.Fatal(err)
		}
	}
	must(Registered{ID: "reviewed", Version: "v1", Validated: true, Predictor: stubPredictor{}})
	must(Registered{ID: "draft", Version: "v1", Validated: false, Predictor: stubPredictor{}})

	elig := p.Eligible()
	if len(elig) != 1 || elig[0].ID != "reviewed" {
		t.Fatalf("Eligible() = %+v, want exactly [reviewed@v1]", elig)
	}
}

func TestToStrategyRefusesUnvalidatedWithoutOverride(t *testing.T) {
	r := Registered{ID: "draft", Version: "v1", Validated: false, Predictor: stubPredictor{p: 0.9, ok: true}}
	if _, err := ToStrategy(r, false, 0, 0); err == nil {
		t.Fatal("an unvalidated predictor must be refused for enforcement without allowUnvalidated")
	}
	s, err := ToStrategy(r, true, 0, 0)
	if err != nil {
		t.Fatalf("allowUnvalidated=true should succeed: %v", err)
	}
	if s.Name() == "" {
		t.Error("ToStrategy returned a strategy with no name")
	}
}

func TestToStrategyValidatedNeedsNoOverride(t *testing.T) {
	r := Registered{ID: "shipped", Version: "v1", Validated: true, Predictor: stubPredictor{p: 0.9, ok: true}}
	if _, err := ToStrategy(r, false, 0, 0); err != nil {
		t.Fatalf("a Validated predictor must not need allowUnvalidated: %v", err)
	}
}

// TestToStrategyBridgesPriceAndTTLInterpretationWithoutDuplicatingIt exercises the actual
// bridge to kvcache.Simulate/kvcache.Custom on a small dataset that mixes a priced and an
// UNKNOWN model, confirming ToStrategy's output composes with the existing, already-tested
// pricing machinery rather than reimplementing it (see ToStrategy's own doc comment: it
// wraps Custom, it does not restate the break-even arithmetic).
func TestToStrategyBridgesPriceAndTTLInterpretationWithoutDuplicatingIt(t *testing.T) {
	rate := modelinfo.Price{Input: 3e-6, Output: 15e-6, CacheRead: 0.3e-6, CacheWrite: 3.75e-6}
	prices := kvcache.NewPriceList(context.Background(), []string{"known-model"},
		stubPricer{"known-model": rate}, kvcache.Multipliers{}, nil)

	rows := derive([]*kvcache.Request{
		req(1, "u", "s1", base, "known-model", 100_000, "end_turn", "hit", 0, 0),
		req(2, "u", "s1", base+60_000, "known-model", 100_000, "end_turn", "hit", 0, 0),
		req(3, "u", "s2", base, "unknown-model", 100_000, "end_turn", "hit", 0, 0),
		req(4, "u", "s2", base+60_000, "unknown-model", 100_000, "end_turn", "hit", 0, 0),
	})

	r := Registered{ID: "always-cache", Version: "v1", Validated: true,
		Predictor: stubPredictor{p: 0.99, ok: true}}
	strat, err := ToStrategy(r, false, 0.5, 0.5)
	if err != nil {
		t.Fatalf("ToStrategy: %v", err)
	}

	result := kvcache.Simulate(rows, strat, kvcache.Config{Prices: prices})
	if result.Requests != 4 {
		t.Fatalf("Requests = %d, want 4", result.Requests)
	}
	// The two rows on the unpriced model must be COUNTED, never guessed at a price — the
	// same invariant kvcache's own TestUnpricedRequestsAreCountedNotValued asserts for the
	// hand-written arms; this proves the bridge does not quietly bypass it.
	if result.Unpriced != 2 {
		t.Fatalf("Unpriced = %d, want 2 (the two unknown-model rows)", result.Unpriced)
	}
}
