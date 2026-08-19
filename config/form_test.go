package config

import (
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/tenant"
)

// The bug this file exists for.
//
// The browser rewrote `pipeline:` in place with a flow-style line, which orphaned the items
// of a block sequence under it and produced a document the server then refused:
//
//	config: yaml: line 3: did not find expected key
//
// Two accounts hit that on every save, and could not get out of it from the UI: the refusal
// left the old document stored, so the next attempt mangled the same input again. Every
// shape below is a document the old writer corrupted or silently emptied.
func TestApplyFormSurvivesDocumentShapesTheOldWriterCorrupted(t *testing.T) {
	on := DefaultExtractLLMForm()
	on.PerOutput = true
	for name, doc := range map[string]string{
		"block sequence pipeline": "mode: sync\npreset: general\npipeline:\n  - format\n  - extract\n",
		"preset, no pipeline":     "preset: general\nmode: sync\n",
		"comment header":          "# my configuration\npipeline: [format]\nmode: sync\n",
		"components with no body": "pipeline: [format]\ncomponents:\nmode: sync\n",
		"empty document":          "",
		"the server default":      tenant.DefaultConfigYAML,
	} {
		t.Run(name, func(t *testing.T) {
			// The pipeline the UI posts is the RESOLVED one it was rendering, which is what
			// ParseForm hands it — so parse and apply are tested as the round trip they are.
			f, err := ParseForm(doc)
			if err != nil {
				t.Fatalf("ParseForm: %v", err)
			}
			f.ExtractLLM = &on
			out, err := ApplyForm(doc, f)
			if err != nil {
				t.Fatalf("ApplyForm: %v", err)
			}
			if err := Validate([]byte(out)); err != nil {
				t.Fatalf("produced a document that does not build: %v\n%s", err, out)
			}
			back, err := ParseForm(out)
			if err != nil {
				t.Fatalf("re-parse: %v", err)
			}
			if !back.ExtractLLM.PerOutput {
				t.Errorf("saved per_output: true and read it back off:\n%s", out)
			}
			if !contains(back.Pipeline, "extract_llm") {
				t.Errorf("extract_llm is configured but not in the pipeline:\n%s", out)
			}
		})
	}
}

// A preset document lost its entire pipeline on save: the regex found no flow-style line to
// read the existing names from, so it wrote `pipeline: [extract_llm]` and every other
// component silently stopped running. Nobody saw an error, which made it the worse of the
// two bugs.
func TestApplyFormKeepsAPresetsOtherComponents(t *testing.T) {
	f, err := ParseForm("preset: general\nmode: sync\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Pipeline) < 2 {
		t.Fatalf("the preset should resolve to a real pipeline, got %v", f.Pipeline)
	}
	on := DefaultExtractLLMForm()
	on.PerOutput = true
	f.ExtractLLM = &on
	out, err := ApplyForm("preset: general\nmode: sync\n", f)
	if err != nil {
		t.Fatal(err)
	}
	back, _ := ParseForm(out)
	for _, name := range f.Pipeline {
		if !contains(back.Pipeline, name) {
			t.Errorf("saving dropped %q from the pipeline:\n%s", name, out)
		}
	}
}

// Keys the form does not own are somebody's deliberate configuration. A map round trip
// preserves them for free; the string surgery it replaces had to special-case them, and only
// did so for one block.
func TestApplyFormPreservesUnmanagedConfiguration(t *testing.T) {
	doc := `pipeline: [format, extract_llm, extract]
mode: sync
components:
  extract_llm:
    strategy: code
    marker_mode: summary
    model:
      name: claude-haiku-4-5
    per_output: true
    cold_cache:
      enabled: true
      min_tokens: 1000
  extract:
    min_tokens: 400
store:
  ttl_seconds: 900
`
	f, err := ParseForm(doc)
	if err != nil {
		t.Fatal(err)
	}
	f.ExtractLLM.Aggressiveness = "high"
	out, err := ApplyForm(doc, f)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"strategy: code", "marker_mode: summary", "claude-haiku-4-5",
		"min_tokens: 400", "ttl_seconds: 900", "aggressiveness: high"} {
		if !strings.Contains(out, want) {
			t.Errorf("lost %q:\n%s", want, out)
		}
	}
}

// Both switches off means the component has nothing to do, so it leaves the pipeline rather
// than costing a pass over every request for nothing.
func TestApplyFormRemovesTheComponentWhenBothSwitchesAreOff(t *testing.T) {
	doc := "pipeline: [format, extract_llm, extract]\ncomponents:\n  extract_llm:\n    per_output: true\nmode: sync\n"
	f, _ := ParseForm(doc)
	f.ExtractLLM.PerOutput = false
	f.ExtractLLM.ColdEnabled = false
	out, err := ApplyForm(doc, f)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "extract_llm") {
		t.Errorf("still configured:\n%s", out)
	}
}

// Enum typos are a 400 naming the field, not a silently ignored key. extract_llm's own
// loader is NON-strict, so a bad value there does nothing at all — and this is the one
// component that spends money.
func TestApplyFormRejectsValuesTheComponentWouldSilentlyIgnore(t *testing.T) {
	base := "pipeline: [format]\nmode: sync\n"
	for name, mangle := range map[string]func(*Form){
		"aggressiveness": func(f *Form) { f.ExtractLLM.Aggressiveness = "aggressive" },
		"context":        func(f *Form) { f.ExtractLLM.Context = "last_n" },
		"mode":           func(f *Form) { f.Mode = "syncc" },
		"negative cap":   func(f *Form) { f.ExtractLLM.MaxPerSession = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			f, _ := ParseForm(base)
			f.ExtractLLM.PerOutput = true
			mangle(&f)
			if _, err := ApplyForm(base, f); err == nil {
				t.Fatal("accepted a value the component would ignore")
			}
		})
	}
}
