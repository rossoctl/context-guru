package config

import "testing"

// The shipped strategies write the keep-alive defaults from the constants. The values are the
// shipped ones; a change is a behavior change.
func TestStrategiesWriteTheShippedKeepAliveDefaults(t *testing.T) {
	want := map[string]int{"keepalive_idle_seconds": 240, "keepalive_min_prefix_tokens": 80000, "keepalive_idle_seconds_1h": 3540}
	seen := 0
	for _, s := range Strategies {
		for _, k := range s.Cache {
			if w, ok := want[k.Key]; ok {
				seen++
				if k.Value != w {
					t.Errorf("%s %s = %v, want %d", s.Name, k.Key, k.Value, w)
				}
			}
		}
	}
	if seen != 15 {
		t.Errorf("checked %d keys, want 15 (5 strategies x 3)", seen)
	}
}
