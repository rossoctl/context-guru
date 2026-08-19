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

// A bare `preset:` resolves to a pipeline that CONTAINS extract_llm and carries no
// per-component block at all. Reading "no block" as "switched off" showed such an account an
// empty form and then, on save, wrote a pipeline with the component removed — silently, which
// is the failure class this file exists to close.
func TestBarePresetDoesNotReadAsSwitchedOff(t *testing.T) {
	// Find a shipped preset whose pipeline includes extract_llm; if none does, there is
	// nothing to regress and the test says so rather than passing vacuously.
	var name string
	for p := range presetConfigs {
		c, err := LoadBytes([]byte("preset: " + p + "\n"))
		if err == nil && contains(c.Pipeline, "extract_llm") && len(c.Components) == 0 {
			name = p
			break
		}
	}
	if name == "" {
		for p := range presets {
			if contains(presets[p], "extract_llm") {
				name = p
				break
			}
		}
	}
	if name == "" {
		t.Skip("no shipped preset runs extract_llm without a block of its own")
	}
	doc := "preset: " + name + "\nmode: sync\n"
	f, err := ParseForm(doc)
	if err != nil {
		t.Fatal(err)
	}
	if !f.ExtractLLM.PerOutput {
		t.Errorf("preset %q runs extract_llm with per_output defaulting on, but the form reads it off", name)
	}
	// And a save that changes something unrelated must not drop it.
	f.ExtractLLM.Aggressiveness = "high"
	out, err := ApplyForm(doc, f)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(mustLoad(t, out).Pipeline, "extract_llm") {
		t.Errorf("saving dropped extract_llm from a preset that ran it:\n%s", out)
	}
}

// Switching the component off means "do not run it", not "forget how it was configured".
// Deleting the whole block took `model:` with it, so re-enabling it later ran on the
// expensive default — a form that quietly costs money.
func TestSwitchingOffKeepsTheKeysTheFormDoesNotOwn(t *testing.T) {
	doc := `pipeline: [format, extract_llm]
mode: sync
components:
  extract_llm:
    model:
      name: claude-haiku-4-5
    marker_mode: summary
    per_output: true
    strategy: code
`
	f, err := ParseForm(doc)
	if err != nil {
		t.Fatal(err)
	}
	f.ExtractLLM.PerOutput = false
	f.ExtractLLM.ColdEnabled = false
	out, err := ApplyForm(doc, f)
	if err != nil {
		t.Fatal(err)
	}
	if contains(mustLoad(t, out).Pipeline, "extract_llm") {
		t.Errorf("the component still runs:\n%s", out)
	}
	// strategy is a key the form OWNS now, so it is cleared like per_output. marker_mode
	// and model.name are not, and must survive.
	for _, want := range []string{"claude-haiku-4-5", "marker_mode: summary"} {
		if !strings.Contains(out, want) {
			t.Errorf("switching off destroyed %q:\n%s", want, out)
		}
	}
	for _, gone := range []string{"per_output", "strategy"} {
		if strings.Contains(out, gone) {
			t.Errorf("a key the form owns survived (%s):\n%s", gone, out)
		}
	}
}

// The form must not change a value it was only asked to display. `llm_max_per_session: 0`
// means UNLIMITED to the component; with a plain int it was indistinguishable from an unset
// key, so the form showed 20 and the next save wrote 20 over a deliberate "no cap".
func TestAZeroCapIsDisplayedAndPreserved(t *testing.T) {
	doc := "pipeline: [format, extract_llm]\nmode: sync\ncomponents:\n  extract_llm:\n" +
		"    per_output: true\n    llm_max_per_session: 0\n    llm_max_per_request: 0\n"
	f, err := ParseForm(doc)
	if err != nil {
		t.Fatal(err)
	}
	if f.ExtractLLM.MaxPerSession != 0 || f.ExtractLLM.MaxPerRequest != 0 {
		t.Fatalf("a stored 0 was displayed as %d/%d",
			f.ExtractLLM.MaxPerSession, f.ExtractLLM.MaxPerRequest)
	}
	out, err := ApplyForm(doc, f)
	if err != nil {
		t.Fatal(err)
	}
	back, _ := ParseForm(out)
	if back.ExtractLLM.MaxPerSession != 0 {
		t.Errorf("the round trip raised a deliberate no-cap to %d:\n%s", back.ExtractLLM.MaxPerSession, out)
	}
	// A zero SIZE threshold is different: it is a removed brake, not a setting.
	f.ExtractLLM.MinTokens = 0
	if _, err := ApplyForm(doc, f); err == nil {
		t.Error("accepted min_tokens: 0, which makes every output a candidate")
	}
}

// A document that does not load strictly still has to draw a usable form — but the form must
// say it is a guess, because with the YAML box gone a save from a misread form is the only
// way left to make things worse.
func TestAnUnloadableDocumentIsReportedNotHidden(t *testing.T) {
	f, err := ParseForm("pipeline: [format]\nmode: sync\nbogus_key_no_binding: 1\n")
	if err != nil {
		t.Fatalf("a document that fails the strict load must still produce a form: %v", err)
	}
	if f.ParseError == "" {
		t.Error("the form does not report that it came from a best-effort read")
	}
	if !strings.Contains(f.ParseError, "bogus_key_no_binding") {
		t.Errorf("the reported error does not name the offending key: %q", f.ParseError)
	}
	// A document that loads cleanly reports nothing, so the UI has a reliable signal.
	ok, err := ParseForm(tenant.DefaultConfigYAML)
	if err != nil || ok.ParseError != "" {
		t.Errorf("a healthy document reported a parse error: %q (%v)", ok.ParseError, err)
	}
}

// mustLoad is the strict loader, for the assertions that are about the DOCUMENT rather
// than about the form's reading of it.
func mustLoad(t *testing.T, doc string) *Config {
	t.Helper()
	c, err := LoadBytes([]byte(doc))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	return c
}

func mustParse(t *testing.T, doc string) Form {
	t.Helper()
	f, err := ParseForm(doc)
	if err != nil {
		t.Fatalf("ParseForm: %v", err)
	}
	return f
}

// osherDoc is a real stored document from the hosted service, the one whose extract_llm
// ran 251 times and acted zero times. It is the regression case for this whole file: the
// form has to SHOW why it is inert (model.source: config has no model on that deployment,
// and allow_on_caching_backend is absent so the gate hard-declines cached traffic) and it
// has to leave every key it does not own exactly where it found it.
const osherDoc = `components:
  extract:
    min_tokens: 400
  extract_llm:
    aggressiveness: medium
    cold_cache:
      enabled: true
      min_tokens: 1000
    context: recent
    context_messages: 7
    fire_on: pressure
    llm_every_n_requests: 1
    llm_max_per_request: 20
    llm_max_per_session: 80
    min_tokens: 1000
    model:
      source: config
    per_output: true
    strategy: code
    trigger:
      min_request_tokens: 3000
mode: sync
pipeline:
  - format
  - toon
  - dedup
  - failed_run
  - cmdfilter
  - extract_llm
  - extract
  - cachesplit
`

func TestTheFormShowsWhyARealAccountsExtractLLMWasInert(t *testing.T) {
	f, err := ParseForm(osherDoc)
	if err != nil {
		t.Fatalf("ParseForm: %v", err)
	}
	if f.ParseError != "" {
		t.Fatalf("a document the proxy runs must load strictly: %s", f.ParseError)
	}
	x := f.ExtractLLM
	// The two facts that made it inert, both invisible on the old form.
	if x.ModelSource != "config" {
		t.Errorf("model source: got %q, want config — the form must show the source that has no model here", x.ModelSource)
	}
	if x.AllowOnCachingBackend {
		t.Error("allow_on_caching_backend is absent in the document, so the form must show it OFF")
	}
	// And the knobs that were already there.
	for _, c := range []struct {
		name string
		got  any
		want any
	}{
		{"per_output", x.PerOutput, true},
		{"cold_enabled", x.ColdEnabled, true},
		{"size_trigger", x.SizeTrigger, false},
		{"min_tokens", x.MinTokens, 1000},
		{"max_per_request", x.MaxPerRequest, 20},
		{"max_per_session", x.MaxPerSession, 80},
		{"every_n_requests", x.EveryNRequests, 1},
		{"trigger_min_tokens", x.TriggerMinTokens, 3000},
		{"strategy", x.Strategy, "code"},
		{"aggressiveness", x.Aggressiveness, "medium"},
		{"context", x.Context, "recent"},
		{"context_messages", x.ContextMessages, 7},
		{"cold_min_tokens", x.ColdMinTokens, 1000},
	} {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
}

// The direction that actually bites: a field the page offers must reach the document, and
// a save must not quietly drop a key the form has no control for. Every field is moved off
// its parsed value here, so a field the writer forgot shows up as an unchanged value.
func TestEveryFormFieldReachesTheDocumentAndNothingElseMoves(t *testing.T) {
	f, err := ParseForm(osherDoc)
	if err != nil {
		t.Fatal(err)
	}
	x := f.ExtractLLM
	x.ModelSource = "incoming"
	x.AllowOnCachingBackend = true
	x.SizeTrigger = true
	x.Strategy = "auto"
	x.MinTokens = 1500
	x.MaxPerRequest = 3
	x.MaxPerSession = 40
	x.EveryNRequests = 2
	x.TriggerMinTokens = 5000
	x.Aggressiveness = "high"
	x.Context = "full"
	x.ContextMessages = 9
	x.ColdMinTokens = 800

	out, err := ApplyForm(osherDoc, f)
	if err != nil {
		t.Fatalf("ApplyForm: %v", err)
	}
	blk := mustLoad(t, out).Components["extract_llm"]
	var got extractLLMDoc
	if err := blk.Decode(&got); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		got  any
		want any
	}{
		{"model.source", got.Model.Source, "incoming"},
		{"allow_on_caching_backend", *got.AllowOnCaching, true},
		{"fire_on", got.FireOn, "size"},
		{"strategy", got.Strategy, "auto"},
		{"min_tokens", *got.MinTokens, 1500},
		{"llm_max_per_request", *got.MaxPerRequest, 3},
		{"llm_max_per_session", *got.MaxPerSession, 40},
		{"llm_every_n_requests", *got.EveryN, 2},
		{"trigger.min_request_tokens", *got.Trigger.MinRequestTokens, 5000},
		{"aggressiveness", got.Aggressiveness, "high"},
		{"context", got.Context, "full"},
		{"context_messages", *got.ContextMessages, 9},
		{"cold_cache.min_tokens", *got.ColdCache.MinTokens, 800},
	} {
		if c.got != c.want {
			t.Errorf("%s did not reach the document: got %v, want %v", c.name, c.got, c.want)
		}
	}
	// And a re-read comes back with what was written: the form is the same shape in both
	// directions, which is what "the dashboard shows my configuration" means.
	back, err := ParseForm(out)
	if err != nil {
		t.Fatal(err)
	}
	if *back.ExtractLLM != *x {
		t.Errorf("round trip changed the fields:\n got %+v\nwant %+v", *back.ExtractLLM, *x)
	}
	// Unowned keys, still where they were.
	if got := mustLoad(t, out).Components["extract"]; got.IsZero() {
		t.Error("another component's block was dropped")
	}
}

// Switching the component off must not delete the model or trigger blocks: they are the
// keys a manager set, and re-enabling on the expensive default model is a form that costs
// money.
func TestSwitchingOffKeepsTheModelAndTriggerBlocks(t *testing.T) {
	f, err := ParseForm(osherDoc)
	if err != nil {
		t.Fatal(err)
	}
	f.ExtractLLM.PerOutput = false
	f.ExtractLLM.ColdEnabled = false
	out, err := ApplyForm(osherDoc, f)
	if err != nil {
		t.Fatal(err)
	}
	blk := mustLoad(t, out).Components["extract_llm"]
	var got extractLLMDoc
	if err := blk.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Model.Source != "config" {
		t.Errorf("model.source was lost on switch-off: %q", got.Model.Source)
	}
	if got.Trigger.MinRequestTokens == nil || *got.Trigger.MinRequestTokens != 3000 {
		t.Error("trigger.min_request_tokens was lost on switch-off")
	}
	if got.Strategy != "" {
		t.Errorf("strategy is a managed key and should have been cleared, got %q", got.Strategy)
	}
	if contains(mustLoad(t, out).Pipeline, "extract_llm") {
		t.Error("the component is still in the pipeline")
	}
}
