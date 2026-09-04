package offload

import (
	"encoding/json"
	"fmt"
	"strings"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
)

func init() { components.Register("smartcrush", newSmartCrush) }

// SmartCrush is headroom's statistical JSON-array compressor, in essence: keep a
// first-K/last-K anchor window plus any item that carries an error signal, drop
// the rest, and stash the full original. Schema-preserving (kept items are
// verbatim originals). v1 uses fixed anchors; headroom's Kneedle adaptive-K is a
// documented refinement.
type SmartCrush struct {
	minItems  int
	minTokens int
	keepFirst int
	keepLast  int
	mode      markerMode
}

type smartCrushConfig struct {
	MinItems   int    `yaml:"min_items"`
	MinTokens  int    `yaml:"min_tokens"`
	KeepFirst  int    `yaml:"keep_first"`
	KeepLast   int    `yaml:"keep_last"`
	MarkerMode string `yaml:"marker_mode"` // full (default) | summary | off
}

func newSmartCrush(raw []byte) (components.Component, error) {
	cfg := smartCrushConfig{MinItems: 5, MinTokens: 200, KeepFirst: 3, KeepLast: 2}
	if err := components.Decode(raw, &cfg); err != nil {
		return nil, err
	}
	return &SmartCrush{minItems: cfg.MinItems, minTokens: cfg.MinTokens, keepFirst: cfg.KeepFirst, keepLast: cfg.KeepLast, mode: parseMarkerMode(cfg.MarkerMode)}, nil
}

func (SmartCrush) Name() string                 { return "smartcrush" }
func (SmartCrush) Enabled(*components.Ctx) bool { return true }

func (s *SmartCrush) Offload(req *bschemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) ([]string, error) {
	var keys []string
	changed := 0
	for _, i := range toolIndices(req) {
		msg := &req.Input[i]
		if !schema.Rewritable(*msg) {
			continue // non-text blocks would be dropped by a text rewrite
		}
		content := schema.MessageText(*msg)
		if gate, skip := skipReduce(c, content); skip {
			// smartcrush raised NO gate here, so its share of both reasons was invisible even in
			// the conflated form. It has no reapplyFrozen path, so kept-verbatim content is
			// precisely where its per-turn work is abandoned.
			rep.Gate(gate)
			continue // already offloaded by an earlier component/turn, or expanded by the agent
		}
		trimmed := strings.TrimSpace(content)
		if len(trimmed) == 0 || trimmed[0] != '[' || schema.TextTokens(content) < s.minTokens {
			continue
		}
		var items []json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &items); err != nil || len(items) < s.minItems {
			continue
		}
		keep := s.keepSet(items)
		if len(keep) >= len(items) {
			continue // nothing to drop
		}
		kept := make([]json.RawMessage, 0, len(keep))
		for idx := range items {
			if _, ok := keep[idx]; ok {
				kept = append(kept, items[idx])
			}
		}
		crushed, err := json.Marshal(kept)
		if err != nil {
			continue
		}
		note := fmt.Sprintf(" [%d of %d items shown] ", len(kept), len(items))
		newText, key, eff, ok := tryMark(c, s.mode, content, " [full array: call "+expand.ToolName+"]",
			func(tok string) string { return string(crushed) + note + tok })
		if !ok {
			continue // crushed array+marker wouldn't shrink this message; leave it verbatim
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

// keepSet is the set of item indices to preserve: first-K, last-K, and any item
// whose raw JSON carries an error signal.
func (s *SmartCrush) keepSet(items []json.RawMessage) map[int]struct{} {
	keep := map[int]struct{}{}
	for i := 0; i < s.keepFirst && i < len(items); i++ {
		keep[i] = struct{}{}
	}
	for i := len(items) - s.keepLast; i < len(items); i++ {
		if i >= 0 {
			keep[i] = struct{}{}
		}
	}
	for i, it := range items {
		if hasError(string(it)) {
			keep[i] = struct{}{}
		}
	}
	return keep
}

func init() {
	components.RegisterFields("smartcrush", smartCrushConfig{}, []components.Field{
		{Key: "min_items", Type: components.FieldInt, Default: 5, Min: 1,
			Hint: "Only crush a list carrying at least this many items."},
		{Key: "min_tokens", Type: components.FieldInt, Default: 200, Min: 1,
			Hint: "Only crush a list above this many tokens."},
		{Key: "keep_first", Type: components.FieldInt, Default: 3,
			Hint: "Items kept verbatim at the head of the list."},
		{Key: "keep_last", Type: components.FieldInt, Default: 2,
			Hint: "Items kept verbatim at the tail of the list."},
		markerModeField(),
	})
}
