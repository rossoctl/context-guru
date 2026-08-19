package offload

import (
	"fmt"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
)

func init() { components.Register("collapse", newCollapse) }

// Collapse is the content-agnostic fallback for an oversized tool output that no
// more specific component handled: it keeps a head + tail window, stashes the
// full original, and leaves an expand marker. Runs late in the pipeline (after
// cmdfilter/format), and skips anything already carrying a marker so it never
// double-collapses.
type Collapse struct {
	maxTokens int
	maxFrac   float64
	headLines int
	tailLines int
	mode      markerMode
}

type collapseConfig struct {
	MaxTokens  int     `yaml:"max_tokens"`
	MaxFrac    float64 `yaml:"max_frac"` // optional: threshold as a fraction of the model window (wins when window known)
	HeadLines  int     `yaml:"head_lines"`
	TailLines  int     `yaml:"tail_lines"`
	MarkerMode string  `yaml:"marker_mode"` // full (default) | summary | off
}

func newCollapse(raw []byte) (components.Component, error) {
	cfg := collapseConfig{MaxTokens: 2000, HeadLines: 20, TailLines: 20}
	if err := components.Decode(raw, &cfg); err != nil {
		return nil, err
	}
	return &Collapse{maxTokens: cfg.MaxTokens, maxFrac: cfg.MaxFrac, headLines: cfg.HeadLines, tailLines: cfg.TailLines, mode: parseMarkerMode(cfg.MarkerMode)}, nil
}

func (Collapse) Name() string                 { return "collapse" }
func (Collapse) Enabled(*components.Ctx) bool { return true }

func (cl *Collapse) Offload(req *schemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) ([]string, error) {
	maxTokens := resolveBudget(cl.maxTokens, cl.maxFrac, c.CtxWindow) // frac of window wins when known
	var keys []string
	changed := 0
	for i := range req.Input {
		m := &req.Input[i]
		if m.Role != schemas.ChatMessageRoleTool {
			continue
		}
		if !schema.Rewritable(*m) {
			continue // non-text blocks would be dropped by a text rewrite
		}
		content := schema.MessageText(*m)
		if content == "" || schema.TextTokens(content) <= maxTokens {
			continue
		}
		if skipReduce(c, content) {
			continue // already offloaded by an earlier component, or expanded by the agent
		}
		lines := strings.Split(content, "\n")
		if len(lines) <= cl.headLines+cl.tailLines {
			continue // few long lines; head/tail wouldn't help
		}
		omitted := len(lines) - cl.headLines - cl.tailLines
		head := strings.Join(lines[:cl.headLines], "\n")
		tail := strings.Join(lines[len(lines)-cl.tailLines:], "\n")
		newText, key, eff, ok := tryMark(c, cl.mode, content, " [full output: call "+expand.ToolName+"]",
			func(tok string) string {
				return fmt.Sprintf("%s\n... (%d lines omitted) %s\n%s", head, omitted, tok, tail)
			})
		if !ok {
			continue // head/tail window+marker wouldn't shrink this output; leave it verbatim
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
	components.RegisterFields("collapse", collapseConfig{}, []components.Field{
		{Key: "max_tokens", Type: components.FieldInt, Default: 2000,
			Hint: "Collapse any output above this many tokens to its head and tail. 0 leaves the threshold to max_frac."},
		{Key: "max_frac", Type: components.FieldFloat,
			Hint: "The same threshold as a fraction of the model's context window; wins when the window is known. 0 = unset."},
		{Key: "head_lines", Type: components.FieldInt, Default: 20,
			Hint: "Lines kept from the start of a collapsed output."},
		{Key: "tail_lines", Type: components.FieldInt, Default: 20,
			Hint: "Lines kept from the end of a collapsed output."},
		markerModeField(),
	})
}
