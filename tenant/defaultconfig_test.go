package tenant_test

// This test lives in an external test package so the dependency direction is
// obvious: `tenant` itself does not import `config` or the component registry, and
// must not — it is the control plane, not the pipeline.

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	_ "github.com/rossoctl/context-guru/components/all" // register every component
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/tenant"
)

// The configuration every new tenant is handed must actually build. A default that
// fails validation would be invisible: the resolver falls open to pass-through, so
// every user would silently get no compaction at all and the service would look
// like it simply does not work.
func TestDefaultConfigBuilds(t *testing.T) {
	if err := config.Validate([]byte(tenant.DefaultConfigYAML)); err != nil {
		t.Fatalf("the default tenant config does not build: %v", err)
	}
}

// It must also be the deterministic one: no cheap-model component in the default,
// because on a shared box those calls bill to the operator's credential, contend
// with every other tenant's agent, and add latency to someone else's hot path.
func TestDefaultConfigIsDeterministic(t *testing.T) {
	cfg, err := config.LoadBytes([]byte(tenant.DefaultConfigYAML))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range cfg.Pipeline {
		if name == "extract_llm" || name == "summarize" || name == "smartcrush" {
			t.Errorf("the default pipeline contains %q, which calls an LLM on the "+
				"request path; a shared-service default must be deterministic", name)
		}
	}
	if cfg.Mode != "" && cfg.Mode != "sync" {
		t.Errorf("default mode = %q, want sync", cfg.Mode)
	}
}

// The default document and the preset it is a copy of must stay the same configuration.
// DefaultConfigYAML is a literal because supersededDefaults compares stored configs
// against it byte for byte and because `tenant` may not import `config` — and the price of
// a literal is that it can drift from the definition it copies. This is the check that
// makes the copy safe: pipeline order and every component block, resolved through the same
// loader the proxy uses.
func TestDefaultConfigMatchesTheHousePreset(t *testing.T) {
	def, err := config.LoadBytes([]byte(tenant.DefaultConfigYAML))
	if err != nil {
		t.Fatal(err)
	}
	pre, err := config.LoadBytes([]byte("preset: house\n"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(def.Pipeline, ",") != strings.Join(pre.Pipeline, ",") {
		t.Errorf("the tenant default runs %v; preset house runs %v", def.Pipeline, pre.Pipeline)
	}
	// Everything OUTSIDE the pipeline and the component blocks, which this test used to
	// ignore entirely — so `mode`, `cache`, `store` and `observe` could drift silently
	// between the literal and the preset it copies. `cache` is the one with money attached:
	// it carries the keep-alive, which spends the caller's own credential per ping. Compared
	// as whole structs so a field added later is covered without editing this test.
	//
	// These catch drift introduced on the LITERAL side, which is the direction that ships:
	// DefaultConfigYAML is what a tracking account actually runs. They cannot catch it on
	// the PRESET side, and it is worth knowing why rather than assuming they do:
	// config.applyPreset copies only Pipeline and Components out of a rich preset and
	// silently discards everything else it decoded, so `pre.Cache` and friends are always
	// the zero value no matter what the preset document says. That hole is closed at its
	// root by TestNoRichPresetDeclaresAFieldApplyPresetDiscards in the config package —
	// forbidding the footgun rather than asserting against a value that can never arrive.
	// Empty mode and "sync" are documented as the same thing (config.Config.Mode: "Empty =
	// sync, which is byte-identical to the behavior before modes existed"), and the literal
	// states it while the preset leaves it out. Normalised rather than papered over: a
	// default of "observe" against a preset of "" still fails here, which is the difference
	// that would matter.
	syncOrEmpty := func(m string) string {
		if m == "" {
			return "sync"
		}
		return m
	}
	if syncOrEmpty(def.Mode) != syncOrEmpty(pre.Mode) {
		t.Errorf("mode differs: default %q, house %q", def.Mode, pre.Mode)
	}
	if def.Cache != pre.Cache {
		t.Errorf("the `cache:` block differs, and it is the one that spends money:\n"+
			"default %+v\nhouse   %+v", def.Cache, pre.Cache)
	}
	if !reflect.DeepEqual(def.Store, pre.Store) {
		t.Errorf("the `store:` block differs:\ndefault %+v\nhouse   %+v", def.Store, pre.Store)
	}
	if def.Observe != pre.Observe {
		t.Errorf("the `observe:` block differs:\ndefault %+v\nhouse   %+v", def.Observe, pre.Observe)
	}
	if len(def.Components) != len(pre.Components) {
		t.Fatalf("the tenant default states %d component blocks, preset house %d",
			len(def.Components), len(pre.Components))
	}
	for name, node := range def.Components {
		other, ok := pre.Components[name]
		if !ok {
			t.Errorf("the tenant default configures %q and preset house does not", name)
			continue
		}
		a, err := yaml.Marshal(&node)
		if err != nil {
			t.Fatal(err)
		}
		b, err := yaml.Marshal(&other)
		if err != nil {
			t.Fatal(err)
		}
		if string(a) != string(b) {
			t.Errorf("component %q differs:\ndefault: %s\nhouse:   %s", name, a, b)
		}
	}
}
