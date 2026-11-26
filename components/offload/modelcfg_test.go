package offload

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/components"
)

// A component's `model:` block names a source and, optionally, a model id to compact WITH — never
// an endpoint or a credential. Those are host config now (CHEAP_MODEL_* / the operator's
// configured cheap model), because the core must not hold a credential. components.Decode itself
// stays strict (yaml.KnownFields(true)): only decodeWithModelHint drops the four retired keys.
func TestModelBlockRejectsCredentialsInStrictDecode(t *testing.T) {
	for _, key := range retiredModelKeys {
		t.Run(key, func(t *testing.T) {
			var cfg struct {
				Model modelConfig `yaml:"model"`
			}
			doc := []byte("model:\n  source: incoming\n  " + key + ": x\n")
			if err := components.Decode(doc, &cfg); err == nil {
				t.Fatalf("components.Decode accepted model.%s; modelConfig must not grow a credential field", key)
			}
		})
	}
}

// captureWarns routes slog to a buffer for the test and returns it.
func captureWarns(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// A removed setting must not stop the proxy. These four keys were valid before the removal, so a
// document written then still loads: the key is dropped, the rest of the block applies, and ONE
// WARN names the key and where the setting lives now. The WARN never carries the value, because
// one of the four is a credential.
func TestDecodeWithModelHintDropsARetiredKeyAndWarns(t *testing.T) {
	const value = "sk-retired-value-never-logged"
	for _, key := range retiredModelKeys {
		t.Run(key, func(t *testing.T) {
			logs := captureWarns(t)
			var cfg struct {
				Model modelConfig `yaml:"model"`
				Other int         `yaml:"other"`
			}
			doc := []byte("other: 7\nmodel:\n  source: config\n  " + key + ": " + value +
				"\n  model: claude-haiku-4-5\n")
			if err := decodeWithModelHint("summarize", doc, &cfg); err != nil {
				t.Fatalf("model.%s stopped the decode: %v", key, err)
			}
			if cfg.Model.Source != "config" || cfg.Model.Model != "claude-haiku-4-5" || cfg.Other != 7 {
				t.Errorf("the rest of the document did not apply: %+v", cfg)
			}
			out := logs.String()
			if n := strings.Count(out, "cg.config.retired_model_keys_ignored"); n != 1 {
				t.Errorf("want exactly one WARN, got %d:\n%s", n, out)
			}
			for _, want := range []string{"component=summarize", "model." + key, "CHEAP_MODEL", "source: config"} {
				if !strings.Contains(out, want) {
					t.Errorf("WARN does not mention %q:\n%s", want, out)
				}
			}
			if strings.Contains(out, value) {
				t.Errorf("WARN carries the dropped value:\n%s", out)
			}
		})
	}
}

// All four keys in one block still give ONE WARN for the component, naming each of them.
func TestDecodeWithModelHintWarnsOncePerComponent(t *testing.T) {
	logs := captureWarns(t)
	var cfg struct {
		Model modelConfig `yaml:"model"`
	}
	doc := []byte("model:\n  source: config\n  provider: anthropic\n  base_url: https://example.invalid\n" +
		"  api_key: k\n  auth: bearer\n")
	if err := decodeWithModelHint("extract_llm", doc, &cfg); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	if n := strings.Count(out, "cg.config.retired_model_keys_ignored"); n != 1 {
		t.Errorf("want one WARN for the component, got %d:\n%s", n, out)
	}
	for _, key := range retiredModelKeys {
		if !strings.Contains(out, "model."+key) {
			t.Errorf("WARN does not name model.%s:\n%s", key, out)
		}
	}
}

// Only the model: block is touched. A key of the same name elsewhere is not a retired model key,
// and with no retired key present the document decodes exactly as components.Decode would, with
// no WARN.
func TestDecodeWithModelHintTouchesOnlyTheModelBlock(t *testing.T) {
	logs := captureWarns(t)
	var cfg struct {
		Model modelConfig `yaml:"model"`
	}
	if err := decodeWithModelHint("summarize", []byte("api_key: x\nmodel:\n  source: config\n"), &cfg); err == nil {
		t.Error("a top-level api_key (not under model:) was accepted")
	}
	if err := decodeWithModelHint("summarize", []byte("model:\n  source: config\n"), &cfg); err != nil {
		t.Errorf("a clean document failed: %v", err)
	}
	if logs.Len() != 0 {
		t.Errorf("a document with no retired key logged:\n%s", logs)
	}
}

// An unrelated decode error (a field this test does not name) still fails, and is not given the
// model-key hint.
func TestDecodeWithModelHintPassesThroughOtherErrors(t *testing.T) {
	var cfg struct {
		Model modelConfig `yaml:"model"`
	}
	if err := decodeWithModelHint("summarize", []byte("model:\n  nonsense_key: x\n"), &cfg); err == nil {
		t.Fatal("an unrelated unknown field was accepted")
	} else if strings.Contains(err.Error(), "CHEAP_MODEL") {
		t.Errorf("an unrelated error was given the model-key hint: %v", err)
	}
}

// Each of the four model: constructors routes through decodeWithModelHint, not
// components.Decode directly. Pinned through every one of them, so the wiring itself, not just
// the helper in isolation, is covered: a constructor that bypassed it would fail on the key again.
func TestEveryModelComponentLoadsAConfigWithARetiredKey(t *testing.T) {
	ctors := map[string]func([]byte) (components.Component, error){
		"extract_llm":            newExtractLLM,
		"summarize":              newSummarize,
		"cache_aware_summarizer": newCacheAwareSummarizer,
		"agentdiet":              newAgentDiet,
	}
	for name, ctor := range ctors {
		t.Run(name, func(t *testing.T) {
			logs := captureWarns(t)
			if _, err := ctor([]byte("model:\n  source: config\n  base_url: https://example.invalid\n")); err != nil {
				t.Fatalf("%s refused a config with model.base_url: %v", name, err)
			}
			if !strings.Contains(logs.String(), "component="+name) {
				t.Errorf("%s did not WARN under its own name:\n%s", name, logs)
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
