package offload

import (
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"

	"github.com/rossoctl/context-guru/components"
	"gopkg.in/yaml.v3"
)

// modelConfig is the shared `model:` block for the NeedsModel components (extract code/rlm,
// summarize). `source` picks the host-resolved client at run time (incoming vs config, via
// Ctx.Model.For), and `model` re-points whatever that resolves to at a different model id — same
// endpoint, same credential.
//
// It names a SOURCE, never an endpoint or a credential. A component must not hold either — those
// are the host's: the operator's configured cheap model (`source: config`, host env/config), or
// the caller's own (`source: incoming`, resolved by the host per request). A document that still
// carries `base_url`, `api_key`, `provider` or `auth` here has those keys dropped before decode
// (see decodeWithModelHint): they are never read, and the rest of the block applies.
type modelConfig struct {
	Source string `yaml:"source"` // incoming (default) | config
	Model  string `yaml:"model"`  // e.g. gpt-4o-mini; empty => no model-id override, `source` decides
}

// retiredModelKeys names the four model: keys this document used to accept (an endpoint and a
// credential). decodeWithModelHint drops them before the strict decode.
var retiredModelKeys = []string{"base_url", "api_key", "provider", "auth"}

// decodeWithModelHint is components.Decode, except that it first drops any of retiredModelKeys
// from the top-level `model:` block, and logs ONE WARN for the component naming each dropped key
// and where that setting lives now. Every component with a model: block (extract_llm, summarize,
// cache_aware_summarizer, agentdiet) calls this instead of components.Decode directly.
//
// Dropping, not refusing, because a removed setting must not stop the proxy: on main these keys
// were valid, so a document written before the removal has to keep loading. Every other decode
// error, including an unrelated unknown field under model:, still fails.
//
// The dropped values are never read. That keeps the point of the removal in hosted mode: a
// tenant's document still cannot carry an endpoint or a credential. The WARN names keys only,
// never a value, because one of them is a credential.
func decodeWithModelHint(component string, raw []byte, dst any) error {
	raw, dropped, err := dropRetiredModelKeys(raw)
	if err != nil {
		return err
	}
	if len(dropped) > 0 {
		slog.Warn("cg.config.retired_model_keys_ignored",
			"component", component,
			"keys", dropped,
			"replacement", "set the endpoint and credential on the host (CHEAP_MODEL / "+
				"CHEAP_MODEL_PROVIDER / CHEAP_MODEL_BASE / CHEAP_MODEL_KEY / CHEAP_MODEL_AUTH on the "+
				"standalone proxy) and use model.source: config; model.source: incoming needs neither")
	}
	return components.Decode(raw, dst)
}

// dropRetiredModelKeys removes retiredModelKeys from raw's top-level `model:` mapping and returns
// the document re-encoded, with the dotted names of the keys it removed (sorted). A document
// with none of them is returned unchanged, byte for byte, so the strict decode sees exactly
// what it saw before. A document that does not parse is also returned unchanged: the strict
// decode then reports the parse error itself.
func dropRetiredModelKeys(raw []byte) ([]byte, []string, error) {
	if len(raw) == 0 {
		return raw, nil, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil || len(doc.Content) == 0 {
		return raw, nil, nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return raw, nil, nil
	}
	var model *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "model" {
			model = root.Content[i+1]
		}
	}
	if model == nil || model.Kind != yaml.MappingNode {
		return raw, nil, nil
	}
	var dropped []string
	kept := model.Content[:0:0]
	for i := 0; i+1 < len(model.Content); i += 2 {
		if slices.Contains(retiredModelKeys, model.Content[i].Value) {
			dropped = append(dropped, "model."+model.Content[i].Value)
			continue
		}
		kept = append(kept, model.Content[i], model.Content[i+1])
	}
	if len(dropped) == 0 {
		return raw, nil, nil
	}
	model.Content = kept
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, nil, fmt.Errorf("model: re-encode after dropping %s: %w", strings.Join(dropped, ", "), err)
	}
	sort.Strings(dropped)
	return out, dropped, nil
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
