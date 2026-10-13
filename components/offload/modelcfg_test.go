package offload

import (
	"strings"
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

// A tenant whose stored config predates this change gets a decode error naming a YAML field
// that no longer exists — correct, but useless to someone who does not already know this
// history. decodeWithModelHint rewords exactly these four fields' rejection to say where the
// setting lives now, so an operator reading a log line (or the settings page, which routes
// through the same path) is not sent hunting for a "field not found" error in a changelog.
func TestDecodeWithModelHintNamesTheReplacementSetting(t *testing.T) {
	for _, key := range []string{"base_url", "api_key", "provider", "auth"} {
		t.Run(key, func(t *testing.T) {
			var cfg struct {
				Model modelConfig `yaml:"model"`
			}
			doc := []byte("model:\n  source: incoming\n  " + key + ": x\n")
			err := decodeWithModelHint(doc, &cfg)
			if err == nil {
				t.Fatalf("model.%s was accepted; want it rejected", key)
			}
			if !strings.Contains(err.Error(), "model."+key) {
				t.Errorf("error %q does not name model.%s", err, key)
			}
			if !strings.Contains(err.Error(), "CHEAP_MODEL") && !strings.Contains(err.Error(), "source") {
				t.Errorf("error %q does not say where the setting lives now", err)
			}
		})
	}
}

// A document with no retired key still decodes normally, and an unrelated decode error (a
// field this test does not name) passes through unchanged rather than being swallowed by the
// hint logic.
func TestDecodeWithModelHintPassesThroughOtherErrors(t *testing.T) {
	var cfg struct {
		Model modelConfig `yaml:"model"`
	}
	if err := decodeWithModelHint([]byte("model:\n  nonsense_key: x\n"), &cfg); err == nil {
		t.Fatal("an unrelated unknown field was accepted")
	} else if strings.Contains(err.Error(), "CHEAP_MODEL") {
		t.Errorf("an unrelated error was given the model-key hint: %v", err)
	}
}

// Each of the four model: constructors routes through decodeWithModelHint, not
// components.Decode directly — pinned here through one of them (extract_llm) so the wiring
// itself, not just the helper in isolation, is covered.
func TestExtractLLMConfigNamesTheReplacementSettingOnARetiredKey(t *testing.T) {
	_, err := newExtractLLM([]byte("model:\n  source: incoming\n  api_key: x\n"))
	if err == nil {
		t.Fatal("extract_llm accepted model.api_key")
	}
	if !strings.Contains(err.Error(), "model.api_key") || !strings.Contains(err.Error(), "CHEAP_MODEL") {
		t.Errorf("error %q does not carry the model-key hint", err)
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
