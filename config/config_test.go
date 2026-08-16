package config

import (
	"strings"
	"testing"
)

func TestLoadStrictRejectsUnknownFields(t *testing.T) {
	_, err := LoadBytes([]byte("pipeline: [format]\nbogus_key: 1\n"))
	if err == nil || !strings.Contains(err.Error(), "bogus_key") {
		t.Fatalf("expected strict rejection of unknown field, got %v", err)
	}
}

func TestPresetExpandsPipeline(t *testing.T) {
	c, err := LoadBytes([]byte("preset: balanced\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Pipeline) == 0 || c.Pipeline[0] != "format" {
		t.Fatalf("preset did not expand pipeline: %v", c.Pipeline)
	}
}

func TestExplicitPipelineOverridesPreset(t *testing.T) {
	c, err := LoadBytes([]byte("preset: balanced\npipeline: [cacheinject]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Pipeline) != 1 || c.Pipeline[0] != "cacheinject" {
		t.Fatalf("explicit pipeline should win: %v", c.Pipeline)
	}
}

func TestUnknownPresetErrors(t *testing.T) {
	if _, err := LoadBytes([]byte("preset: nope\n")); err == nil {
		t.Fatal("expected error for unknown preset")
	}
}

// TestRichPresetCarriesComponentConfig verifies the codesmart preset expands to both
// its pipeline AND its tuned per-component config (which a bare name-list can't carry) —
// specifically that extract_llm is routed to the cheap "config" model, not the default.
func TestRichPresetCarriesComponentConfig(t *testing.T) {
	c, err := LoadBytes([]byte("preset: codesmart\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"format", "toon", "dedup", "failed_run", "cmdfilter", "extract_llm", "extract", "cachesplit"}
	if strings.Join(c.Pipeline, ",") != strings.Join(want, ",") {
		t.Fatalf("codesmart pipeline = %v, want %v", c.Pipeline, want)
	}
	node, ok := c.Components["extract_llm"]
	if !ok {
		t.Fatal("codesmart must carry extract_llm component config")
	}
	var got struct {
		Model struct {
			Source string `yaml:"source"`
		} `yaml:"model"`
		MinTokens int `yaml:"min_tokens"`
	}
	if err := node.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Model.Source != "config" {
		t.Fatalf("extract_llm.model.source = %q, want config (cheap model)", got.Model.Source)
	}
	if got.MinTokens != 3000 {
		t.Fatalf("extract_llm.min_tokens = %d, want 3000", got.MinTokens)
	}
}

// TestRichPresetUserOverrideWins: an explicit component config overrides the preset's.
func TestRichPresetUserOverrideWins(t *testing.T) {
	c, err := LoadBytes([]byte("preset: codesmart\ncomponents:\n  extract_llm:\n    min_tokens: 9999\n"))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		MinTokens int `yaml:"min_tokens"`
	}
	node := c.Components["extract_llm"]
	if err := node.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.MinTokens != 9999 {
		t.Fatalf("user min_tokens should win: got %d, want 9999", got.MinTokens)
	}
	// and codesafe (deterministic-only) must contain NO extract_llm.
	cs, err := LoadBytes([]byte("preset: codesafe\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range cs.Pipeline {
		if name == "extract_llm" {
			t.Fatal("codesafe must be deterministic-only (no extract_llm)")
		}
	}
}

func TestStoreOptionsParse(t *testing.T) {
	c, err := LoadBytes([]byte("store: {ttl_seconds: 60, max_entries: 5}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Store.TTLSeconds != 60 || c.Store.MaxEntries != 5 {
		t.Fatalf("store options not parsed: %+v", c.Store)
	}
}

// cacheinject is deliberately absent from every default preset (#32). Its breakpoints
// only started reaching the provider with that fix, and placement has never been shown
// to help — so it must not be on by default. `cachesplit` carries the volatile-tail
// split, which IS measured, so disabling placement does not disable the split with it.
func TestNoPresetEnablesCacheinjectByDefault(t *testing.T) {
	for name := range presets {
		p, _ := PresetPipeline(name)
		for _, c := range p {
			if c == "cacheinject" {
				t.Errorf("preset %q enables cacheinject; placement is unmeasured and must be opt-in", name)
			}
		}
	}
	// The split must still be on in the recommended presets, or dropping cacheinject
	// would be a cost regression rather than a neutral default change.
	for _, name := range []string{"general", "codesmart", "codesafe", "balanced"} {
		p, _ := PresetPipeline(name)
		found := false
		for _, c := range p {
			if c == "cachesplit" {
				found = true
			}
		}
		if !found {
			t.Errorf("preset %q lost the volatile-tail split", name)
		}
	}
}
