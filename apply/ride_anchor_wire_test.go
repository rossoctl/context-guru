package apply

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/components"
	_ "github.com/rossoctl/context-guru/components/all"
	"github.com/rossoctl/context-guru/store"
)

// parkAsker answers the ride's triage ask by parking every offered output.
type parkAsker struct{ asks int }

var rideLabelRe = regexp.MustCompile(`\[(\d+)\] \d+ tokens`)

func (a *parkAsker) Ask(ctx context.Context, s, ask string) (string, components.PrefixUsage, error) {
	return a.AskAs(ctx, s, ask, "user")
}

func (a *parkAsker) AskAs(_ context.Context, _, ask, _ string) (string, components.PrefixUsage, error) {
	a.asks++
	var parts []string
	for _, l := range rideLabelRe.FindAllStringSubmatch(ask, -1) {
		parts = append(parts, `{"label":`+l[1]+`,"action":"park"}`)
	}
	return "[" + strings.Join(parts, ",") + "]", components.PrefixUsage{CacheRead: 50000, Fresh: 500, Output: 100}, nil
}

// ccBody is a Claude Code shaped request: two marked system blocks (the second carries the volatile git
// snapshot the system split looks for), a tool, and (assistant tool_use, user tool_result) pairs with a
// mark on the last message only.
func ccBody(turns int, big map[int]bool) []byte {
	snap := strings.Repeat("Stable instructions for the agent. ", 200) + "\nCurrent branch: main\nRecent commits:\nabc fix\n"
	msgs := []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "investigate"}}}}
	for t := 1; t <= turns; t++ {
		lines := 100
		if big[t] {
			lines = 400
		}
		var sb strings.Builder
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
			map[string]any{"type": "text", "text": snap, "cache_control": map[string]any{"type": "ephemeral"}}},
		"tools":    []any{map[string]any{"name": "Bash", "description": "run", "input_schema": map[string]any{"type": "object"}}},
		"messages": msgs})
	return b
}

// anchorOnWire returns the index of an assistant message (not the last message) carrying cache_control.
func anchorOnWire(body []byte) int {
	for i, m := range gjson.GetBytes(body, "messages").Array() {
		if m.Get("role").String() != "assistant" {
			continue
		}
		for _, b := range m.Get("content").Array() {
			if b.Get("cache_control").Exists() {
				return i
			}
		}
	}
	return -1
}

func TestRideAnchorReachesTheWireAndCachesplitDoesNotTakeItsSlot(t *testing.T) {
	rideCfg := "apply_mode: cold\nanchor: true\nmin_tokens: 3000\nmin_inventory: 2\ntrigger_pending: 2\n" +
		"trigger_size_tokens: 1000\nmin_later_turns: 2\nmin_turns_between: 4\nhorizon_turns: 30\n"
	ride, err := components.New("extract_llm_ride", []byte(rideCfg))
	if err != nil {
		t.Fatal(err)
	}
	split, err := components.New("cachesplit", nil)
	if err != nil {
		t.Fatal(err)
	}
	big := map[int]bool{1: true, 2: true, 8: true, 9: true}

	// Control: the system split alone does add a system block (and a breakpoint), so the property below is
	// not vacuous.
	only := BodyOpts(context.Background(), components.NewPipeline([]components.Component{split}, nil),
		store.NewMemory(store.Options{}), Opts{Provider: bschemas.Anthropic, Body: ccBody(3, big), Session: "ctl"})
	if got, in := len(gjson.GetBytes(only.Body, "system").Array()), 2; got != in+1 {
		t.Fatalf("control: cachesplit alone should split the volatile system block (%d blocks)", got)
	}

	pipe := components.NewPipeline([]components.Component{ride, split}, nil)
	st := store.NewMemory(store.Options{MaxEntries: 4000})
	asker := &parkAsker{}
	anchored := false
	for turns := 1; turns <= 14; turns++ {
		res := BodyOpts(context.Background(), pipe, st, Opts{Provider: bschemas.Anthropic, Body: ccBody(turns, big),
			Session: "anchor-wire", PrefixAsk: asker})
		out := res.Body
		if out == nil {
			out = ccBody(turns, big)
		}
		if n := CountBreakpoints(out).Total(); n > 4 {
			t.Fatalf("turn %d: %d breakpoints on the wire, a fifth is a 400", turns, n)
		}
		if got := len(gjson.GetBytes(out, "system").Array()); got != 2 {
			t.Fatalf("turn %d: the system block was split (%d blocks) although the ride needs the slot", turns, got)
		}
		if anchorOnWire(out) >= 0 {
			anchored = true
			if n := CountBreakpoints(out).Total(); n != 4 {
				t.Errorf("turn %d: anchored request carries %d breakpoints, want 4", turns, n)
			}
		}
	}
	if asker.asks == 0 {
		t.Fatal("the ride never asked, so the scenario did not exercise the anchor")
	}
	if !anchored {
		t.Error("no request carried the anchor breakpoint although cachesplit was in the pipeline")
	}
}
