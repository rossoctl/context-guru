package offload

import (
	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
)

func init() { components.Register("extract", newExtract) }

// Extract is the DETERMINISTIC (no-LLM) tool-output reducer. It runs every request
// (cheap) and is deliberately CONSERVATIVE: it removes only OBVIOUS, provably
// redundant noise (exactly repeated lines/blocks, runs of blank lines, progress
// bars/spinners) via collapseObviousNoise, keeping every unique informative line
// verbatim. This guarantees it can never hide content the agent needs and force it
// to redo work. Relevance-aware, aggressive trimming (regex rewrites, summaries) is
// the job of the separate `extract_llm` component — configure them together
// (`[extract, extract_llm]`) so the cheap pass runs every step and the LLM pass only
// every few steps.
//
// It is byte-stable on unchanged content (deterministic per-message), so it is
// cache-safe and needs no cache-boundary restriction. The full original is stashed
// under a marker (reversible via the expand tool).
type Extract struct {
	minTokens int
	trigger   components.Trigger
	mode      markerMode
}

type extractConfig struct {
	MinTokens  int                `yaml:"min_tokens"`
	Trigger    components.Trigger `yaml:"trigger"`
	MarkerMode string             `yaml:"marker_mode"` // full (default) | summary | off
}

func newExtract(raw []byte) (components.Component, error) {
	cfg := extractConfig{MinTokens: 300}
	if err := components.Decode(raw, &cfg); err != nil {
		return nil, err
	}
	return &Extract{minTokens: cfg.MinTokens, trigger: cfg.Trigger, mode: parseMarkerMode(cfg.MarkerMode)}, nil
}

func (Extract) Name() string                 { return "extract" }
func (Extract) Enabled(*components.Ctx) bool { return true }

func (e *Extract) outputFloor(window int) int {
	return e.trigger.OutputFloor(window, e.minTokens)
}

func (e *Extract) Offload(req *bschemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) ([]string, error) {
	floor := e.outputFloor(c.CtxWindow)
	var keys []string
	changed := 0
	for _, i := range toolIndices(req) {
		msg := &req.Input[i]
		if !schema.Rewritable(*msg) {
			rep.Gate("non_text_blocks") // would be dropped by a text rewrite
			continue
		}
		content := schema.MessageText(*msg)
		if content == "" || schema.TextTokens(content) < floor {
			rep.Gate("below_output_floor")
			continue
		}
		if skipReduce(c, content) {
			rep.Gate("marker_or_kept_verbatim") // don't re-reduce
			continue
		}
		projected, ok := collapseObviousNoise(content)
		if !ok || schema.TextTokens(projected) >= schema.TextTokens(content) {
			rep.Gate("no_obvious_noise")
			continue
		}
		newText, key, eff, ok2 := tryMark(c, e.mode, content, " [full output: call "+expand.ToolName+"]",
			func(tok string) string { return projected + "\n" + tok })
		if !ok2 {
			rep.Gate("marker_no_win") // projection+marker wouldn't shrink this message
			continue
		}
		if !commitMark(c, rep, eff, key, content) {
			continue // the store cannot back the marker; leave this message verbatim
		}
		schema.SetMessageText(msg, newText)
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
	f := []components.Field{
		{Key: "min_tokens", Type: components.FieldInt, Default: 300, Min: 1,
			Hint: "Per-output floor: only extract from a tool output above this many tokens."},
		markerModeField(),
	}
	components.RegisterFields("extract", extractConfig{}, append(f, components.TriggerFields("trigger")...))
}
