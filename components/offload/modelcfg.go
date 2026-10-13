package offload

import (
	"fmt"
	"strings"

	"github.com/rossoctl/context-guru/components"
)

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

// retiredModelKeys names the four model: keys this document used to accept (an endpoint and a
// credential), each now rejected by components.Decode's strict YAML. decodeWithModelHint exists
// only to reword that rejection; the rejection itself is unconditional.
var retiredModelKeys = []string{"base_url", "api_key", "provider", "auth"}

// decodeWithModelHint is components.Decode, except that a decode error naming one of
// retiredModelKeys under model: is reworded to say where that setting lives now, instead of a
// bare "field not found" that gives no reader a next step. Every component with a model: block
// (extract_llm, summarize, cache_aware_summarizer, agentdiet) calls this instead of
// components.Decode directly, so a stored document from before this field removal — and the
// settings page, which validates through the same path — gets one consistent message. Any other
// decode error, including an unrelated unknown field, passes through unchanged.
func decodeWithModelHint(raw []byte, dst any) error {
	err := components.Decode(raw, dst)
	if err == nil {
		return nil
	}
	for _, key := range retiredModelKeys {
		if strings.Contains(err.Error(), "field "+key+" not found in type offload.modelConfig") {
			return fmt.Errorf("model.%s is no longer a component setting (%w): a component's "+
				"model: block names a source, never a credential or an endpoint. "+
				"model.source: incoming needs neither — it reuses the caller's own. "+
				"model.source: config uses the operator's configured model, set on the host "+
				"(CHEAP_MODEL_BASE / CHEAP_MODEL_KEY / CHEAP_MODEL_PROVIDER / CHEAP_MODEL_AUTH on "+
				"the standalone proxy)", key, err)
		}
	}
	return err
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
