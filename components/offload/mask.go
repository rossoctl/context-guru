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
	coldCache     bool
}

type maskConfig struct {
	KeepRecent int `yaml:"keep_recent"`
	MinTokens  int `yaml:"min_tokens"`
	// KeepHeadChars leaves a one-line peek of the masked output inside the marker
	// so the model knows what was hidden (cuts blind expand round-trips); 0 disables.
	KeepHeadChars *int   `yaml:"keep_head_chars"`
	MarkerMode    string `yaml:"marker_mode"` // full (default) | summary | off
	// ColdCache lets a NEW mask act at any depth on a turn whose prompt cache has
	// provably expired (see components.Ctx.TailOnlyCold). Off by default.
	ColdCache bool `yaml:"cold_cache"`
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
	return &Mask{keepRecent: cfg.KeepRecent, minTokens: cfg.MinTokens, keepHeadChars: keepHead,
		mode: parseMarkerMode(cfg.MarkerMode), coldCache: cfg.ColdCache}, nil
}

func (Mask) Name() string                 { return "mask" }
func (Mask) Enabled(*components.Ctx) bool { return true }

func (m *Mask) Offload(req *bschemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) ([]string, error) {
	tools := toolIndices(req)
	if len(tools) <= m.keepRecent {
		// Reported, not silent: mask is the biggest lever in the docs and "acted: 0" with
		// no reason was indistinguishable from a broken component (see docs/reference/components.md).
		rep.Gate("within_keep_recent")
		rep.Skipped = true
		return nil, nil
	}
	var keys []string
	changed := 0
	// Mask every tool output except the most recent keepRecent.
	for _, i := range tools[:len(tools)-m.keepRecent] {
		msg := &req.Input[i]
		if !schema.Rewritable(*msg) {
			rep.Gate("non_text_blocks") // would be dropped by a text rewrite
			continue
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
			rep.Gate("marker_or_kept_verbatim") // already offloaded, or expanded by the agent
			continue
		}
		if schema.TextTokens(content) < m.minTokens {
			rep.Gate("below_min_tokens")
			continue
		}
		// A NEW mask only in the uncached tail: masking content the provider already
		// cached flips it full→masked and forces a cache-write of the suffix. Frozen masks
		// are replayed everywhere above; new ones stay in the tail. The one exception is a
		// freeze this session established and the store then LOST — there the provider
		// already holds the masked bytes, so re-deriving them (deterministic: same content
		// + config ⇒ same text and same sha256 key) PRESERVES the cache and leaving the
		// output verbatim is what destroys it.
		//
		// The one place the depth restriction lifts wholesale is a turn whose cache has
		// provably expired (cold_cache, off by default): there is no cached prefix left to
		// flip, so the freeze below simply establishes the decision a turn earlier than the
		// tail would have.
		if !c.TailOnlyCold(i, m.coldCache) && !repairLostFreeze(c, m.Name(), content) {
			rep.Gate("cached_prefix")
			continue
		}
		prefix := "[older tool output masked] "
		if peek := headPeek(content, m.keepHeadChars); peek != "" {
			prefix = "[older tool output masked; starts: " + peek + "] "
		}
		newText, key, eff, ok := tryMark(c, m.mode, content, " [full output: call "+expand.ToolName+"]",
			func(tok string) string { return prefix + tok })
		if !ok {
			rep.Gate("marker_no_win") // rewrite+marker wouldn't shrink this message
			continue
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
		coldCacheField(),
	})
}

// coldCacheField is the cold_cache descriptor, shared by the age/supersession offloaders
// that can lift their depth restriction on a provably-expired cache (mask, failed_run,
// collapse). Declared once because the trade-off — and the reason it defaults to off —
// is one decision, not three.
func coldCacheField() components.Field {
	return components.Field{Key: "cold_cache", Type: components.FieldBool, Default: false,
		Hint: "On a turn whose prompt cache has provably expired (idle past the provider TTL), also compact at depth instead of only in the uncached tail. Free when the cache really is gone; a wrong cold reading costs a cache-write of the whole suffix, so this is off by default."}
}
