package offload

import (
	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
)

func init() { components.Register("mask", newMask) }

// Mask hides older tool outputs beyond a keep-recent window (after CE-Manager's
// context garbage collection): the newest KeepRecent tool results stay verbatim,
// older ones are replaced with a short marker + stash. Age-based, complementary
// to the content-based offloaders.
type Mask struct {
	keepRecent    int
	minTokens     int
	keepHeadChars int
	mode          markerMode
}

type maskConfig struct {
	KeepRecent int `yaml:"keep_recent"`
	MinTokens  int `yaml:"min_tokens"`
	// KeepHeadChars leaves a one-line peek of the masked output inside the marker
	// so the model knows what was hidden (cuts blind expand round-trips); 0 disables.
	KeepHeadChars *int   `yaml:"keep_head_chars"`
	MarkerMode    string `yaml:"marker_mode"` // full (default) | summary | off
}

func newMask(raw []byte) (components.Component, error) {
	cfg := maskConfig{KeepRecent: 3, MinTokens: 100}
	if err := components.Decode(raw, &cfg); err != nil {
		return nil, err
	}
	keepHead := 96 // default: one-line cue in the marker
	if cfg.KeepHeadChars != nil {
		keepHead = *cfg.KeepHeadChars
	}
	return &Mask{keepRecent: cfg.KeepRecent, minTokens: cfg.MinTokens, keepHeadChars: keepHead, mode: parseMarkerMode(cfg.MarkerMode)}, nil
}

func (Mask) Name() string                 { return "mask" }
func (Mask) Enabled(*components.Ctx) bool { return true }

func (m *Mask) Offload(req *bschemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) ([]string, error) {
	tools := toolIndices(req)
	if len(tools) <= m.keepRecent {
		rep.Skipped = true
		return nil, nil
	}
	var keys []string
	changed := 0
	// Mask every tool output except the most recent keepRecent.
	for _, i := range tools[:len(tools)-m.keepRecent] {
		msg := &req.Input[i]
		if !schema.Rewritable(*msg) {
			continue // non-text blocks would be dropped by a text rewrite
		}
		content := schema.MessageText(*msg)
		if content == "" {
			continue
		}
		// Reapply a previously-frozen mask on EVERY turn (cache-stable), regardless of
		// the tail boundary: the agent re-sends the original, so we must re-mask it to the
		// same bytes or it reverts full→masked→full and churns the provider KV cache. This
		// also skips kept-verbatim content (see reapplyFrozen).
		if fk, _, ok := reapplyFrozen(c, m.Name(), msg); ok {
			changed++
			keys = append(keys, fk...)
			continue
		}
		if skipReduce(c, content) {
			continue // already offloaded, or expanded by the agent — don't re-hide
		}
		if schema.TextTokens(content) < m.minTokens {
			continue
		}
		// A NEW mask only in the uncached tail: masking content the provider already
		// cached flips it full→masked and forces a cache-write of the suffix. Frozen masks
		// are replayed everywhere above; new ones stay in the tail. The one exception is a
		// freeze this session established and the store then LOST — there the provider
		// already holds the masked bytes, so re-deriving them (deterministic: same content
		// + config ⇒ same text and same sha256 key) PRESERVES the cache and leaving the
		// output verbatim is what destroys it.
		if !c.TailOnly(i) && !repairLostFreeze(c, m.Name(), content) {
			continue
		}
		prefix := "[older tool output masked] "
		if peek := headPeek(content, m.keepHeadChars); peek != "" {
			prefix = "[older tool output masked; starts: " + peek + "] "
		}
		newText, key, eff, ok := tryMark(c, m.mode, content, " [full output: call "+expand.ToolName+"]",
			func(tok string) string { return prefix + tok })
		if !ok {
			continue // marker-inclusive rewrite wouldn't shrink this message; leave it verbatim
		}
		commitMark(c, rep, eff, key, content)
		schema.SetMessageText(msg, newText)
		freeze(c, m.Name(), content, newText) // freeze so later turns replay it (no churn)
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
	components.RegisterFields("mask", maskConfig{}, []components.Field{
		{Key: "keep_recent", Type: components.FieldInt, Default: 3,
			Hint: "How many of the newest tool results stay verbatim. Everything older is masked; 0 masks them all."},
		{Key: "min_tokens", Type: components.FieldInt, Default: 100, Min: 1,
			Hint: "Only mask an output above this many tokens."},
		{Key: "keep_head_chars", Type: components.FieldInt, Default: 96,
			Hint: "Leave a one-line peek of the masked output inside the marker so the model knows what was hidden (cuts blind expand round-trips); 0 disables."},
		markerModeField(),
	})
}
