package offload

import (
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
)

func init() { components.Register("dedup", newDedup) }

// Dedup replaces a tool output that is byte-identical to an earlier one in the
// same request with a short pointer + expand marker, stashing the original.
// Exact-match only in v1; near-duplicate (similarity threshold) is deferred.
type Dedup struct {
	minTokens int
	mode      markerMode
}

type dedupConfig struct {
	MinTokens  int    `yaml:"min_tokens"`
	MarkerMode string `yaml:"marker_mode"` // full (default) | summary | off
}

func newDedup(raw []byte) (components.Component, error) {
	cfg := dedupConfig{MinTokens: 100}
	if err := components.Decode(raw, &cfg); err != nil {
		return nil, err
	}
	return &Dedup{minTokens: cfg.MinTokens, mode: parseMarkerMode(cfg.MarkerMode)}, nil
}

func (Dedup) Name() string                 { return "dedup" }
func (Dedup) Enabled(*components.Ctx) bool { return true }

func (d *Dedup) Offload(req *schemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) ([]string, error) {
	seen := map[string]int{} // content hash -> first message index
	var keys []string
	changed := 0
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
		if content == "" || schema.TextTokens(content) < d.minTokens {
			rep.Gate("below_min_tokens")
			continue
		}
		if skipReduce(c, content) {
			rep.Gate("marker_or_kept_verbatim") // don't re-reduce
			continue
		}
		h := hashKey(content)
		if _, dup := seen[h]; !dup {
			seen[h] = i
			rep.Gate("no_earlier_identical_output")
			continue
		}
		// Later duplicate: collapse to a pointer (stash+marker in full mode).
		newText, key, eff, ok := tryMark(c, d.mode, content, "",
			func(tok string) string { return "[identical to an earlier tool output] " + tok })
		if !ok {
			rep.Gate("marker_no_win") // pointer+marker wouldn't shrink this duplicate
			continue
		}
		commitMark(c, rep, eff, key, content)
		schema.SetMessageText(m, newText)
		changed++
		if key != "" {
			keys = append(keys, key)
		}
	}
	if changed == 0 {
		rep.Skipped = true
	}
	return keys, nil
}

func init() {
	components.RegisterFields("dedup", dedupConfig{}, []components.Field{
		{Key: "min_tokens", Type: components.FieldInt, Default: 100, Min: 1,
			Hint: "Only replace a repeated tool output above this many tokens with a pointer to the first copy."},
		markerModeField(),
	})
}
