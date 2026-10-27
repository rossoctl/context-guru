package keepalive

import (
	"encoding/json"
	"testing"

	"github.com/rossoctl/context-guru/components/offload"
)

// Each carried-set counter the warm sweep keeps must reach keepalive.sweep_warm in /stats. They were counted
// but not published once, and nothing noticed. Each gets a different value, so a swapped field fails too.
func TestEveryCarriedCounterReachesTheSweepWarmStats(t *testing.T) {
	t.Cleanup(offload.SeedCarriedCountersForTest())
	w := sweepWarmStats()
	if w == nil {
		t.Fatal("sweepWarmStats() is nil with carried counters set")
	}
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]float64{
		"carried_merged": 1, "carried_overridden": 2, "carried_refused": 3, "carried_expired": 4, "carried_capped": 5,
	} {
		if v, ok := got[key].(float64); !ok || v != want {
			t.Errorf("sweep_warm.%s = %v, want %v", key, got[key], want)
		}
	}
}
