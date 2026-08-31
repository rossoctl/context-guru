package tenant

import (
	"errors"
	"strings"
	"testing"
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

func TestMigrateDeprecatedExtractLLMConfigMovesBothKeys(t *testing.T) {
	out, changed, err := migrateDeprecatedExtractLLMConfig(legacyExtractLLMConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	if strings.Contains(out, "per_output") {
		t.Error("per_output still present")
	}
	if strings.Contains(out, "cold_cache") {
		t.Error("cold_cache still present")
	}
	if !strings.Contains(out, "extract_llm_sweep:\n    min_tokens: 1000\n") {
		t.Errorf("extract_llm_sweep block missing or wrong, got:\n%s", out)
	}
	if !strings.Contains(out, "extract_llm, extract_llm_sweep,") {
		t.Errorf("extract_llm_sweep not inserted into the pipeline list, got:\n%s", out)
	}
	// Everything else about the extract_llm block — the account's OWN tuning — must survive
	// untouched: this is a migration, not a reset to defaults.
	for _, want := range []string{"min_tokens: 500", "aggressiveness: medium", "min_request_tokens: 500"} {
		if !strings.Contains(out, want) {
			t.Errorf("lost %q across the migration", want)
		}
	}
}

func TestMigrateDeprecatedExtractLLMConfigHandlesBlockStylePipeline(t *testing.T) {
	blockStyle := `cache:
  head_ttl_1h: false
components:
  extract_llm:
    cold_cache:
      enabled: true
      min_tokens: 1000
    per_output: true
    trigger:
      min_request_tokens: 3000
mode: sync
pipeline:
  - format
  - extract_llm
  - extract
`
	out, changed, err := migrateDeprecatedExtractLLMConfig(blockStyle)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	if !strings.Contains(out, "  - extract_llm\n  - extract_llm_sweep\n") {
		t.Errorf("extract_llm_sweep not inserted into the block-style pipeline, got:\n%s", out)
	}
}

func TestMigrateDeprecatedExtractLLMConfigIsANoOpWithoutEitherKey(t *testing.T) {
	clean := `pipeline: [format, extract_llm, extract]
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
	// cold_cache present but not the enabled+min_tokens-only shape every real account had —
	// this must be refused, not guessed at.
	weird := `components:
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
