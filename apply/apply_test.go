package apply_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	_ "github.com/rossoctl/context-guru/components/all"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func pipe(t *testing.T, yaml string) *config.Config {
	t.Helper()
	c, err := config.LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestI1UnmodifiedMessagesAndFieldsPreserved is the cache-safety invariant:
// messages a component doesn't touch, and every non-messages top-level field,
// come out byte-identical (headroom I1).
func TestI1UnmodifiedMessagesAndFieldsPreserved(t *testing.T) {
	cfg := pipe(t, "pipeline: [dedup]\n")
	p, _ := cfg.Build(nil)
	st := store.NewMemory(store.Options{})

	dump := strings.Repeat("repeated tool output line with content\n", 60)
	body, _ := json.Marshal(map[string]any{
		"model":       "gpt-x",
		"temperature": 0.7,
		"top_p":       0.95,
		"metadata":    map[string]any{"user_id": "u1"},
		"messages": []map[string]any{
			{"role": "system", "content": "you are helpful"},
			{"role": "user", "content": "please help"},
			{"role": "tool", "tool_call_id": "a", "content": dump},
			{"role": "tool", "tool_call_id": "b", "content": dump}, // dedup collapses this one
		},
	})

	out, changed := apply.Body(context.Background(), p, st, bschemas.OpenAI, body, "", false)
	if !changed {
		t.Fatal("expected dedup to change the body")
	}

	// Non-messages fields: byte-identical.
	for _, path := range []string{"model", "temperature", "top_p", "metadata.user_id"} {
		if gjson.GetBytes(out, path).Raw != gjson.GetBytes(body, path).Raw {
			t.Fatalf("field %q not preserved: %q -> %q", path, gjson.GetBytes(body, path).Raw, gjson.GetBytes(out, path).Raw)
		}
	}
	// Untouched messages (system, user, first tool) are byte-identical.
	for _, i := range []string{"0", "1", "2"} {
		if gjson.GetBytes(out, "messages."+i).Raw != gjson.GetBytes(body, "messages."+i).Raw {
			t.Fatalf("message %s should be unmodified:\n old=%s\n new=%s", i,
				gjson.GetBytes(body, "messages."+i).Raw, gjson.GetBytes(out, "messages."+i).Raw)
		}
	}
	// Only the duplicate (index 3) changed.
	if gjson.GetBytes(out, "messages.3.content").Raw == gjson.GetBytes(body, "messages.3.content").Raw {
		t.Fatal("the duplicate tool output should have been collapsed")
	}
}

// TestLosslessGuardProtectsUnmodeledFields is the data-safety invariant: a
// component that modifies a message bifrost can't round-trip losslessly (here an
// Anthropic user turn carrying a tool_result block, whose payload bifrost drops)
// must NOT corrupt it. cacheinject would add cache_control to that boundary
// message; the guard discards the change rather than splice a lossy re-marshal.
func TestLosslessGuardProtectsUnmodeledFields(t *testing.T) {
	cfg := pipe(t, "pipeline: [cacheinject]\n")
	p, _ := cfg.Build(nil)
	st := store.NewMemory(store.Options{})

	body, _ := json.Marshal(map[string]any{
		"model": "claude-x",
		"messages": []any{
			map[string]any{"role": "user", "content": "please run the tool"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "tu_1", "content": "CRITICAL TOOL OUTPUT that must survive"},
			}},
			map[string]any{"role": "user", "content": "now answer"},
		},
	})

	out, _ := apply.Body(context.Background(), p, st, bschemas.Anthropic, body, "", false)

	// The tool_result payload must be byte-identical — never dropped.
	if gjson.GetBytes(out, "messages.1").Raw != gjson.GetBytes(body, "messages.1").Raw {
		t.Fatalf("tool_result message corrupted:\n old=%s\n new=%s",
			gjson.GetBytes(body, "messages.1").Raw, gjson.GetBytes(out, "messages.1").Raw)
	}
	if !strings.Contains(string(out), "CRITICAL TOOL OUTPUT") {
		t.Fatalf("tool_result content was dropped: %s", out)
	}
}

// TestMixedContentNotFlattened proves an offload skips a tool message that
// carries a non-text block (an image), so the image is never silently dropped.
func TestMixedContentNotFlattened(t *testing.T) {
	cfg := pipe(t, "pipeline: [collapse]\ncomponents:\n  collapse: {max_tokens: 5, head_lines: 1, tail_lines: 1}\n")
	p, _ := cfg.Build(nil)
	st := store.NewMemory(store.Options{})

	long := strings.Repeat("verbose tool line that would normally be collapsed\n", 40)
	body, _ := json.Marshal(map[string]any{
		"model": "gpt-x",
		"messages": []any{
			map[string]any{"role": "user", "content": "go"},
			map[string]any{"role": "tool", "tool_call_id": "a", "content": []any{
				map[string]any{"type": "text", "text": long},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
			}},
		},
	})

	out, changed := apply.Body(context.Background(), p, st, bschemas.OpenAI, body, "", false)
	if changed {
		t.Fatal("collapse must skip a mixed text+image message, not rewrite it")
	}
	if gjson.GetBytes(out, "messages.1.content.1.image_url.url").String() != "data:image/png;base64,AAAA" {
		t.Fatalf("image block was dropped: %s", out)
	}
}

// TestAnthropicToolResultOffloaded is the payoff for the normalization layer:
// dedup must now fire on Anthropic tool outputs (tool_result blocks inside user
// messages), collapsing a duplicate while preserving the block's siblings
// (tool_use_id, is_error) and every other message byte-for-byte.
func TestAnthropicToolResultOffloaded(t *testing.T) {
	cfg := pipe(t, "pipeline: [dedup]\ncomponents:\n  dedup: {min_tokens: 20}\n")
	p, _ := cfg.Build(nil)
	st := store.NewMemory(store.Options{})

	long := strings.Repeat("a line of tool output that repeats verbatim across two calls\n", 30)
	body, _ := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "user", "content": "fix the failing test"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": long},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "t2", "is_error": false, "content": long},
			}},
		},
	})

	out, changed := apply.Body(context.Background(), p, st, bschemas.Anthropic, body, "", false)
	if !changed {
		t.Fatal("dedup should have fired on the Anthropic tool_result duplicate")
	}
	// First tool_result stays verbatim; the duplicate is collapsed to a pointer.
	if gjson.GetBytes(out, "messages.1.content.0.content").String() != long {
		t.Fatal("the first tool_result must be left untouched")
	}
	dup := gjson.GetBytes(out, "messages.2.content.0.content").String()
	if !strings.Contains(dup, "identical to an earlier") || !strings.Contains(dup, "<<cg:") {
		t.Fatalf("the duplicate tool_result was not collapsed: %q", dup)
	}
	// Block siblings survive the rewrite (only the content string changed).
	if gjson.GetBytes(out, "messages.2.content.0.tool_use_id").String() != "t2" {
		t.Fatalf("tool_use_id must be preserved: %s", gjson.GetBytes(out, "messages.2").Raw)
	}
	if gjson.GetBytes(out, "messages.2.content.0.type").String() != "tool_result" {
		t.Fatal("block type must be preserved")
	}
	if gjson.GetBytes(out, "model").String() != "claude-sonnet-4-6" {
		t.Fatal("model field must be preserved")
	}
}

// TestAnthropicStructuredToolResultKeepsNonTextBlocks: a tool_result whose content is a
// structured array has its TEXT blocks compacted in place (each at its own
// content.<k>.text path) — skipping the whole array made 100% of such a request's tool
// output silently uncompactable. Non-text blocks (images) are never touched, so no data
// is dropped.
func TestAnthropicStructuredToolResultKeepsNonTextBlocks(t *testing.T) {
	cfg := pipe(t, "pipeline: [collapse]\ncomponents:\n  collapse: {max_tokens: 5, head_lines: 1, tail_lines: 1}\n")
	p, _ := cfg.Build(nil)
	st := store.NewMemory(store.Options{})

	body, _ := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "user", "content": "go"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": []any{
					map[string]any{"type": "text", "text": strings.Repeat("structured line\n", 40)},
					map[string]any{"type": "image", "source": map[string]any{"data": "AAAA"}},
				}},
			}},
		},
	})
	out, changed := apply.Body(context.Background(), p, st, bschemas.Anthropic, body, "", false)
	if !changed {
		t.Fatal("the text block inside the structured tool_result should have been collapsed")
	}
	if s := gjson.GetBytes(out, "messages.1.content.0.content.0.text").String(); !strings.Contains(s, "<<cg:") {
		t.Fatalf("text block not collapsed: %q", s)
	}
	if gjson.GetBytes(out, "messages.1.content.0.content.1.source.data").String() != "AAAA" {
		t.Fatalf("the non-text block was corrupted: %s", out)
	}
}

type stubModel struct{ resp string }

func (m stubModel) Complete(context.Context, string) (string, error) { return m.resp, nil }

// TestSummarizeCountChangeLossless: summarize restructures [system,u1,tool,final]
// into [system, <summary>, final]; apply must keep the retained messages and all
// non-message fields byte-identical while the count drops.
func TestSummarizeCountChangeLossless(t *testing.T) {
	cfg := pipe(t, "pipeline: [summarize]\ncomponents:\n  summarize: {keep_last: 1, start_from_message: 0, min_tokens: 1}\n")
	p, _ := cfg.Build(nil)
	st := store.NewMemory(store.Options{})

	body, _ := json.Marshal(map[string]any{
		"model":       "gpt-x",
		"temperature": 0.3,
		"messages": []map[string]any{
			{"role": "system", "content": "you are helpful"},
			{"role": "user", "content": "do the task"},
			{"role": "tool", "tool_call_id": "a", "content": strings.Repeat("verbose tool output\n", 50)},
			{"role": "user", "content": "the final question"},
		},
	})

	out, changed := apply.BodyWithModel(context.Background(), p, st, bschemas.OpenAI, body, "", false,
		components.ModelSpec{Incoming: stubModel{resp: "essential facts"}})
	if !changed {
		t.Fatal("summarize should have restructured the transcript")
	}
	if n := gjson.GetBytes(out, "messages.#").Int(); n != 3 {
		t.Fatalf("expected 3 messages after summarize, got %d: %s", n, out)
	}
	// Non-message fields byte-identical.
	for _, path := range []string{"model", "temperature"} {
		if gjson.GetBytes(out, path).Raw != gjson.GetBytes(body, path).Raw {
			t.Fatalf("field %q not preserved", path)
		}
	}
	// Retained messages (system msg0, final user msg) byte-identical to originals.
	if gjson.GetBytes(out, "messages.0").Raw != gjson.GetBytes(body, "messages.0").Raw {
		t.Fatal("msg0 must be byte-identical")
	}
	if gjson.GetBytes(out, "messages.2").Raw != gjson.GetBytes(body, "messages.3").Raw {
		t.Fatalf("the final message must be preserved verbatim: %s", gjson.GetBytes(out, "messages.2").Raw)
	}
	// The inserted summary carries the marker for expand recovery.
	if s := gjson.GetBytes(out, "messages.1.content").String(); !strings.Contains(s, "History Summary") || !strings.Contains(s, "<<cg:") {
		t.Fatalf("summary message missing wrapper/marker: %q", s)
	}
}

func TestNoMessagesForwardsUnchanged(t *testing.T) {
	cfg := pipe(t, "pipeline: [dedup]\n")
	p, _ := cfg.Build(nil)
	body := []byte(`{"model":"x","prompt":"legacy completion"}`)
	out, changed := apply.Body(context.Background(), p, store.NewMemory(store.Options{}), bschemas.OpenAI, body, "", false)
	if changed || string(out) != string(body) {
		t.Fatalf("no messages array => forward unchanged; got changed=%v %s", changed, out)
	}
}

// TestCacheinjectReachesTheWire is the #32 regression: cacheinject's only possible
// targets on Claude Code traffic are assistant messages carrying `tool_use`, which
// bifrost cannot round-trip — so every mark used to be discarded by the writeback
// loop (46 applied, 0 forwarded, measured over 40 real requests). cache_control is
// metadata, so it is written onto the ORIGINAL raw bytes and the unmodellable
// provider fields must survive verbatim.
func TestCacheinjectReachesTheWire(t *testing.T) {
	cfg := pipe(t, "pipeline: [cacheinject]\n")
	p, _ := cfg.Build(nil)
	st := store.NewMemory(store.Options{})

	body := []byte(`{"model":"claude-x","messages":[
		{"role":"user","content":"run the tool"},
		{"role":"assistant","content":[{"type":"text","text":"on it"},{"type":"tool_use","id":"toolu_abc","name":"Bash","input":{"command":"ls -la"}}]}
	]}`)

	out, changed := apply.Body(context.Background(), p, st, bschemas.Anthropic, body, "", false)
	if !changed {
		t.Fatal("expected cacheinject's breakpoint to change the body")
	}
	cc := gjson.GetBytes(out, "messages.1.content.1.cache_control")
	if !cc.Exists() || cc.Get("type").String() != "ephemeral" {
		t.Fatalf("breakpoint never reached the wire: %s", out)
	}
	// Provider fields bifrost drops on unmarshal must be intact.
	blk := gjson.GetBytes(out, "messages.1.content.1")
	if blk.Get("id").String() != "toolu_abc" || blk.Get("name").String() != "Bash" ||
		blk.Get("input.command").String() != "ls -la" {
		t.Fatalf("tool_use provider fields corrupted: %s", blk.Raw)
	}
	if gjson.GetBytes(out, "messages.1.content.0.text").String() != "on it" {
		t.Fatalf("sibling text block corrupted: %s", out)
	}
	// Everything except the added cache_control is byte-identical.
	stripped, err := sjson.DeleteBytes(out, "messages.1.content.1.cache_control")
	if err != nil {
		t.Fatal(err)
	}
	if !jsonEq(t, stripped, body) {
		t.Fatalf("body changed beyond the metadata write:\n old=%s\n new=%s", body, stripped)
	}
}

func jsonEq(t *testing.T, a, b []byte) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(av, bv)
}

// TestWireBreakpointCapRealTrafficShape uses the exact shape 1,771 of 1,794
// captured Claude Code requests carry — system=2, tools=0, messages=1 — on a long
// conversation, and asserts the total the PROVIDER sees never exceeds 4. Before #32
// the component counted only the messages array, saw 1 existing breakpoint, and
// budgeted 3 more: 6 on the wire, which the provider rejects with a 400.
func TestWireBreakpointCapRealTrafficShape(t *testing.T) {
	cfg := pipe(t, "pipeline: [cacheinject]\n")
	p, _ := cfg.Build(nil)

	msgs := make([]any, 0, 60)
	for i := 0; i < 60; i++ {
		role, blk := "user", map[string]any{"type": "text", "text": strings.Repeat("turn ", i%7+1)}
		if i%2 == 1 {
			role = "assistant"
		}
		m := map[string]any{"role": role, "content": []any{blk}}
		if i == 59 { // the caller's own trailing breakpoint
			blk["cache_control"] = map[string]any{"type": "ephemeral"}
		}
		msgs = append(msgs, m)
	}
	body, _ := json.Marshal(map[string]any{
		"model": "claude-x",
		"system": []any{
			map[string]any{"type": "text", "text": "tools preamble", "cache_control": map[string]any{"type": "ephemeral"}},
			map[string]any{"type": "text", "text": "main system prompt", "cache_control": map[string]any{"type": "ephemeral"}},
		},
		"messages": msgs,
	})

	out, _ := apply.Body(context.Background(), p, store.NewMemory(store.Options{}), bschemas.Anthropic, body, "", false)
	if n := countWireBreakpoints(t, out); n > 4 {
		t.Fatalf("%d breakpoints on the wire — the provider caps at 4 and 400s above it", n)
	}
}

// countWireBreakpoints counts cache_control across system, tools and messages the
// way the provider does — deliberately re-derived in the test rather than reusing
// the implementation's counter, so a bug in that counter cannot hide the cap breach.
func countWireBreakpoints(t *testing.T, body []byte) int {
	t.Helper()
	var req struct {
		System []map[string]json.RawMessage `json:"system"`
		Tools  []map[string]json.RawMessage `json:"tools"`
		Msgs   []struct {
			CacheControl json.RawMessage              `json:"cache_control"`
			Content      []map[string]json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	n := 0
	count := func(blocks []map[string]json.RawMessage) {
		for _, b := range blocks {
			if _, ok := b["cache_control"]; ok {
				n++
			}
		}
	}
	count(req.System)
	count(req.Tools)
	for _, m := range req.Msgs {
		if len(m.CacheControl) > 0 {
			n++
		}
		count(m.Content)
	}
	return n
}

// contentRewriter is a test-only component that rewrites an assistant message's
// text — a CONTENT change on a message bifrost cannot round-trip, so the writeback
// layer must discard it. No shipped component targets assistant turns, so the
// discard path needs one to exercise it.
type contentRewriter struct{}

func (contentRewriter) Name() string                 { return "testrewrite" }
func (contentRewriter) Enabled(*components.Ctx) bool { return true }
func (contentRewriter) Reformat(req *bschemas.BifrostChatRequest, _ *components.Report, _ *components.Ctx) error {
	for i := range req.Input {
		if req.Input[i].Role != bschemas.ChatMessageRoleAssistant || req.Input[i].Content == nil {
			continue
		}
		for b := range req.Input[i].Content.ContentBlocks {
			if t := req.Input[i].Content.ContentBlocks[b].Text; t != nil {
				short := "shortened"
				req.Input[i].Content.ContentBlocks[b].Text = &short
			}
		}
	}
	return nil
}

func init() {
	components.Register("testrewrite", func([]byte) (components.Component, error) {
		return contentRewriter{}, nil
	})
}

// TestDiscardedChangeIsCounted: a change the writeback layer throws away must be
// attributed to the component that made it, not silently vanish. A component that
// mutates and is then discarded used to look exactly like one that works — which is
// how #32 survived two benchmark studies.
func TestDiscardedChangeIsCounted(t *testing.T) {
	cfg := pipe(t, "pipeline: [testrewrite]\n")
	agg := metrics.NewAggregator()
	p, _ := cfg.Build(agg)

	body := []byte(`{"model":"claude-x","messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","content":[{"type":"text","text":"a long narration that the component will rewrite"},{"type":"tool_use","id":"toolu_z","name":"Bash","input":{"command":"ls"}}]}
	]}`)

	out, _ := apply.Body(context.Background(), p, store.NewMemory(store.Options{}), bschemas.Anthropic, body, "", false)
	if gjson.GetBytes(out, "messages.1").Raw != gjson.GetBytes(body, "messages.1").Raw {
		t.Fatalf("the unmodellable message must be kept verbatim:\n old=%s\n new=%s",
			gjson.GetBytes(body, "messages.1").Raw, gjson.GetBytes(out, "messages.1").Raw)
	}
	snap := agg.Snapshot()
	if got := snap.Components["testrewrite"].Discarded; got == 0 {
		t.Fatalf("discarded change not counted: %+v", snap.Components["testrewrite"])
	}
	if len(snap.TopDiscarded) == 0 || snap.TopDiscarded[0] != "testrewrite" {
		t.Fatalf("top_discarded should name the component, got %v", snap.TopDiscarded)
	}
	// A Discarded report must not inflate Runs (it is attribution, not a second run).
	if r := snap.Components["testrewrite"].Runs; r != 1 {
		t.Fatalf("Runs should stay 1, got %d", r)
	}
}

// reverter always errors, so the pipeline rolls its change back. A reverted component
// must NOT be charged a discard: its change never reached the writeback layer at all.
type reverter struct{}

func (reverter) Name() string                 { return "testrevert" }
func (reverter) Enabled(*components.Ctx) bool { return true }
func (reverter) Reformat(req *bschemas.BifrostChatRequest, _ *components.Report, _ *components.Ctx) error {
	for i := range req.Input {
		if req.Input[i].Role == bschemas.ChatMessageRoleAssistant && req.Input[i].Content != nil {
			for b := range req.Input[i].Content.ContentBlocks {
				if req.Input[i].Content.ContentBlocks[b].Text != nil {
					s := "mutated then reverted"
					req.Input[i].Content.ContentBlocks[b].Text = &s
				}
			}
		}
	}
	return errors.New("deliberate failure so the pipeline reverts")
}

func init() {
	components.Register("testrevert", func([]byte) (components.Component, error) { return reverter{}, nil })
}

// A rolled-back component must not appear as a discard. Otherwise the counter meant to
// catch #32-class bugs becomes a false-positive generator.
func TestRevertedComponentNotChargedDiscard(t *testing.T) {
	// testrewrite runs AFTER and produces the discard, so the writeback layer really does
	// throw a change away at that index. Pre-fix, testrevert's ChangedIdx was recorded
	// before the rollback, so it got charged for a discard caused by the other component.
	cfg := pipe(t, "pipeline: [testrewrite, testrevert]\n")
	agg := metrics.NewAggregator()
	p, _ := cfg.Build(agg)

	body := []byte(`{"model":"claude-x","messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","content":[{"type":"text","text":"narration"},{"type":"tool_use","id":"toolu_r","name":"Bash","input":{}}]}
	]}`)
	apply.Body(context.Background(), p, store.NewMemory(store.Options{}), bschemas.Anthropic, body, "", false)

	cs := agg.Snapshot().Components["testrevert"]
	if cs.Reverted != 1 {
		t.Fatalf("expected the component to be reverted, got %+v", cs)
	}
	if cs.Discarded != 0 {
		t.Fatalf("reverted component charged %d discards — its change never reached writeback", cs.Discarded)
	}
}

// One discarded message is charged to exactly ONE component, not to every component
// that touched it. Two components rewrite the same unmodellable message; only the last
// one's change is what the writeback layer actually threw away.
func TestDiscardChargedOnceNotPerToucher(t *testing.T) {
	cfg := pipe(t, "pipeline: [testrewrite, testrewrite2]\n")
	agg := metrics.NewAggregator()
	p, _ := cfg.Build(agg)

	body := []byte(`{"model":"claude-x","messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","content":[{"type":"text","text":"a long narration both components rewrite"},{"type":"tool_use","id":"toolu_d","name":"Bash","input":{}}]}
	]}`)
	apply.Body(context.Background(), p, store.NewMemory(store.Options{}), bschemas.Anthropic, body, "", false)

	snap := agg.Snapshot()
	total := snap.Components["testrewrite"].Discarded + snap.Components["testrewrite2"].Discarded
	if total != 1 {
		t.Fatalf("one discarded message charged %d times (testrewrite=%d testrewrite2=%d)",
			total, snap.Components["testrewrite"].Discarded, snap.Components["testrewrite2"].Discarded)
	}
	// It must land on the LAST toucher — its change is the discarded state.
	if snap.Components["testrewrite2"].Discarded != 1 {
		t.Fatalf("discard should be charged to the last component to change the message, got %v", snap.TopDiscarded)
	}
}

// contentRewriter2 is a second rewriter so two components touch one message.
type contentRewriter2 struct{}

func (contentRewriter2) Name() string                 { return "testrewrite2" }
func (contentRewriter2) Enabled(*components.Ctx) bool { return true }
func (contentRewriter2) Reformat(req *bschemas.BifrostChatRequest, _ *components.Report, _ *components.Ctx) error {
	for i := range req.Input {
		if req.Input[i].Role != bschemas.ChatMessageRoleAssistant || req.Input[i].Content == nil {
			continue
		}
		for b := range req.Input[i].Content.ContentBlocks {
			if req.Input[i].Content.ContentBlocks[b].Text != nil {
				s := "even shorter"
				req.Input[i].Content.ContentBlocks[b].Text = &s
			}
		}
	}
	return nil
}

func init() {
	components.Register("testrewrite2", func([]byte) (components.Component, error) {
		return contentRewriter2{}, nil
	})
}
