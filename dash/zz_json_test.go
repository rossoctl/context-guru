package dash

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEventJSONFlattensMeta(t *testing.T) {
	f := 0.7
	b, err := json.Marshal(&Event{Meta: Meta{ReasoningEffort: "high", Temperature: &f, CacheBPSystem: 2, StopReason: "end_turn"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"reasoning_effort":"high"`, `"temperature":0.7`, `"cache_bp_system":2`, `"stop_reason":"end_turn"`} {
		if !strings.Contains(string(b), k) {
			t.Fatalf("missing %s in %s", k, b)
		}
	}
	if strings.Contains(string(b), `"Meta"`) {
		t.Fatalf("Meta was nested rather than flattened: %s", b)
	}
	var unset Event
	ub, _ := json.Marshal(&unset)
	if !strings.Contains(string(ub), `"temperature":null`) {
		t.Fatalf("an unset temperature must serialize as null, not 0: %s", ub)
	}
}
