package tenant

import (
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// legacyExtractLLMConfig is the exact shape every affected account shared: extract_llm with
// both deprecated keys, in a flow-style pipeline list.
const legacyExtractLLMConfig = `pipeline: [format, dedup, toon, cmdfilter, searchfold, textclean, extract_llm, extract, cachesplit, toolfilter]
components:
  extract:
    min_tokens: 400
  extract_llm:
    aggressiveness: medium
    cold_cache:
      enabled: true
      min_tokens: 1000
    context: recent
    context_messages: 2
    economic_gate: true
    fire_on: pressure
    llm_every_n_requests: 1
    llm_max_per_request: 8
    llm_max_per_session: 0
    min_tokens: 500
    model:
      model: claude-haiku-4-5
      source: incoming
    per_output: true
    strategy: code
    trigger:
      min_request_tokens: 500
mode: sync
`

// asDoc decodes a migrated document for assertions that are easier to state on structure than
// on text — the whole point of moving off a text-level rewrite (see this file's package
// comment) is that "is extract_llm_sweep in the pipeline" should be answered by looking at the
// pipeline, not by grepping for a substring next to a comma.
func asDoc(t *testing.T, cfg string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(cfg), &doc); err != nil {
		t.Fatalf("migrated document is not valid YAML: %v\n%s", err, cfg)
	}
	return doc
}

func pipelineOf(t *testing.T, doc map[string]any) []string {
	t.Helper()
	raw, _ := doc["pipeline"].([]any)
	out := make([]string, len(raw))
	for i, v := range raw {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("pipeline entry %d is not a string: %v", i, v)
		}
		out[i] = s
	}
	return out
}

func componentsOf(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	m, _ := doc["components"].(map[string]any)
	if m == nil {
		t.Fatal("components is missing or not a mapping")
	}
	return m
}

func TestMigrateDeprecatedExtractLLMConfigMovesBothKeys(t *testing.T) {
	out, changed, err := migrateDeprecatedExtractLLMConfig(legacyExtractLLMConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	doc := asDoc(t, out)
	comps := componentsOf(t, doc)
	extractLLM, _ := comps["extract_llm"].(map[string]any)
	if extractLLM == nil {
		t.Fatal("extract_llm missing after migration")
	}
	if _, present := extractLLM["per_output"]; present {
		t.Error("per_output still present")
	}
	if _, present := extractLLM["cold_cache"]; present {
		t.Error("cold_cache still present")
	}
	sweep, _ := comps["extract_llm_sweep"].(map[string]any)
	if sweep == nil {
		t.Fatal("extract_llm_sweep missing")
	}
	if mt, ok := sweep["min_tokens"].(int); !ok || mt != 1000 {
		t.Errorf("extract_llm_sweep.min_tokens = %v, want 1000", sweep["min_tokens"])
	}
	pipe := pipelineOf(t, doc)
	llmIdx, sweepIdx := indexOf(pipe, "extract_llm"), indexOf(pipe, "extract_llm_sweep")
	if llmIdx < 0 || sweepIdx != llmIdx+1 {
		t.Errorf("pipeline = %v; want extract_llm_sweep immediately after extract_llm", pipe)
	}
	// Everything else about the extract_llm block — the account's OWN tuning — must survive
	// untouched: this is a migration, not a reset to defaults.
	if mt, ok := extractLLM["min_tokens"].(int); !ok || mt != 500 {
		t.Errorf("extract_llm.min_tokens = %v, want 500 (the account's own tuning)", extractLLM["min_tokens"])
	}
	if extractLLM["aggressiveness"] != "medium" {
		t.Errorf("extract_llm.aggressiveness = %v, want medium", extractLLM["aggressiveness"])
	}
	trigger, _ := extractLLM["trigger"].(map[string]any)
	if trigger == nil || trigger["min_request_tokens"] != 500 {
		t.Errorf("extract_llm.trigger = %v, want {min_request_tokens: 500}", trigger)
	}
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

// Regression for the bug an earlier, regex-based draft of this file shipped with: a flow-style
// pipeline where extract_llm is the LAST element has no trailing comma after it, and a
// substring replace on "extract_llm," was a silent no-op there — config.Validate cannot catch
// this, because the resulting document is perfectly valid, it just never runs the sweep. A
// parse-based rewrite has no such case: the insertion point is found by comparing list
// elements, not by matching literal punctuation.
func TestMigrateDeprecatedExtractLLMConfigHandlesExtractLLMLastInPipeline(t *testing.T) {
	const cfg = `pipeline: [format, dedup, toon, cmdfilter, searchfold, textclean, extract_llm]
components:
  extract_llm:
    cold_cache:
      enabled: true
      min_tokens: 1000
    per_output: true
    trigger:
      min_request_tokens: 500
`
	out, changed, err := migrateDeprecatedExtractLLMConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	doc := asDoc(t, out)
	pipe := pipelineOf(t, doc)
	if got := pipe[len(pipe)-1]; got != "extract_llm_sweep" {
		t.Errorf("pipeline = %v; want extract_llm_sweep as the new last element", pipe)
	}
	comps := componentsOf(t, doc)
	if comps["extract_llm_sweep"] == nil {
		t.Error("extract_llm_sweep missing from components despite the pipeline claiming it's there")
	}
}

// Regression: components.Trigger is a field shared by extract, summarize and extract_llm alike
// (each embeds it separately as yaml:"trigger"). A migration that looks for "any trigger block
// in the document" rather than "extract_llm's own trigger field" would corrupt or double-insert
// against an unrelated component. This document has trigger blocks on BOTH extract and
// extract_llm; only extract_llm's own keys may be touched.
func TestMigrateDeprecatedExtractLLMConfigDoesNotTouchAnotherComponentsTrigger(t *testing.T) {
	const cfg = `pipeline: [extract_llm, extract]
components:
  extract:
    min_tokens: 400
    trigger:
      min_request_tokens: 700
  extract_llm:
    cold_cache:
      enabled: true
      min_tokens: 1000
    trigger:
      min_request_tokens: 500
`
	out, changed, err := migrateDeprecatedExtractLLMConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	doc := asDoc(t, out)
	comps := componentsOf(t, doc)
	extract, _ := comps["extract"].(map[string]any)
	if extract == nil {
		t.Fatal("extract missing after migration")
	}
	trigger, _ := extract["trigger"].(map[string]any)
	if trigger == nil || trigger["min_request_tokens"] != 700 {
		t.Errorf("extract's own trigger = %v, want untouched {min_request_tokens: 700}", trigger)
	}
	if comps["extract_llm_sweep"] == nil {
		t.Error("extract_llm_sweep missing")
	}
}

// Regression: extract_llm's trigger carrying more than one key (min_request_tokens is not
// necessarily first, or alone — yaml.Marshal output on this codebase's settings-form save path
// sorts keys alphabetically, and min_request_tokens sorts LAST among Trigger's fields) must not
// matter to a parse-based rewrite the way it mattered to a text anchor that only matched a
// single-key block.
func TestMigrateDeprecatedExtractLLMConfigHandlesMultiKeyTrigger(t *testing.T) {
	const cfg = `pipeline: [extract_llm]
components:
  extract_llm:
    cold_cache:
      enabled: true
      min_tokens: 1000
    trigger:
      min_messages: 2
      min_output_tokens: 200
      min_request_tokens: 500
`
	out, changed, err := migrateDeprecatedExtractLLMConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	doc := asDoc(t, out)
	comps := componentsOf(t, doc)
	extractLLM, _ := comps["extract_llm"].(map[string]any)
	trigger, _ := extractLLM["trigger"].(map[string]any)
	if trigger["min_messages"] != 2 || trigger["min_output_tokens"] != 200 || trigger["min_request_tokens"] != 500 {
		t.Errorf("extract_llm.trigger = %v, want all three keys preserved", trigger)
	}
}

// Regression: cold_cache.enabled: false means the sweep never ran for this account — "the whole
// mechanism was off" is a real, valid state, not an unrecognized shape. It should be dropped
// with nothing added, per extract_llm.go's own migration note ("cold_cache.enabled becomes the
// component's presence in the pipeline").
func TestMigrateDeprecatedExtractLLMConfigDropsADisabledColdCacheWithoutAddingASweep(t *testing.T) {
	const cfg = `pipeline: [extract_llm]
components:
  extract_llm:
    cold_cache:
      enabled: false
      min_tokens: 1000
    per_output: true
`
	out, changed, err := migrateDeprecatedExtractLLMConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	doc := asDoc(t, out)
	comps := componentsOf(t, doc)
	if comps["extract_llm_sweep"] != nil {
		t.Error("extract_llm_sweep added for a cold_cache that was disabled")
	}
	pipe := pipelineOf(t, doc)
	if indexOf(pipe, "extract_llm_sweep") >= 0 {
		t.Errorf("pipeline = %v; extract_llm_sweep should not be in it", pipe)
	}
	extractLLM, _ := comps["extract_llm"].(map[string]any)
	if _, present := extractLLM["cold_cache"]; present {
		t.Error("cold_cache still present")
	}
	if _, present := extractLLM["per_output"]; present {
		t.Error("per_output still present")
	}
}

// Regression for a real blocker an independent review found in an untyped read of `enabled`:
// YAML 1.1 accepts yes/Yes/YES/on/On/ON/y/Y as true, but reading that value out of a
// map[string]any with a bare `.(bool)` assertion silently gets false (the decoder resolved it
// to a plain string, not a bool, when the target type was `any`) — an account whose sweep was
// genuinely running pre-#118 would have had it dropped with nothing added, silently, the exact
// same class of loss as the flow-pipeline bug this file's package comment already describes.
// Decoding cold_cache through a typed struct (the same path config.LoadBytes itself uses)
// fixes this by construction; this pins that it stays fixed.
func TestMigrateDeprecatedExtractLLMConfigHandlesYAML11BoolWords(t *testing.T) {
	for _, word := range []string{"yes", "Yes", "YES", "on", "On", "ON", "y", "Y"} {
		t.Run(word, func(t *testing.T) {
			cfg := "pipeline: [extract_llm]\ncomponents:\n  extract_llm:\n    cold_cache:\n" +
				"      enabled: " + word + "\n      min_tokens: 1000\n"
			out, changed, err := migrateDeprecatedExtractLLMConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if !changed {
				t.Fatal("changed = false, want true")
			}
			doc := asDoc(t, out)
			pipe := pipelineOf(t, doc)
			if indexOf(pipe, "extract_llm_sweep") < 0 {
				t.Errorf("enabled: %s did not add extract_llm_sweep to the pipeline (got %v)", word, pipe)
			}
			comps := componentsOf(t, doc)
			sweep, _ := comps["extract_llm_sweep"].(map[string]any)
			if sweep == nil || sweep["min_tokens"] != 1000 {
				t.Errorf("enabled: %s: extract_llm_sweep = %v, want {min_tokens: 1000}", word, sweep)
			}
		})
	}
}

// Companion to the above: the falsy YAML 1.1 words must still drop cleanly, matching plain
// `false` — these coincidentally already worked under the untyped read, but the typed decode
// must not regress them.
func TestMigrateDeprecatedExtractLLMConfigHandlesYAML11FalseWords(t *testing.T) {
	for _, word := range []string{"no", "No", "NO", "off", "Off", "OFF", "n", "N"} {
		t.Run(word, func(t *testing.T) {
			cfg := "pipeline: [extract_llm]\ncomponents:\n  extract_llm:\n    cold_cache:\n" +
				"      enabled: " + word + "\n      min_tokens: 1000\n"
			out, changed, err := migrateDeprecatedExtractLLMConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if !changed {
				t.Fatal("changed = false, want true")
			}
			doc := asDoc(t, out)
			comps := componentsOf(t, doc)
			if comps["extract_llm_sweep"] != nil {
				t.Errorf("enabled: %s added extract_llm_sweep; a false word must drop cleanly", word)
			}
		})
	}
}

// Regression: cold_cache: (YAML null) and cold_cache: {} both mean the same thing a plain
// `enabled: false` does — the typed decode's zero value is Enabled=false — so both must drop
// cleanly rather than being refused as an unrecognized shape.
func TestMigrateDeprecatedExtractLLMConfigDropsNullOrEmptyColdCache(t *testing.T) {
	for name, cfg := range map[string]string{
		"null":  "pipeline: [extract_llm]\ncomponents:\n  extract_llm:\n    cold_cache:\n    per_output: true\n",
		"empty": "pipeline: [extract_llm]\ncomponents:\n  extract_llm:\n    cold_cache: {}\n    per_output: true\n",
	} {
		t.Run(name, func(t *testing.T) {
			out, changed, err := migrateDeprecatedExtractLLMConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if !changed {
				t.Fatal("changed = false, want true")
			}
			doc := asDoc(t, out)
			comps := componentsOf(t, doc)
			if comps["extract_llm_sweep"] != nil {
				t.Error("extract_llm_sweep added for a null/empty cold_cache")
			}
			extractLLM, _ := comps["extract_llm"].(map[string]any)
			if _, present := extractLLM["cold_cache"]; present {
				t.Error("cold_cache still present")
			}
		})
	}
}

// Regression: a non-bool `enabled` (e.g. hand-edited to 1 or "true") must be refused and
// logged, not silently coerced to false — such a config never built pre-#118 either (the typed
// loader rejected it the same way), so refusing here costs nothing and matches that behavior.
func TestMigrateDeprecatedExtractLLMConfigRefusesANonBoolEnabled(t *testing.T) {
	for name, cfg := range map[string]string{
		"integer": "pipeline: [extract_llm]\ncomponents:\n  extract_llm:\n    cold_cache:\n      enabled: 1\n      min_tokens: 1000\n",
		"string":  `pipeline: [extract_llm]` + "\ncomponents:\n  extract_llm:\n    cold_cache:\n      enabled: \"true\"\n      min_tokens: 1000\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, changed, err := migrateDeprecatedExtractLLMConfig(cfg)
			if err == nil {
				t.Fatal("expected an error for a non-bool enabled value, got nil")
			}
			if changed {
				t.Error("changed = true on a refused document")
			}
		})
	}
}

// Regression: a fourth cold_cache field (max_calls, min_idle_seconds — the two the old
// component had that the sweep has no equivalent for) must still be refused now that the extra-
// field check is KnownFields(true) on the typed decode rather than hand-rolled counting.
func TestMigrateDeprecatedExtractLLMConfigRefusesMaxCallsAndMinIdleSeconds(t *testing.T) {
	for name, cfg := range map[string]string{
		"max_calls":        "pipeline: [extract_llm]\ncomponents:\n  extract_llm:\n    cold_cache:\n      enabled: true\n      min_tokens: 1000\n      max_calls: 3\n",
		"min_idle_seconds": "pipeline: [extract_llm]\ncomponents:\n  extract_llm:\n    cold_cache:\n      enabled: true\n      min_tokens: 1000\n      min_idle_seconds: 600\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, changed, err := migrateDeprecatedExtractLLMConfig(cfg)
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if changed {
				t.Error("changed = true on a refused document")
			}
		})
	}
}

// Regression: a "cold_cache:" substring that is not actually the key — inside a YAML comment,
// for instance — must not be mistaken for the real thing. A parse-based rewrite only ever sees
// what the document actually decodes to, so this is really a test that the migration looks at
// extractLLM["cold_cache"] and nothing textual.
func TestMigrateDeprecatedExtractLLMConfigIgnoresColdCacheMentionedInAComment(t *testing.T) {
	const cfg = `# note: cold_cache: was removed upstream, revisit
pipeline: [format, extract_llm, extract]
components:
  extract_llm:
    min_tokens: 500
    per_output: true
    trigger:
      min_request_tokens: 500
`
	out, changed, err := migrateDeprecatedExtractLLMConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("changed = false, want true — per_output alone is still a real thing to fix")
	}
	doc := asDoc(t, out)
	comps := componentsOf(t, doc)
	extractLLM, _ := comps["extract_llm"].(map[string]any)
	if _, present := extractLLM["per_output"]; present {
		t.Error("per_output still present")
	}
	if comps["extract_llm_sweep"] != nil {
		t.Error("extract_llm_sweep added despite no real cold_cache key ever being present")
	}
}

func TestMigrateDeprecatedExtractLLMConfigHandlesBlockStylePipeline(t *testing.T) {
	const cfg = `cache:
  head_ttl_1h: false
components:
  extract_llm:
    cold_cache:
      enabled: true
      min_tokens: 1000
    per_output: true
mode: sync
pipeline:
  - format
  - extract_llm
  - extract
`
	out, changed, err := migrateDeprecatedExtractLLMConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	doc := asDoc(t, out)
	pipe := pipelineOf(t, doc)
	llmIdx, sweepIdx := indexOf(pipe, "extract_llm"), indexOf(pipe, "extract_llm_sweep")
	if llmIdx < 0 || sweepIdx != llmIdx+1 {
		t.Errorf("pipeline = %v; want extract_llm_sweep immediately after extract_llm", pipe)
	}
	// The unrelated cache: block, written before components: in the source, must survive.
	cache, _ := doc["cache"].(map[string]any)
	if cache == nil || cache["head_ttl_1h"] != false {
		t.Errorf("cache block = %v, want untouched {head_ttl_1h: false}", cache)
	}
}

func TestMigrateDeprecatedExtractLLMConfigIsANoOpWithoutEitherKey(t *testing.T) {
	const clean = `pipeline: [format, extract_llm, extract]
components:
  extract_llm:
    min_tokens: 500
    trigger:
      min_request_tokens: 500
mode: sync
`
	out, changed, err := migrateDeprecatedExtractLLMConfig(clean)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("changed = true on a config with neither deprecated key")
	}
	if out != clean {
		t.Error("output differs from input on a no-op call")
	}
}

func TestMigrateDeprecatedExtractLLMConfigRefusesAnUnrecognizedColdCacheShape(t *testing.T) {
	// cold_cache present and enabled, but with a THIRD field (max_calls) beyond
	// enabled/min_tokens — this migration only knows the shape every real account shared.
	const weird = `components:
  extract_llm:
    cold_cache:
      enabled: true
      min_tokens: 1000
      max_calls: 3
    trigger:
      min_request_tokens: 500
pipeline: [extract_llm]
`
	_, changed, err := migrateDeprecatedExtractLLMConfig(weird)
	if err == nil {
		t.Fatal("expected an error for a cold_cache shape this migration does not recognize, got nil")
	}
	if changed {
		t.Error("changed = true on a refused document")
	}
}

func TestMigrateDeprecatedExtractLLMConfigRefusesWhenExtractLLMSweepAlreadyExists(t *testing.T) {
	// Should not happen in practice (extract_llm_sweep didn't exist when cold_cache did), but a
	// hand-edited or otherwise unusual document must not silently clobber an existing entry.
	const cfg = `pipeline: [extract_llm, extract_llm_sweep]
components:
  extract_llm:
    cold_cache:
      enabled: true
      min_tokens: 1000
  extract_llm_sweep:
    min_tokens: 2000
`
	_, changed, err := migrateDeprecatedExtractLLMConfig(cfg)
	if err == nil {
		t.Fatal("expected an error when extract_llm_sweep already exists, got nil")
	}
	if changed {
		t.Error("changed = true on a refused document")
	}
}

// fixDeprecatedExtractLLMConfigs (the DB-integration half) below.

func seedLegacyTenant(t *testing.T, r *Registry, id, cfg string) {
	t.Helper()
	if _, err := r.db.Exec(
		`INSERT INTO tenants(id, label, email, config_yaml, created_at) VALUES (?,?,?,?,0)`,
		id, id, id+"@example.test", cfg); err != nil {
		t.Fatal(err)
	}
}

func TestFixDeprecatedExtractLLMConfigsRewritesAndValidates(t *testing.T) {
	r, err := Open("", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	seedLegacyTenant(t, r, "t1", legacyExtractLLMConfig)

	var validated string
	always := func(b []byte) error { validated = string(b); return nil }
	if err := fixDeprecatedExtractLLMConfigs(r.db, always); err != nil {
		t.Fatal(err)
	}
	if validated == "" {
		t.Fatal("validate was never called")
	}
	if strings.Contains(validated, "per_output") || strings.Contains(validated, "cold_cache") {
		t.Errorf("validate was called with an unmigrated document:\n%s", validated)
	}

	var stored string
	if err := r.db.QueryRow(`SELECT config_yaml FROM tenants WHERE id = 't1'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != validated {
		t.Errorf("stored config differs from the one that passed validation:\nstored: %s\nvalidated: %s", stored, validated)
	}
}

func TestFixDeprecatedExtractLLMConfigsSkipsEntirelyWithoutAValidator(t *testing.T) {
	r, err := Open("", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	seedLegacyTenant(t, r, "t1", legacyExtractLLMConfig)

	if err := fixDeprecatedExtractLLMConfigs(r.db, nil); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := r.db.QueryRow(`SELECT config_yaml FROM tenants WHERE id = 't1'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != legacyExtractLLMConfig {
		t.Error("config was rewritten despite no validator being supplied")
	}
}

func TestFixDeprecatedExtractLLMConfigsNeverWritesAFailedValidation(t *testing.T) {
	r, err := Open("", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	seedLegacyTenant(t, r, "t1", legacyExtractLLMConfig)

	alwaysFails := func([]byte) error { return errors.New("simulated: this deployment's real validator rejected it") }
	if err := fixDeprecatedExtractLLMConfigs(r.db, alwaysFails); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := r.db.QueryRow(`SELECT config_yaml FROM tenants WHERE id = 't1'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != legacyExtractLLMConfig {
		t.Error("config was overwritten even though validation failed")
	}
}

func TestFixDeprecatedExtractLLMConfigsLeavesCleanTenantsAlone(t *testing.T) {
	r, err := Open("", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	const clean = `pipeline: [extract_llm]
components:
  extract_llm:
    min_tokens: 500
`
	seedLegacyTenant(t, r, "t1", clean)

	called := false
	track := func(b []byte) error { called = true; return nil }
	if err := fixDeprecatedExtractLLMConfigs(r.db, track); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("validate was called for a tenant with neither deprecated key")
	}
}

// Regression for the isolation property between candidates: one tenant's config being
// unmigratable (a shape this doesn't recognize) or unvalidatable (the real config.Validate
// rejects the result) must not stop the OTHER candidates in the same batch from getting fixed.
func TestFixDeprecatedExtractLLMConfigsOneFailureDoesNotStarveTheOthers(t *testing.T) {
	r, err := Open("", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	const unrecognizedShape = `components:
  extract_llm:
    cold_cache:
      enabled: true
      min_tokens: 1000
      max_calls: 3
pipeline: [extract_llm]
`
	seedLegacyTenant(t, r, "t_shape", unrecognizedShape)
	// t_good and t_validate are identical on purpose: which one the validator happens to see
	// first is not something this test can (or needs to) pin down, since SELECT ... WHERE with
	// no ORDER BY makes no ordering promise. Rejecting exactly the SECOND call forces exactly
	// one of these two to fail regardless of which is seen first, which is all the isolation
	// property being tested needs.
	seedLegacyTenant(t, r, "t_good", legacyExtractLLMConfig)
	seedLegacyTenant(t, r, "t_validate", legacyExtractLLMConfig)

	calls := 0
	validate := func([]byte) error {
		calls++
		if calls == 2 {
			return errors.New("simulated rejection of the second candidate seen")
		}
		return nil
	}
	if err := fixDeprecatedExtractLLMConfigs(r.db, validate); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("validate called %d times, want 2 (t_shape is refused before validate ever runs; only t_good and t_validate reach it)", calls)
	}

	var shapeCfg, goodCfg, validateCfg string
	r.db.QueryRow(`SELECT config_yaml FROM tenants WHERE id = 't_shape'`).Scan(&shapeCfg)
	r.db.QueryRow(`SELECT config_yaml FROM tenants WHERE id = 't_good'`).Scan(&goodCfg)
	r.db.QueryRow(`SELECT config_yaml FROM tenants WHERE id = 't_validate'`).Scan(&validateCfg)

	if strings.Contains(shapeCfg, "extract_llm_sweep") {
		t.Error("t_shape (unrecognized shape) was migrated; it should have been refused")
	}
	migrated := !strings.Contains(goodCfg, "per_output") || !strings.Contains(validateCfg, "per_output")
	if !migrated {
		t.Error("neither t_good nor t_validate was migrated; the second candidate's failure starved the third")
	}
	bothMigrated := !strings.Contains(goodCfg, "per_output") && !strings.Contains(validateCfg, "per_output")
	if bothMigrated {
		t.Fatal("test setup did not actually force one candidate to fail validation — fix the fixture")
	}
}
