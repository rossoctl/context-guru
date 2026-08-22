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
	coldCache bool
}

type collapseConfig struct {
	MaxTokens  int     `yaml:"max_tokens"`
	MaxFrac    float64 `yaml:"max_frac"` // optional: threshold as a fraction of the model window (wins when window known)
	HeadLines  int     `yaml:"head_lines"`
	TailLines  int     `yaml:"tail_lines"`
	MarkerMode string  `yaml:"marker_mode"` // full (default) | summary | off
	// ColdCache lets a NEW collapse act at any depth on a turn whose prompt cache has
	// provably expired (see components.Ctx.TailOnlyCold). ON by default; see
	// coldCacheDefault.
	ColdCache *bool `yaml:"cold_cache"`
}

func newCollapse(raw []byte) (components.Component, error) {
	cfg := collapseConfig{MaxTokens: 2000, HeadLines: 20, TailLines: 20}
	if err := components.Decode(raw, &cfg); err != nil {
		return nil, err
	}
	return &Collapse{maxTokens: cfg.MaxTokens, maxFrac: cfg.MaxFrac, headLines: cfg.HeadLines,
		tailLines: cfg.TailLines, mode: parseMarkerMode(cfg.MarkerMode), coldCache: coldCacheDefault(cfg.ColdCache)}, nil
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
			rep.Gate("non_text_blocks") // would be dropped by a text rewrite
			continue
		}
		content := schema.MessageText(*m)
		if content == "" {
			continue
		}
		// Replay a previously-frozen collapse on EVERY turn, regardless of depth and BEFORE
		// the size test: the agent re-sends the original each turn, so the only way the
		// representation stays stable is to re-derive the same bytes. Ahead of the size test
		// on purpose — with max_frac set, CtxWindow can resolve differently mid-session
		// (model swap, refreshed modelinfo), and a threshold that drifts above this output
		// would otherwise flip it collapsed→full inside the cached prefix.
		if fk, _, ok := reapplyFrozen(c, cl.Name(), m); ok {
			changed++
			keys = append(keys, fk...)
			continue
		}
		if schema.TextTokens(content) <= maxTokens {
			rep.Gate("below_max_tokens")
			continue
		}
		if skipReduce(c, content) {
			rep.Gate("marker_or_kept_verbatim") // already offloaded, or expanded by the agent
			continue
		}
		lines := strings.Split(content, "\n")
		if len(lines) <= cl.headLines+cl.tailLines {
			rep.Gate("too_few_lines") // few long lines; head/tail wouldn't help
			continue
		}
		// A NEW collapse only in the uncached tail. This component used to carry no depth
		// restriction at all, contradicting the contract in components/component.go: it
		// rewrote the whole transcript on every turn and survived only because the rewrite is
		// deterministic. It is not quite: `max_tokens` is resolved through resolveBudget, so a
		// max_frac config plus a mid-session CtxWindow change silently re-thresholds messages
		// inside the cached prefix. Tail gate + freeze is the same pair mask and failed_run
		// use, and it decides each output once, on the turn it arrives.
		if !c.TailOnlyCold(i, cl.coldCache) && !repairLostFreeze(c, cl.Name(), content) {
			rep.Gate("cached_prefix")
			continue
		}
		omitted := len(lines) - cl.headLines - cl.tailLines
		head := strings.Join(lines[:cl.headLines], "\n")
		tail := strings.Join(lines[len(lines)-cl.tailLines:], "\n")
		newText, key, eff, ok := tryMark(c, cl.mode, content, " [full output: call "+expand.ToolName+"]",
			func(tok string) string {
				return fmt.Sprintf("%s\n... (%d lines omitted) %s\n%s", head, omitted, tok, tail)
			})
		if !ok {
			rep.Gate("marker_no_win") // head/tail window+marker wouldn't shrink this output
			continue
		}
		commitMark(c, rep, eff, key, content)
		schema.SetMessageText(m, newText)
		freeze(c, cl.Name(), content, newText) // freeze so later turns replay it (no churn)
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
		coldCacheField(),
	})
}
