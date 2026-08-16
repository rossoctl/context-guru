package tenant_test

// This test lives in an external test package so the dependency direction is
// obvious: `tenant` itself does not import `config` or the component registry, and
// must not — it is the control plane, not the pipeline.

import (
	"testing"

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
