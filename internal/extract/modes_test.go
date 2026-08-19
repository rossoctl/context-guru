package extract

import "testing"

// Modes is what the settings form offers, so a name on that list the engine does not
// honor is a form writing a value the engine ignores — and a name the engine honors that
// is MISSING from the list is worse: the form does not recognise the stored value, falls
// back to its own default and writes that over it. `deterministic` was missing for exactly
// that reason, which is how an LLM-free configuration could be turned into one that spends.
func TestModesAreTheModesRawStrategyOrderHonors(t *testing.T) {
	for _, m := range Modes {
		order := rawStrategyOrder(1000, Cfg{Mode: m})
		if len(order) == 0 {
			t.Fatalf("mode %q yields no strategy order", m)
		}
		if m == "auto" {
			continue
		}
		if order[0] != m {
			t.Errorf("mode %q is not honored: the order starts with %q", m, order[0])
		}
	}
	// And a name NOT on the list must fall through to auto — the mechanism that made a
	// missing name silent rather than loud.
	if got := rawStrategyOrder(1000, Cfg{Mode: "no-such-strategy"}); got[0] == "no-such-strategy" {
		t.Fatal("an unknown mode was honored, so the list is not the authority it claims to be")
	}
}
