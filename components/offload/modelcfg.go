package offload

import (
	"os"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/cheapmodel"
)

// modelConfig is the shared `model:` block for the NeedsModel components
// (extract code/rlm, summarize). `source` picks the host-resolved client at run
// time (incoming vs config, via Ctx.Model.For), and `model` re-points whatever that
// resolves to at a different model id — same endpoint, same credential.
//
// Setting base_url and/or api_key pins a DEDICATED endpoint+credentials right in the
// config instead: Client() then returns that client and the component uses it directly,
// no CHEAP_MODEL_* env required. `model` on its own does NOT pin — see Client.
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
	// `model:` ALONE is a model-ID override, not a pinned endpoint. Pinning needs an endpoint
	// or a credential to pin TO; without one this built a client at the provider's PUBLIC API
	// and sent whatever credential the environment happened to hold as x-api-key.
	//
	// Measured: a config of `model: {source: incoming, model: claude-haiku-4-5}` — the shape
	// that names the cheap model to compact WITH — silently pinned api.anthropic.com and
	// presented an IBM gateway token there, so every call came back
	// `authentication_error: API key is invalid.` and `source: incoming` was never consulted.
	// The hosted service escaped it only because AllowEnvModelKey is false there, which made
	// the same block return nil and fall through to the right client by accident.
	//
	// So: no endpoint and no key means "let `source` resolve the client, then re-point it at
	// this model id" — which is exactly what ModelSpec.ForModel does, and what the `model:`
	// key was added to express.
	if m.BaseURL == "" && m.APIKey == "" {
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

// modelFields declares the shared `model:` block for the form, under prefix (normally
// "model"). Every key here was undocumented before the form grew to cover it, api_key
// included — a credential the settings page has to be able to set without ever echoing it
// back, which is what Secret means.
func modelFields(prefix string) []components.Field {
	p := prefix + "."
	return []components.Field{
		{Key: p + "source", Type: components.FieldEnum, Default: "incoming", Options: []string{"incoming", "config"},
			Hint: "Which host-resolved client does the work: incoming = the caller's own endpoint and credential; config = the operator's configured compaction model. `config` resolves to NOTHING on a deployment that has none, and the component then never makes a call."},
		{Key: p + "provider", Type: components.FieldEnum, Default: "anthropic", Options: []string{"anthropic", "openai"},
			Hint: "Wire dialect for a config-pinned endpoint below."},
		{Key: p + "base_url", Type: components.FieldString,
			Hint: "Pin a dedicated endpoint as a full URL (an internal gateway, say). Empty = the provider's public API."},
		{Key: p + "api_key", Type: components.FieldString, Secret: true,
			Hint: "Credential for the pinned endpoint. Empty falls back to this process's provider env key, and on a hosted deployment that fallback is refused rather than billed to the operator."},
		{Key: p + "model", Type: components.FieldString,
			Hint: "The model to compact WITH, e.g. claude-haiku-4-5. Empty = not a config-pinned client, so `source` decides."},
		{Key: p + "auth", Type: components.FieldEnum, Default: "x-api-key", Options: []string{"x-api-key", "bearer"},
			Hint: "Anthropic only: how the key is sent. bearer is what a LiteLLM/gateway front end expects."},
	}
}
