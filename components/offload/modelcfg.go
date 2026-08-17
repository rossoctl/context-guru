package offload

import (
	"os"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/cheapmodel"
)

// modelConfig is the shared `model:` block for the NeedsModel components
// (extract code/rlm, summarize). `source` picks the host-resolved client at run
// time (incoming vs config, via Ctx.Model.For). Setting base_url/model/api_key
// instead pins a dedicated endpoint+credentials right in the config: Client()
// then returns that client and the component uses it directly, no CHEAP_MODEL_*
// env required.
type modelConfig struct {
	Source   string `yaml:"source"`   // incoming (default) | config
	Provider string `yaml:"provider"` // anthropic (default) | openai
	BaseURL  string `yaml:"base_url"` // e.g. http://llm-d-gateway:8000 (default: provider public API)
	APIKey   string `yaml:"api_key"`  // empty => provider env key (see Client)
	Model    string `yaml:"model"`    // e.g. gpt-4o-mini; empty => not a config-pinned client
	Auth     string `yaml:"auth"`     // anthropic only: "" | x-api-key (default) | bearer (LiteLLM/gateway)
}

// AllowEnvModelKey permits an api_key-less `model:` block to fall back to this
// process's provider credential (OPENAI_API_KEY / ANTHROPIC_API_KEY /
// ANTHROPIC_AUTH_TOKEN). True for single-tenant use, where the config and the
// environment belong to the same person.
//
// A hosted host sets it FALSE: there, the config document is written by a tenant and
// the environment holds the operator's credential, so the fallback would let any
// tenant bill their compaction to the operator — the one thing per-caller credentials
// exist to prevent. A block with no key then builds no client, and the component
// degrades (fail open).
var AllowEnvModelKey = true

// Client builds the LLM client this block pins, or nil when no model is named
// (the component then falls back to the host-resolved Ctx.Model.For(source)).
// An empty api_key falls back to the provider's env key when AllowEnvModelKey — so
// secrets can stay in the environment, out of the config file.
func (m modelConfig) Client() components.Model {
	if m.Model == "" {
		return nil
	}
	key := m.APIKey
	if key == "" && !AllowEnvModelKey {
		return nil
	}
	if m.Provider == "openai" {
		if key == "" {
			key = os.Getenv("OPENAI_API_KEY")
		}
		return cheapmodel.OpenAI{BaseURL: m.BaseURL, Model: m.Model, APIKey: key}
	}
	if key == "" {
		if key = os.Getenv("ANTHROPIC_API_KEY"); key == "" {
			key = os.Getenv("ANTHROPIC_AUTH_TOKEN")
		}
	}
	return cheapmodel.Anthropic{BaseURL: m.BaseURL, Model: m.Model, APIKey: key, AuthScheme: m.Auth}
}
