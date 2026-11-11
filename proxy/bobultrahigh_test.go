package proxy

import (
	"reflect"
	"testing"

	"github.com/rossoctl/context-guru/config"
)

// decodedComponents is each component's settings as plain values, so two presets compare with DeepEqual.
func decodedComponents(t *testing.T, c *config.Config) map[string]any {
	t.Helper()
	out := map[string]any{}
	for name, n := range c.Components {
		var v any
		if err := n.Decode(&v); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out[name] = v
	}
	return out
}

// The Bob plugin's bob-ultrahigh preset is ultrahigh with exactly 4 Bob changes (#1030), so it
// cannot drift from ultrahigh unseen:
//  1. extract_llm names no model. Bob does not serve claude-haiku-4-5; the request's own model is used.
//  2. extract_llm runs on a caching backend. Bob bills a cache rewrite at the input rate.
//  3. the sweep may apply on a warm cache (#720).
//  4. toolfilter keeps Bob's 15 core tools.
func TestBobUltrahighIsUltrahighWithTheFourBobChanges(t *testing.T) {
	u, err := config.LoadBytes([]byte("preset: ultrahigh\n"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := config.LoadBytes(bobPreset(t))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(b.Pipeline, u.Pipeline) {
		t.Fatalf("pipeline = %v, want ultrahigh's %v", b.Pipeline, u.Pipeline)
	}
	want := decodedComponents(t, u)
	llm := want["extract_llm"].(map[string]any)
	llm["model"] = map[string]any{"source": "incoming"}
	llm["allow_on_caching_backend"] = true
	want["extract_llm_sweep"].(map[string]any)["warm"] = true
	keep := make([]any, len(bobKeep))
	for i, k := range bobKeep {
		keep[i] = k
	}
	want["toolfilter"].(map[string]any)["keep"] = keep
	if got := decodedComponents(t, b); !reflect.DeepEqual(got, want) {
		t.Fatalf("components =\n%v\nwant ultrahigh with the 4 Bob changes =\n%v", got, want)
	}
}
