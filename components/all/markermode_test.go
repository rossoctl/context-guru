package all_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
)

// bigTool builds a tool output extract will reduce: one informative line, then the same
// noise line repeated well past its token floor.
func bigTool() *schemas.BifrostChatRequest {
	var b strings.Builder
	b.WriteString("line 50: the only informative line\n")
	for i := 0; i < 120; i++ {
		fmt.Fprintf(&b, "retry: connection refused to the database host, waiting\n")
	}
	return &schemas.BifrostChatRequest{Input: []schemas.ChatMessage{toolMsg(b.String())}}
}

// TestMarkerModeSummarySurvivesWire drives the summary sentinel through the real
// wire path (apply.Body → JSON splice, Anthropic tool_result shape) to confirm the
// non-ASCII ⟪cg⟫ survives JSON round-trip and HasPlaceholder still matches it — the
// cross-turn skip-detection depends on this (HANDOVER review item 5).
func TestMarkerModeSummarySurvivesWire(t *testing.T) {
	cfg, err := config.LoadBytes([]byte("pipeline: [extract]\ncomponents:\n  extract: {min_tokens: 10, marker_mode: summary}\n"))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := cfg.Build(nil)
	big := "line 1: header\n" + strings.Repeat("a fairly long line of tool output content to reduce\n", 60)
	body, _ := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "user", "content": "go"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": big},
			}},
		},
	})
	out, changed := apply.Body(context.Background(), p, store.NewMemory(store.Options{}), schemas.Anthropic, body, "", false)
	if !changed {
		t.Fatal("summary-mode extract should have reduced the tool_result")
	}
	got := gjson.GetBytes(out, "messages.1.content.0.content").String()
	if !strings.Contains(got, expand.SummaryMarker) || !expand.HasPlaceholder(got) {
		t.Fatalf("⟪cg⟫ sentinel did not survive the wire round-trip: %q", got)
	}
	if len(expand.ParseMarkers(got)) != 0 {
		t.Fatalf("summary mode must not leave a resolvable marker: %q", got)
	}
}

// TestMarkerModeFull is the default: content is dropped, a resolvable <<cg:HASH>>
// marker is left, and the original is recoverable from the Store.
func TestMarkerModeFull(t *testing.T) {
	req := bigTool()
	before := schema.MessagesTokens(req)
	_, st := run(t, "pipeline: [extract]\ncomponents:\n  extract: {min_tokens: 10}\n", req)
	got := schema.MessageText(req.Input[0])
	if schema.MessagesTokens(req) >= before {
		t.Fatalf("full: expected reduction, before=%d after=%d", before, schema.MessagesTokens(req))
	}
	keys := expand.ParseMarkers(got)
	if len(keys) != 1 {
		t.Fatalf("full: expected one resolvable marker, got %q", got)
	}
	if orig, ok := expand.Resolve(st, keys[0]); !ok || !strings.Contains(orig, "line 50:") {
		t.Fatal("full: original must be recoverable from the Store")
	}
}

// TestMarkerModeSummary: content is dropped, a non-resolvable ⟪cg⟫ sentinel is
// left (recognized by HasPlaceholder for cross-turn skip), and nothing is stashed
// — the reduction must survive the pipeline's dropped-without-stash guard.
func TestMarkerModeSummary(t *testing.T) {
	req := bigTool()
	before := schema.MessagesTokens(req)
	run(t, "pipeline: [extract]\ncomponents:\n  extract: {min_tokens: 10, marker_mode: summary}\n", req)
	got := schema.MessageText(req.Input[0])
	if schema.MessagesTokens(req) >= before {
		t.Fatalf("summary: expected reduction (guard must not revert), before=%d after=%d", before, schema.MessagesTokens(req))
	}
	if len(expand.ParseMarkers(got)) != 0 {
		t.Fatalf("summary: must NOT leave a resolvable <<cg:HASH>> marker, got %q", got)
	}
	if !strings.Contains(got, expand.SummaryMarker) || !expand.HasPlaceholder(got) {
		t.Fatalf("summary: expected a ⟪cg⟫ sentinel, got %q", got)
	}
}

// TestMarkerModeOff: content is dropped with no marker at all; the reduction must
// still survive the guard.
func TestMarkerModeOff(t *testing.T) {
	req := bigTool()
	before := schema.MessagesTokens(req)
	run(t, "pipeline: [extract]\ncomponents:\n  extract: {min_tokens: 10, marker_mode: off}\n", req)
	got := schema.MessageText(req.Input[0])
	if schema.MessagesTokens(req) >= before {
		t.Fatalf("off: expected reduction (guard must not revert), before=%d after=%d", before, schema.MessagesTokens(req))
	}
	if expand.HasPlaceholder(got) {
		t.Fatalf("off: expected no marker or sentinel, got %q", got)
	}
}
