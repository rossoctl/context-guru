package kvcache

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The JSON keys of this package's types are a CROSS-BOUNDARY CONTRACT, and this pins them.
//
// # Why a golden key set rather than trusting review
//
// Nothing in this package renders anything. Every type below is serialized and read by a
// surface owned by someone else — the dashboard's KV-cache page and the offline evaluator in
// deploy/harbor. A field renamed here does not break their build and does not fail their
// tests: the consumer simply dereferences a key that is no longer there, gets `undefined`,
// and renders an empty cell. That reads as a styling bug, not a contract break, and it is
// nobody's fault in particular, which is why it survives.
//
// It has already happened once on this feature, one layer up: KVCacheFormula was rebuilt with
// tags {name, expression, prose} while the page read {name, formula, note}. `name` matched by
// luck, so eleven formula headings rendered above eleven EMPTY code boxes. Two tests passed
// through it — one asserting the payload carried at least eight formulas, one asserting the
// page did not hardcode them. Each was right about its own half of a contract neither checked
// across. This is that check, for this side of the boundary.
//
// # What to do when this test fails
//
// It is not a test to "fix" by pasting the new keys in. A red line here means a consumer is
// about to render a blank cell:
//
//   - Renaming or removing a key is a BREAKING change. Update the consumers in the same
//     commit — dash's read layer and page, and deploy/harbor/kv_ttl_cost_model.py — then
//     update the golden set.
//   - ADDING a key is additive and safe. Add it to the golden set, and tell whoever owns the
//     page, or it ships as a field nothing reads.
var wireKeys = map[string][]string{
	"Request": {"id", "user", "conversation_id", "ts", "hour_utc", "bucket", "model",
		"provider", "agent", "input_tokens", "output_tokens", "cache_read_tokens",
		"cache_write_tokens", "cache_write_1h_tokens", "cached_context_tokens", "ttl",
		"ttl_source", "hit", "miss_reason", "next_ts", "next_id", "has_next", "idle_ms",
		"within_5m", "within_1h", "upstream_ms", "cost_usd", "cost_known", "keepalive"},
	"Result": {"strategy", "description", "requests", "conversations", "total_usd",
		"fresh_input_usd", "cache_read_usd", "cache_write_usd", "output_usd", "ping_usd",
		"uncached_usd", "cache_premium_usd", "hits", "misses", "hit_rate_pct", "miss_rate_pct",
		"forced_misses", "pings", "pings_that_rewrote", "pings_that_upgraded",
		"pings_on_open_spans", "writes_5m", "writes_1h", "expires", "avoided_recomputations",
		"avoided_tokens", "retained_ms", "unpriced", "valued", "decisions", "stats_levels",
		"observed_coverage", "by_user", "by_model", "latency"},
	"Savings": {"strategy", "baseline", "baseline_usd", "strategy_usd", "absolute_usd",
		"percent_usd", "percent_known", "hit_delta", "latency_avoided_ms", "latency_known"},
	"Group": {"key", "requests", "total_usd", "hits", "misses", "hit_rate_pct", "pings",
		"ping_usd", "writes_5m", "writes_1h", "unpriced"},
	"Latency":      {"per_miss_ms", "hit_n", "miss_n", "hit_mean_ms", "miss_mean_ms", "known"},
	"Coverage":     {"recorded", "assumed"},
	"Pricing":      {"model", "input", "output", "cache_read", "write_5m", "write_1h", "ping_input_tokens", "ping_output_tokens", "source", "known"},
	"Override":     {"input", "output", "cache_read", "write_5m", "write_1h", "ping_input_tokens", "ping_output_tokens"},
	"Multipliers":  {"cache_read", "write_5m", "write_1h"},
	"Semantics":    {"hit_refreshes_ttl", "ping_refreshes_ttl", "zero_generation"},
	"PriceList":    {"multipliers", "models"},
	"StrategySpec": {"name", "description", "unreachable", "needs_dataset", "baseline"},
}

// wireTypes is one zero value per contract type, so the reflection below cannot drift from the
// golden map by a type being added and forgotten.
func wireTypes() []any {
	return []any{Request{}, Result{}, Savings{}, Group{}, Latency{}, Coverage{}, Pricing{},
		Override{}, Multipliers{}, Semantics{}, PriceList{}, StrategySpec{}}
}

func TestTheWireContractIsUnchanged(t *testing.T) {
	seen := map[string]bool{}
	for _, v := range wireTypes() {
		rt := reflect.TypeOf(v)
		name := rt.Name()
		seen[name] = true
		want, ok := wireKeys[name]
		if !ok {
			t.Errorf("%s is serialized to a consumer and has no entry in wireKeys; add one, "+
				"and tell whoever owns the page", name)
			continue
		}
		var got []string
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			tag := f.Tag.Get("json")
			if tag == "-" {
				continue
			}
			if tag == "" {
				// An exported field with NO tag is serialized under its Go name, which is a
				// key nobody outside this package would predict.
				if f.IsExported() {
					t.Errorf("%s.%s is exported with no json tag, so it ships as %q — give it "+
						"an explicit key or tag it `json:\"-\"`", name, f.Name, f.Name)
				}
				continue
			}
			got = append(got, strings.Split(tag, ",")[0])
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s's JSON keys changed.\n  got:  %v\n  want: %v\n\n"+
				"A renamed or removed key does NOT break the consumer's build — it renders a "+
				"blank cell, which reads as a styling bug. Update dash's read layer and page "+
				"and deploy/harbor/kv_ttl_cost_model.py in the same commit, THEN update "+
				"wireKeys. An added key is safe: add it here and tell whoever owns the page.",
				name, got, want)
		}
	}
	for name := range wireKeys {
		if !seen[name] {
			t.Errorf("wireKeys has an entry for %s, which wireTypes no longer covers", name)
		}
	}
}

// The keys must also survive an actual round trip through encoding/json, not merely match the
// struct tags: a tag this test reads and a key the encoder emits are two different things once
// embedding, `omitempty` on a struct, or a custom marshaller is involved.
func TestTheWireContractSurvivesARoundTrip(t *testing.T) {
	reqs, cfg := dataset(t)
	res := Simulate(reqs, Fixed5m(), cfg)
	if !res.Valued {
		t.Fatal("the fixture is unpriced, so the payload below carries no cost keys to check")
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	// omitempty drops a zero value, so only the keys that CANNOT be empty on a real replay are
	// required here — asserting all of them would fail on a legitimately absent field and
	// teach whoever hits it to weaken the check.
	for _, k := range []string{"strategy", "requests", "conversations", "total_usd",
		"cache_write_usd", "hits", "misses", "hit_rate_pct", "valued", "decisions",
		"stats_levels", "by_user", "by_model", "latency"} {
		if _, ok := got[k]; !ok {
			present := make([]string, 0, len(got))
			for k := range got {
				present = append(present, k)
			}
			sort.Strings(present)
			t.Errorf("a real replay's JSON has no %q key; the page reads it. Present: %v",
				k, present)
		}
	}
	// Savings too: percent_known is the field that stops a zero baseline reading as 0%.
	sv, err := json.Marshal(Compare(res, res))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"absolute_usd", "percent_known"} {
		if !strings.Contains(string(sv), `"`+k+`"`) {
			t.Errorf("Savings JSON is missing %q: %s", k, sv)
		}
	}
}
