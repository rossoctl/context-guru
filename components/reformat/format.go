package reformat

import (
	"encoding/json"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
	"gopkg.in/yaml.v3"
)

func init() { components.Register("format", newFormat) }

// Format re-encodes JSON tool outputs denser without losing data (a Reformat):
// pretty-printed JSON is re-marshaled compact. It's strictly lossless — same
// value, fewer whitespace tokens — so no stash is needed. (For a denser tabular
// re-encoding of uniform object arrays, see the `toon` component.)
type Format struct{ minTokens int }

type formatConfig struct {
	MinTokens int `yaml:"min_tokens"`
}

func newFormat(raw []byte) (components.Component, error) {
	cfg := formatConfig{MinTokens: 50}
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
	}
	return &Format{minTokens: cfg.MinTokens}, nil
}

func (Format) Name() string                 { return "format" }
func (Format) Enabled(*components.Ctx) bool { return true }

func (f *Format) Reformat(req *schemas.BifrostChatRequest, rep *components.Report, _ *components.Ctx) error {
	acted := false
	for i := range req.Input {
		m := &req.Input[i]
		if m.Role != schemas.ChatMessageRoleTool {
			continue
		}
		if !schema.Rewritable(*m) {
			rep.Gate("non_text_blocks") // would be dropped by a text rewrite
			continue
		}
		content := schema.MessageText(*m)
		trimmed := strings.TrimSpace(content)
		if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
			rep.Gate("not_json_shaped")
			continue
		}
		if schema.TextTokens(content) < f.minTokens {
			rep.Gate("below_min_tokens")
			continue
		}
		var v any
		if err := json.Unmarshal([]byte(trimmed), &v); err != nil {
			rep.Gate("json_parse_failed") // not valid JSON — leave untouched
			continue
		}
		compact, err := json.Marshal(v)
		if err != nil {
			rep.Gate("json_marshal_failed")
			continue
		}
		if schema.TextTokens(string(compact)) >= schema.TextTokens(content) {
			rep.Gate("already_compact")
			continue
		}
		schema.SetMessageText(m, string(compact))
		acted = true
	}
	if !acted {
		rep.Skipped = true
	}
	return nil
}
