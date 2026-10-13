package offload

import (
	"testing"

	"github.com/rossoctl/context-guru/components"
)

// A component's `model:` block names a source and, optionally, a model id to compact WITH — never
// an endpoint or a credential. Those are host config now (CHEAP_MODEL_* / the operator's
// configured cheap model), because the core must not hold a credential. Decode is strict
// (components.Decode: yaml.KnownFields(true)), so a document that still carries one of the four
// retired keys is rejected rather than silently ignored.
func TestModelBlockRejectsCredentials(t *testing.T) {
	for _, key := range []string{"base_url", "api_key", "provider", "auth"} {
		t.Run(key, func(t *testing.T) {
			var cfg struct {
				Model modelConfig `yaml:"model"`
			}
			doc := []byte("model:\n  source: incoming\n  " + key + ": x\n")
			if err := components.Decode(doc, &cfg); err == nil {
				t.Fatalf("model.%s was accepted; a component config must not carry a credential", key)
			}
		})
	}
}

func TestModelBlockAcceptsSourceAndModel(t *testing.T) {
	var cfg struct {
		Model modelConfig `yaml:"model"`
	}
	doc := []byte("model:\n  source: config\n  model: claude-haiku-4-5\n")
	if err := components.Decode(doc, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Model.Source != "config" || cfg.Model.Model != "claude-haiku-4-5" {
		t.Fatalf("got %+v", cfg.Model)
	}
}
