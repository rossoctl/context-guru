package offload

import "github.com/rossoctl/context-guru/components"

// modelConfig is the shared `model:` block for the NeedsModel components (extract code/rlm,
// summarize). `source` picks the host-resolved client at run time (incoming vs config, via
// Ctx.Model.For), and `model` re-points whatever that resolves to at a different model id — same
// endpoint, same credential.
//
// It names a SOURCE, never an endpoint or a credential. A component must not hold either — those
// are the host's: the operator's configured cheap model (`source: config`, host env/config), or
// the caller's own (`source: incoming`, resolved by the host per request). A document that still
// carries `base_url`, `api_key`, `provider` or `auth` here is rejected at decode time
// (components.Decode is strict), rather than silently accepted and ignored.
type modelConfig struct {
	Source string `yaml:"source"` // incoming (default) | config
	Model  string `yaml:"model"`  // e.g. gpt-4o-mini; empty => no model-id override, `source` decides
}

// modelFields declares the shared `model:` block for the form, under prefix (normally "model").
func modelFields(prefix string) []components.Field {
	p := prefix + "."
	return []components.Field{
		{Key: p + "source", Type: components.FieldEnum, Default: "incoming", Options: []string{"incoming", "config"},
			Hint: "Which host-resolved client does the work: incoming = the caller's own endpoint and credential; config = the operator's configured compaction model (CHEAP_MODEL_*). `config` resolves to NOTHING on a deployment that has none, and the component then never makes a call."},
		{Key: p + "model", Type: components.FieldString,
			Hint: "The model to compact WITH, e.g. claude-haiku-4-5. Empty = no override; `source`'s own model is used."},
	}
}
