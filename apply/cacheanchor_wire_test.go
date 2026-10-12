package apply

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/components"
	_ "github.com/rossoctl/context-guru/components/all"
	"github.com/rossoctl/context-guru/store"
)

// anchorBody is a Claude Code shaped request: two marked system blocks, a tool, and (assistant tool_use,
// user tool_result) pairs with a mark on the last message only.
func anchorBody(turns int) []byte {
	msgs := []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "investigate"}}}}
	for t := 1; t <= turns; t++ {
		var sb strings.Builder
		lines := 120
		if t <= 4 {
			lines = 3 // small early outputs are never masked, so the early prefix stays unchanged
		}
		for j := 0; j < lines; j++ {
			fmt.Fprintf(&sb, "2026-10-11 worker-%d reconciled batch %d for catalog item %d ok\n", j%7, t*1000+j, j)
		}
		id := fmt.Sprintf("toolu_%03d", t)
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "step"},
				map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": "s"}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": sb.String()}}})
	}
	last := msgs[len(msgs)-1].(map[string]any)["content"].([]any)
	last[len(last)-1].(map[string]any)["cache_control"] = map[string]any{"type": "ephemeral"}
	b, _ := json.Marshal(map[string]any{
		"model": "claude-opus-5", "max_tokens": 64,
		"system": []any{
			map[string]any{"type": "text", "text": "billing header", "cache_control": map[string]any{"type": "ephemeral"}},
			map[string]any{"type": "text", "text": "You are an agent.", "cache_control": map[string]any{"type": "ephemeral"}}},
		"tools":    []any{map[string]any{"name": "Bash", "description": "run", "input_schema": map[string]any{"type": "object"}}},
		"messages": msgs})
	return b
}

// assistantMarks lists assistant messages other than the last message that carry cache_control.
func assistantMarks(body []byte) []int {
	var at []int
	for i, m := range gjson.GetBytes(body, "messages").Array() {
		if m.Get("role").String() != "assistant" {
			continue
		}
		for _, b := range m.Get("content").Array() {
			if b.Get("cache_control").Exists() {
				at = append(at, i)
			}
		}
	}
	return at
}

func TestCacheAnchorReachesTheWireBehindABatchedDeepMask(t *testing.T) {
	run := func(spec string) (anchored int, maxBP int, edited bool) {
		var comps []components.Component
		for _, name := range strings.Split(spec, ",") {
			cfg := ""
			if name == "mask" {
				cfg = "keep_recent: 2\nmin_tokens: 100\ndeep_batch: 3\n"
			}
			c, err := components.New(name, []byte(cfg))
			if err != nil {
				t.Fatal(err)
			}
			comps = append(comps, c)
		}
		pipe := components.NewPipeline(comps, nil)
		st := store.NewMemory(store.Options{MaxEntries: 4000})
		for turns := 1; turns <= 16; turns++ {
			raw := anchorBody(turns)
			res := BodyOpts(context.Background(), pipe, st, Opts{Provider: bschemas.Anthropic, Body: raw, Session: "anchor-wire"})
			out := res.Body
			if out == nil {
				out = raw
			}
			if n := CountBreakpoints(out).Total(); n > maxBP {
				maxBP = n
			}
			if len(assistantMarks(out)) > 0 {
				anchored++
			}
			if res.Changed && strings.Contains(string(out), "masked") {
				edited = true
			}
		}
		return
	}
	a, bp, edited := run("mask,cacheanchor")
	if !edited {
		t.Fatal("the batched mask never edited history, so the scenario did not exercise the anchor")
	}
	if a == 0 {
		t.Error("an edit inside the cached history was sent without an anchor breakpoint")
	}
	if bp > 4 {
		t.Errorf("%d breakpoints on the wire, a fifth is a 400", bp)
	}
	if c, _, _ := run("mask"); c != 0 {
		t.Errorf("control: without cacheanchor no assistant turn should carry a mark, got %d requests", c)
	}
}
