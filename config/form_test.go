package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/tenant"
	"gopkg.in/yaml.v3"
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
// shape in the table below is a document the old writer corrupted or silently emptied.
//
// The second half of the file is about the failure the FIRST fix still had: the form was
// hand-written, so it covered 18 keys of about a hundred and nothing could notice the gap.
// Those tests are generated from the declarations, which is the only way a form can be
// trusted to reach every key it draws.
func TestApplyFormSurvivesDocumentShapesTheOldWriterCorrupted(t *testing.T) {
	// One representative field per type, on the component that spends money. The full
	// per-field sweep runs on ONE canonical document below; running it against every
	// hostile shape too would be ~700 cases for no extra coverage.
	perturb := map[string]any{
		"per_output": true, "strategy": "single", "min_tokens": 1234, "model.model": "cg-test-model",
	}
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
			f := mustParse(t, doc)
			if f.Components == nil {
				f.Components = map[string]map[string]any{}
			}
			f.Components["extract_llm"] = clone(perturb)
			out, err := ApplyForm(doc, f)
			if err != nil {
				t.Fatalf("ApplyForm: %v", err)
			}
			if err := Validate([]byte(out)); err != nil {
				t.Fatalf("produced a document that does not build: %v\n%s", err, out)
			}
			back := mustParse(t, out)
			if !reflect.DeepEqual(back.Components["extract_llm"], perturb) {
				t.Errorf("round trip changed the fields:\n got %v\nwant %v\n%s",
					back.Components["extract_llm"], perturb, out)
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
	const doc = "preset: general\nmode: sync\n"
	f := mustParse(t, doc)
	if len(f.Pipeline) < 2 {
		t.Fatalf("the preset should resolve to a real pipeline, got %v", f.Pipeline)
	}
	f.Components = map[string]map[string]any{"extract_llm": {"per_output": true}}
	out, err := ApplyForm(doc, f)
	if err != nil {
		t.Fatal(err)
	}
	back := mustParse(t, out)
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
      model: claude-haiku-4-5
    per_output: true
    cold_cache:
      enabled: true
      min_tokens: 1000
  extract:
    min_tokens: 400
store:
  ttl_seconds: 900
`
	f := mustParse(t, doc)
	f.Components["extract_llm"]["aggressiveness"] = "high"
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

// Both switches off means the component has nothing to do — its own constructor refuses
// that combination outright — so it leaves the pipeline rather than costing a pass over
// every request for nothing. This is the ONE per-component coupling the form has.
func TestApplyFormRemovesTheComponentWhenBothSwitchesAreOff(t *testing.T) {
	doc := "pipeline: [format, extract_llm, extract]\ncomponents:\n  extract_llm:\n    per_output: true\nmode: sync\n"
	f := mustParse(t, doc)
	f.Components["extract_llm"]["per_output"] = false
	out, err := ApplyForm(doc, f)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "extract_llm") {
		t.Errorf("still configured:\n%s", out)
	}
}

// Enum typos and out-of-range numbers are a 400 naming the field, not a silently ignored
// key. Per-component blocks are strict now, so a bad value would refuse the whole document
// on the next build — which is a worse place to find out than the save that caused it.
func TestApplyFormRejectsValuesTheComponentWouldRefuse(t *testing.T) {
	base := "pipeline: [format, extract_llm]\nmode: sync\ncomponents:\n  extract_llm:\n    per_output: true\n"
	for name, mangle := range map[string]func(*Form){
		"enum":              func(f *Form) { f.Components["extract_llm"]["aggressiveness"] = "aggressive" },
		"enum, other field": func(f *Form) { f.Components["extract_llm"]["context"] = "last_n" },
		"mode":              func(f *Form) { f.Mode = "syncc" },
		"negative cap":      func(f *Form) { f.Components["extract_llm"]["llm_max_per_session"] = -1 },
		"zero threshold":    func(f *Form) { f.Components["extract_llm"]["min_tokens"] = 0 },
		"wrong type":        func(f *Form) { f.Components["extract_llm"]["per_output"] = "yes" },
		"undeclared key":    func(f *Form) { f.Components["extract_llm"]["min_tokns"] = 5000 },
		"unknown component": func(f *Form) { f.Components["no_such_component"] = map[string]any{} },
	} {
		t.Run(name, func(t *testing.T) {
			f := mustParse(t, base)
			mangle(&f)
			if _, err := ApplyForm(base, f); err == nil {
				t.Fatal("accepted a value the component would refuse")
			}
		})
	}
}

// A bare `preset:` resolves to a pipeline that CONTAINS extract_llm and carries no
// per-component block at all. Reading "no block" as "switched off" showed such an account an
// empty form and then, on save, wrote a pipeline with the component removed — silently, which
// is the failure class this file exists to close. Enablement is pipeline membership, full
// stop (R2).
func TestBarePresetDoesNotReadAsSwitchedOff(t *testing.T) {
	var name string
	for p := range presets {
		if _, rich := presetConfigs[p]; rich {
			continue // a rich preset ships tuned blocks; this is about the bare case
		}
		if contains(presets[p], "extract_llm") {
			name = p
			break
		}
	}
	if name == "" {
		t.Skip("no shipped preset runs extract_llm")
	}
	doc := "preset: " + name + "\nmode: sync\n"
	f := mustParse(t, doc)
	if !contains(f.Pipeline, "extract_llm") {
		t.Fatalf("preset %q runs extract_llm but the form does not show it enabled", name)
	}
	if len(f.Components["extract_llm"]) != 0 {
		t.Errorf("a bare preset states no keys, but the form claims %v", f.Components["extract_llm"])
	}
	// And a save that changes something unrelated must not drop it.
	f.Components = map[string]map[string]any{"extract_llm": {"aggressiveness": "high"}}
	out, err := ApplyForm(doc, f)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(mustLoad(t, out).Pipeline, "extract_llm") {
		t.Errorf("saving dropped extract_llm from a preset that ran it:\n%s", out)
	}
}

// The form must not change a value it was only asked to display. `llm_max_per_session: 0`
// means UNLIMITED to the component; a form that prefilled its own defaults could not tell
// that from an unset key, so it showed 20 and the next save wrote 20 over a deliberate
// "no cap" (R3). Only keys the document really states are on the form.
func TestAZeroCapIsDisplayedAndPreserved(t *testing.T) {
	doc := "pipeline: [format, extract_llm]\nmode: sync\ncomponents:\n  extract_llm:\n" +
		"    per_output: true\n    llm_max_per_session: 0\n    llm_max_per_request: 0\n"
	f := mustParse(t, doc)
	if f.Components["extract_llm"]["llm_max_per_session"] != 0 ||
		f.Components["extract_llm"]["llm_max_per_request"] != 0 {
		t.Fatalf("a stored 0 was displayed as %v", f.Components["extract_llm"])
	}
	out, err := ApplyForm(doc, f)
	if err != nil {
		t.Fatal(err)
	}
	if back := mustParse(t, out); back.Components["extract_llm"]["llm_max_per_session"] != 0 {
		t.Errorf("the round trip raised a deliberate no-cap to %v:\n%s",
			back.Components["extract_llm"]["llm_max_per_session"], out)
	}
	// A zero SIZE threshold is different: it is a removed brake, not a setting. That
	// distinction is Field.Min, i.e. data, not two hand-written maps of field names.
	f.Components["extract_llm"]["min_tokens"] = 0
	if _, err := ApplyForm(doc, f); err == nil {
		t.Error("accepted min_tokens: 0, which makes every output a candidate")
	}
}

// A document that does not load strictly still has to draw a usable form — but the form must
// say it is a guess, because with the YAML box gone a save from a misread form is the only
// way left to make things worse (R1).
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
	if !contains(f.Pipeline, "format") {
		t.Error("the best-effort read produced no pipeline, so the page has nothing to draw")
	}
	// A document that loads cleanly reports nothing, so the UI has a reliable signal.
	ok, err := ParseForm(tenant.DefaultConfigYAML)
	if err != nil || ok.ParseError != "" {
		t.Errorf("a healthy document reported a parse error: %q (%v)", ok.ParseError, err)
	}
	// A mistyped key inside a component block is the same class now that blocks are
	// strict, and it is the case the old form could not see at all.
	typo, err := ParseForm("pipeline: [dedup]\ncomponents:\n  dedup:\n    min_tokns: 5000\n")
	if err != nil {
		t.Fatal(err)
	}
	if typo.ParseError == "" {
		t.Error("a mistyped per-component key is not reported, so the page would draw it as fact")
	}
}

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
	if f.Components == nil {
		f.Components = map[string]map[string]any{}
	}
	return f
}

func clone(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
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
      min_idle_seconds: 120
      max_calls: 6
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
store:
  ttl_seconds: 900
`

func TestTheFormShowsWhyARealAccountsExtractLLMWasInert(t *testing.T) {
	f := mustParse(t, osherDoc)
	if f.ParseError != "" {
		t.Fatalf("a document the proxy runs must load strictly: %s", f.ParseError)
	}
	x := f.Components["extract_llm"]
	// The two facts that made it inert, both invisible on the old form.
	if x["model.source"] != "config" {
		t.Errorf("model source: got %v, want config — the form must show the source that has no model here", x["model.source"])
	}
	if _, ok := x["allow_on_caching_backend"]; ok {
		t.Error("allow_on_caching_backend is absent in the document, so the form must show it as unset (default FALSE), not as a value")
	}
	for key, want := range map[string]any{
		"per_output": true, "cold_cache.enabled": true, "fire_on": "pressure",
		"min_tokens": 1000, "llm_max_per_request": 20, "llm_max_per_session": 80,
		"llm_every_n_requests": 1, "trigger.min_request_tokens": 3000, "strategy": "code",
		"aggressiveness": "medium", "context": "recent", "context_messages": 7,
		"cold_cache.min_tokens": 1000, "cold_cache.min_idle_seconds": 120, "cold_cache.max_calls": 6,
	} {
		if x[key] != want {
			t.Errorf("%s: got %v, want %v", key, x[key], want)
		}
	}
	// And the whole document is now reachable, not just this component's block.
	if f.Components["extract"]["min_tokens"] != 400 {
		t.Error("another component's block is not on the form, which is the 16-percent-coverage bug")
	}
}

// canonicalDoc runs EVERY registered component, so the sweep below can perturb every
// declared field of every component and still be exercising a document the proxy builds.
// cold_cache.enabled is on because per_output: false with the sweep off is a combination the
// component refuses — the one coupling the form has.
func canonicalDoc(t *testing.T) string {
	t.Helper()
	names := components.Names()
	m := map[string]any{
		"mode":     "sync",
		"pipeline": names,
		"components": map[string]any{
			"extract_llm": map[string]any{"cold_cache": map[string]any{"enabled": true}},
		},
		"store": map[string]any{"ttl_seconds": 900},
	}
	b, err := yaml.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(b); err != nil {
		t.Fatalf("the canonical document does not build, so the sweep would test nothing: %v\n%s", err, b)
	}
	return string(b)
}

// TestEveryDeclaredFieldReachesTheDocumentAndNothingElseMoves is the point of the whole
// descriptor exercise, and it replaces thirteen hand-listed assertions about one component.
//
// For every registered component and every field it declares: move the value off what the
// document says, save, and check four things.
//
//	(a) it arrived at the dotted path it declares;
//	(b) a diff of the WHOLE document against an unperturbed save shows that path as the only
//	    change — this is what catches a save that quietly deletes a neighbouring key, the
//	    class R7 was (the disable path deleted cold_cache wholesale);
//	(c) re-parsing reads the value back, which catches the R8 class: a value the form does
//	    not recognise gets replaced by the form's own default on the next save;
//	(d) the result still builds — ApplyForm's own strict Validate, on every single case.
func TestEveryDeclaredFieldReachesTheDocumentAndNothingElseMoves(t *testing.T) {
	doc := canonicalDoc(t)
	base := mustParse(t, doc)
	baseline, err := ApplyForm(doc, base)
	if err != nil {
		t.Fatalf("the unperturbed save failed: %v", err)
	}
	for name, decls := range components.AllFields() {
		for _, fd := range decls {
			for i, v := range perturbations(fd, base.Components[name][fd.Key]) {
				t.Run(fmt.Sprintf("%s/%s/%d", name, fd.Key, i), func(t *testing.T) {
					f := mustParse(t, doc)
					if f.Components[name] == nil {
						f.Components[name] = map[string]any{}
					}
					f.Components[name][fd.Key] = v
					out, err := ApplyForm(doc, f)
					if err != nil {
						t.Fatalf("%s.%s = %v: %v", name, fd.Key, v, err)
					}
					// (a) + (b): the only thing that moved is this path.
					want := "components." + name + "." + fd.Key
					got := changedPaths(t, baseline, out)
					// A list field changes as its elements (`filters[0]`), which is the same
					// path.
					if len(got) == 0 || !allUnder(got, want) {
						t.Fatalf("setting %s changed %v, want only that path\n%s", want, got, out)
					}
					// (c) the form reads back what it wrote — except a secret, which is
					// write-only on purpose (asserted in its own test below).
					back := mustParse(t, out)
					if !fd.Secret && !reflect.DeepEqual(back.Components[name][fd.Key], v) {
						t.Fatalf("%s read back as %#v, want %#v", want, back.Components[name][fd.Key], v)
					}
					// Idempotence: re-saving what was just read must be a no-op, or the
					// settings page rewrites the document every time it is opened.
					again, err := ApplyForm(out, back)
					if err != nil {
						t.Fatalf("re-saving the parsed form failed: %v", err)
					}
					if again != out {
						t.Fatalf("ApplyForm(out, ParseForm(out)) != out\n got %s\nwant %s", again, out)
					}
				})
			}
		}
	}
}

// perturbations returns values that are definitely DIFFERENT from cur, per declared type.
func perturbations(fd components.Field, cur any) []any {
	switch fd.Type {
	case components.FieldBool:
		b, _ := cur.(bool)
		return []any{!b}
	case components.FieldEnum:
		var out []any
		for _, o := range fd.Options {
			if o != cur {
				out = append(out, o)
			}
		}
		return out
	case components.FieldInt:
		n, ok := asInt(cur)
		if !ok {
			n, _ = asInt(fd.Default)
		}
		if n+1 < fd.Min {
			return []any{fd.Min}
		}
		return []any{n + 1}
	case components.FieldFloat:
		x, _ := asFloat(cur)
		if x == 0 {
			x, _ = asFloat(fd.Default)
		}
		return []any{x + 0.1}
	case components.FieldString:
		return []any{"cg-form-sentinel"}
	case components.FieldStrings:
		// A string LIST is validated by the component that owns it, so one sentinel cannot
		// serve every such field: a filter list has to be a valid DSL document, and
		// toolfilter's removal list has to be a declaration NAME (it rejects anything else
		// so a junk entry is a 400 on the settings page rather than a filter that silently
		// matches nothing). Keyed by the field, because a value valid for both would have to
		// satisfy two grammars that share nothing.
		if fd.Key == "remove" {
			return []any{[]string{"cg-form-sentinel"}}
		}
		return []any{[]string{"schema_version: 1\nfilters:\n  cgsentinel:\n    description: sentinel\n    match: 'cg-form-sentinel'\n"}}
	}
	return nil
}

// changedPaths flattens both documents to leaf paths and returns the paths that differ.
// Whole-document, so a save that damages an unrelated block cannot hide behind an assertion
// list that only looks where the author thought to look.
func changedPaths(t *testing.T, a, b string) []string {
	t.Helper()
	fa, fb := flatten(t, a), flatten(t, b)
	seen := map[string]bool{}
	var out []string
	for k, v := range fa {
		if fb[k] != v {
			seen[k] = true
		}
	}
	for k, v := range fb {
		if fa[k] != v {
			seen[k] = true
		}
	}
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// allUnder reports whether every changed path IS the wanted path or an element of it.
func allUnder(got []string, want string) bool {
	for _, g := range got {
		if g != want && !strings.HasPrefix(g, want+"[") {
			return false
		}
	}
	return true
}

func flatten(t *testing.T, doc string) map[string]string {
	t.Helper()
	var m any
	if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
		t.Fatalf("flatten: %v", err)
	}
	out := map[string]string{}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, sub := range x {
				walk(join(prefix, k), sub)
			}
		case []any:
			for i, sub := range x {
				walk(fmt.Sprintf("%s[%d]", prefix, i), sub)
			}
		default:
			out[prefix] = fmt.Sprint(v)
		}
	}
	walk("", m)
	return out
}

func join(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// Switching a component off clears exactly the keys the form declares for it, and nothing
// else in the document moves. The old delete list was hand-maintained and named the whole
// `cold_cache` block, so disabling deleted min_idle_seconds and max_calls that the enable
// path had carefully preserved (R7). It is derived from the declarations now, so the two
// paths cannot disagree.
//
// There is no "undeclared key inside the block" case left to protect: per-component blocks
// decode strictly, so a key no descriptor names is a key the component rejects, and
// TestEveryComponentDeclaresExactlyItsConfigurableKeys is what keeps that true.
func TestSwitchingAComponentOffClearsItsDeclaredKeysAndNothingElse(t *testing.T) {
	doc := canonicalDoc(t)
	for name, decls := range components.AllFields() {
		if len(decls) == 0 {
			continue
		}
		t.Run(name, func(t *testing.T) {
			// Give the component a full block first, so switch-off has something to clear.
			on := mustParse(t, doc)
			on.Components[name] = map[string]any{}
			for _, fd := range decls {
				if fd.Secret {
					continue
				}
				vs := perturbations(fd, on.Components[name][fd.Key])
				if len(vs) == 0 {
					continue
				}
				on.Components[name][fd.Key] = vs[0]
			}
			configured, err := ApplyForm(doc, on)
			if err != nil {
				t.Fatalf("configuring every key at once failed: %v", err)
			}
			// Now switch it off: out of the pipeline, values still posted.
			off := mustParse(t, configured)
			off.Pipeline = remove(off.Pipeline, name)
			if name == "extract_llm" {
				// The one component with a coupling: it is switched off by its two
				// switches, and the pipeline follows from them (see
				// applyExtractLLMCoupling), not the other way round.
				off.Components[name]["per_output"] = false
				off.Components[name]["cold_cache.enabled"] = false
			}
			out, err := ApplyForm(configured, off)
			if err != nil {
				t.Fatalf("switch-off failed: %v", err)
			}
			if blk, ok := mustLoad(t, out).Components[name]; ok && !blk.IsZero() {
				var m map[string]any
				_ = blk.Decode(&m)
				t.Errorf("the block survived with %v, so a declared key was not cleared", m)
			}
			// Every OTHER component's block, and every top-level key, byte-identical.
			for path, v := range flatten(t, configured) {
				if strings.HasPrefix(path, "components."+name+".") || strings.HasPrefix(path, "pipeline[") {
					continue
				}
				if got := flatten(t, out)[path]; got != v {
					t.Errorf("switching %s off changed %s: %q -> %q", name, path, v, got)
				}
			}
			if contains(mustLoad(t, out).Pipeline, name) {
				t.Errorf("%s is still in the pipeline", name)
			}
		})
	}
}

// R8, the reason a descriptor's enum options are read from the same constant the engine
// parses. `deterministic` was missing from the form's copy of the strategy list, so a
// stored `strategy: deterministic` was not matched, the form fell back to "code", and the
// next save WROTE `strategy: code` over it — silently converting an LLM-free configuration
// into one that makes model calls.
func TestAStoredStrategyTheFormDoesNotOfferIsNeverRewritten(t *testing.T) {
	doc := "pipeline: [extract_llm]\nmode: sync\ncomponents:\n  extract_llm:\n" +
		"    per_output: true\n    strategy: deterministic\n"
	f := mustParse(t, doc)
	if got := f.Components["extract_llm"]["strategy"]; got != "deterministic" {
		t.Fatalf("the form read strategy %q, not the stored deterministic", got)
	}
	out, err := ApplyForm(doc, f)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "strategy: deterministic") {
		t.Errorf("the save rewrote a deterministic (LLM-free) strategy:\n%s", out)
	}
}

// R7's other half: the ENABLE path must not lose the cold-cache keys the form now owns,
// and switching off must not reach outside the component's own block.
func TestTheColdCacheKeysSurviveASaveThatDoesNotMentionThem(t *testing.T) {
	f := mustParse(t, osherDoc)
	out, err := ApplyForm(osherDoc, f)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"min_idle_seconds: 120", "max_calls: 6"} {
		if !strings.Contains(out, want) {
			t.Errorf("a plain re-save lost %q:\n%s", want, out)
		}
	}
	if paths := changedPaths(t, osherDoc, out); len(paths) != 0 {
		t.Errorf("re-saving an unchanged form moved %v", paths)
	}
}

// A credential is write-only. It is declared (the form has to be able to SET it) but never
// echoed back, and "absent from the form" therefore cannot mean "cleared" — otherwise every
// save would delete the stored key.
func TestAStoredCredentialIsNeverEchoedAndNeverLost(t *testing.T) {
	const secret = "sk-do-not-echo-me"
	doc := "pipeline: [extract_llm]\nmode: sync\ncomponents:\n  extract_llm:\n" +
		"    per_output: true\n    model:\n      model: claude-haiku-4-5\n      api_key: " + secret + "\n"
	f := mustParse(t, doc)
	if _, ok := f.Components["extract_llm"]["model.api_key"]; ok {
		t.Error("the parsed form carries the stored api_key")
	}
	blob, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), secret) {
		t.Errorf("the api_key reached the settings payload: %s", blob)
	}
	out, err := ApplyForm(doc, f)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, secret) {
		t.Errorf("saving the form deleted the stored credential:\n%s", out)
	}
	// An explicit empty string is how it gets cleared.
	f.Components["extract_llm"]["model.api_key"] = ""
	out, err = ApplyForm(doc, f)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, secret) {
		t.Errorf("an explicit clear left the credential in place:\n%s", out)
	}
}

// A component the form does not send is not touched at all. Without that, a round trip on a
// `preset: codesafe` document deleted the tuned block of every component the preset does not
// run — the form would be editing configuration nobody put on the page.
func TestAComponentTheFormDoesNotSendIsUntouched(t *testing.T) {
	doc := "preset: codesafe\nmode: sync\n"
	f := mustParse(t, doc)
	if f.Components["collapse"]["max_tokens"] != 3000 {
		t.Fatalf("the preset's collapse block is not on the form: %v", f.Components)
	}
	f.Components = map[string]map[string]any{"extract": {"min_tokens": 500}}
	out, err := ApplyForm(doc, f)
	if err != nil {
		t.Fatal(err)
	}
	if back := mustParse(t, out); back.Components["collapse"]["max_tokens"] != 3000 {
		t.Errorf("a component the form did not send lost its configuration (%v):\n%s",
			back.Components["collapse"], out)
	}
}
