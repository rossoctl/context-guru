package tenant_test

// This test lives in an external test package so the dependency direction is
// obvious: `tenant` itself does not import `config` or the component registry, and
// must not — it is the control plane, not the pipeline.

import (
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
