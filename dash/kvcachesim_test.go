package dash

import (
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/kvcache"
)

// The served assumptions report the read multiple as SET. Unset is 0, so the page shows no 0.1
// placeholder that a user could type to pin every model, and the formula text names the tiers.
func TestAssumptionsReadMultipleIsBlankWhenUnset(t *testing.T) {
	a := kvCacheAssumptions(KVCacheSimConfig{})
	if a.Multipliers.CacheRead != 0 {
		t.Errorf("unset read multiple = %v, want 0 (per model)", a.Multipliers.CacheRead)
	}
	if a.Multipliers.Write5m != 1.25 || a.Multipliers.Write1h != 2.0 {
		t.Errorf("write multiples = %+v", a.Multipliers)
	}
	if got := kvCacheAssumptions(KVCacheSimConfig{Multipliers: kvcache.Multipliers{CacheRead: 0.2}}).Multipliers.CacheRead; got != 0.2 {
		t.Errorf("set read multiple = %v, want 0.2", got)
	}
	found := false
	for _, f := range a.Formulas {
		if f.Name == "Cache read" && strings.Contains(f.Note, "0.05") {
			found = true
		}
	}
	if !found {
		t.Error("the cache-read formula text does not name the per-model tiers")
	}
}
