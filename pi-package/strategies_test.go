package pipackage

import (
	"encoding/json"
	"os/exec"
	"testing"

	"github.com/rossoctl/context-guru/config"
)

// dumpPiStrategies prints pi_plugin.STRATEGIES as JSON. python3 is required: a skip would prove
// nothing.
const dumpPiStrategies = `
import json, sys
sys.path.insert(0, "scripts")
import pi_plugin
print(json.dumps({k: {"cache": v["cache"], "spends": v["spends"]} for k, v in pi_plugin.STRATEGIES.items()}))
`

// TestPiStrategiesMatchTheProxyTable. pi_plugin.py keeps its own copy of the strategy keys, for a proxy
// older than the one that names strategies. One name means one behaviour, so each Pi strategy must set
// the same cache keys and values as the proxy's table (config/strategy.go), the one place that owns
// them. This replaced a check against the Claude Code plugin's settings.py, which no longer holds a
// table.
func TestPiStrategiesMatchTheProxyTable(t *testing.T) {
	out, err := exec.Command("python3", "-c", dumpPiStrategies).Output()
	if err != nil {
		t.Fatalf("python3 could not read pi_plugin.py: %v", err)
	}
	var pi map[string]struct {
		Cache  map[string]any `json:"cache"`
		Spends bool           `json:"spends"`
	}
	if err := json.Unmarshal(out, &pi); err != nil {
		t.Fatal(err)
	}
	if len(pi) == 0 {
		t.Fatal("no Pi strategies were read, so this guard proved nothing")
	}
	for name, spec := range pi {
		row, ok := config.LookupStrategy(name)
		if !ok {
			t.Errorf("%s: the proxy has no such strategy", name)
			continue
		}
		if row.Spends != spec.Spends {
			t.Errorf("%s: spends pi=%v proxy=%v", name, spec.Spends, row.Spends)
		}
		if len(row.Cache) != len(spec.Cache) {
			t.Errorf("%s: pi sets %d cache keys, the proxy %d", name, len(spec.Cache), len(row.Cache))
		}
		for _, k := range row.Cache {
			got, _ := json.Marshal(spec.Cache[k.Key])
			want, _ := json.Marshal(k.Value)
			if string(got) != string(want) {
				t.Errorf("%s.%s: pi %s, proxy %s", name, k.Key, got, want)
			}
		}
	}
}
