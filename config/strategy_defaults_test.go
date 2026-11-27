package config

import "testing"

// The strategies write their numbers out on purpose (see the header of strategy.go). Three keys in
// each of the 5 strategies must equal the shipped values and the Default* constants. A drift fails
// here, and a person decides which side moves.
func TestStrategiesWriteTheShippedKeepAliveDefaults(t *testing.T) {
	want := map[string]int{"keepalive_idle_seconds": 240, "keepalive_min_prefix_tokens": 80000, "keepalive_idle_seconds_1h": 3540}
	if want["keepalive_idle_seconds"] != DefaultKeepAliveIdle || want["keepalive_min_prefix_tokens"] != DefaultKeepAliveMinPrefix ||
		want["keepalive_idle_seconds_1h"] != DefaultKeepAliveIdle1h {
		t.Fatal("a Default* constant moved from the shipped value; decide whether the strategies move too")
	}
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
