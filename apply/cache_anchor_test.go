package apply_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
)

// anchorBody is a Claude Code-shaped Anthropic request: a tool exchange, a text turn, a SECOND tool
// exchange, two text turns, and Claude Code's trailing system reminder with its breakpoint.
// systemMarks sets how many system blocks carry a breakpoint (Claude Code uses 2).
func anchorBody(t *testing.T, withTools bool, systemMarks int) []byte {
	t.Helper()
	cc := map[string]any{"type": "ephemeral"}
	sys := []any{}
	for i := 0; i < 3; i++ {
		blk := map[string]any{"type": "text", "text": "system block " + string(rune('a'+i)) + strings.Repeat(" padding", 40)}
		if i >= 3-systemMarks {
			blk["cache_control"] = cc
		}
		sys = append(sys, blk)
	}
	long := strings.Repeat("row of tool output with some words in it\n", 60)
	msgs := []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "TASK: read the files"}}}}
	if withTools {
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Read", "input": map[string]any{"p": "a"}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "FIRST OUTPUT " + long}}},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "read a"}}},
			map[string]any{"role": "user", "content": "now read b"},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "toolu_2", "name": "Read", "input": map[string]any{"p": "b"}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_2", "content": "SECOND OUTPUT " + long}}},
		)
	} else {
		for i := 0; i < 3; i++ {
			msgs = append(msgs,
				map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "answer " + long}}},
				map[string]any{"role": "user", "content": "go on"})
		}
	}
	msgs = append(msgs,
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "read b"}}},
		map[string]any{"role": "user", "content": "reply with one word"},
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "ten"}}},
		map[string]any{"role": "user", "content": "and again"},
		map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": "<system-reminder>r</system-reminder>", "cache_control": cc}}},
	)
	b, err := json.Marshal(map[string]any{"model": "aws/claude-sonnet-5", "max_tokens": 64, "system": sys, "messages": msgs})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// spliceTwice runs a cache_aware_summarizer pipeline over body twice: the first pass commissions the
// summary, the second splices it, which is the count-changing rebuild under test.
func spliceTwice(t *testing.T, body []byte) []byte {
	t.Helper()
	cfg := pipe(t, "pipeline: [cache_aware_summarizer]\ncomponents:\n  cache_aware_summarizer:\n"+
		"    keep_last_turns: 4\n    min_tokens: 1\n"+
		"    trigger:\n      min_messages: 1\n      min_request_tokens: 1\n      min_request_frac: 0\n")
	p, _ := cfg.Build(nil)
	st := store.NewMemory(store.Options{})
	model := &recordingModel{reply: "<summary>SUMMARY-MARKER of the early turns</summary>"}
	apply.BodyWithModel(context.Background(), p, st, bschemas.Anthropic, body, "", false, components.ModelSpec{Incoming: model})
	offload.WaitForAllSummariesForTest(5 * time.Second)
	out, changed := apply.BodyWithModel(context.Background(), p, st, bschemas.Anthropic, body, "", false, components.ModelSpec{Incoming: model})
	if !changed || !strings.Contains(string(out), "SUMMARY-MARKER") {
		t.Fatalf("the summarizer never spliced, so the rebuild under test never ran:\n%.400s", out)
	}
	return out
}

// toolResultMark reports whether the tool_result for id is present, and whether it carries a breakpoint.
func toolResultMark(out []byte, id string) (present, marked bool) {
	gjson.GetBytes(out, "messages").ForEach(func(_, m gjson.Result) bool {
		m.Get("content").ForEach(func(_, b gjson.Result) bool {
			if b.Get("type").String() == "tool_result" && b.Get("tool_use_id").String() == id {
				present = true
				marked = b.Get("cache_control").IsObject()
			}
			return true
		})
		return true
	})
	return
}

func anchorWireMarks(out []byte) int { return strings.Count(string(out), `"cache_control"`) }

// #723 (a) and (b). After a splice, the LAST tool exchange stays verbatim and its tool_result carries
// a breakpoint, so the next turn has a cache entry it can read. Without either, the kept history is
// text turns only and every following turn re-writes it.
func TestASpliceKeepsAndMarksTheLastToolExchange(t *testing.T) {
	written, _, _ := apply.CacheAnchorStats()
	out := spliceTwice(t, anchorBody(t, true, 2))
	present, marked := toolResultMark(out, "toolu_2")
	if !present {
		t.Fatalf("(a) the last tool exchange was summarized away; it must stay verbatim after the splice:\n%.600s", out)
	}
	if p1, _ := toolResultMark(out, "toolu_1"); p1 {
		t.Error("the earlier tool exchange should be inside the summary")
	}
	if !marked {
		t.Fatal("(b) the kept tool_result carries no cache_control, so the next turn has nothing to read")
	}
	if n := anchorWireMarks(out); n > 4 {
		t.Fatalf("%d breakpoints on the wire, over the provider cap of 4", n)
	}
	if w, _, _ := apply.CacheAnchorStats(); w != written+1 {
		t.Errorf("cache anchors written rose by %d, want 1", w-written)
	}
}

// #723, the budget guard. With 4 breakpoints already on the wire (3 system + Claude Code's message
// breakpoint), the anchor must be skipped and counted, never a 5th breakpoint (a provider 400).
func TestTheCacheAnchorIsSkippedWhenNoBreakpointSlotIsFree(t *testing.T) {
	_, noSlot, _ := apply.CacheAnchorStats()
	out := spliceTwice(t, anchorBody(t, true, 3))
	if _, marked := toolResultMark(out, "toolu_2"); marked {
		t.Fatal("the anchor was written with no free slot")
	}
	if n := anchorWireMarks(out); n > 4 {
		t.Fatalf("%d breakpoints on the wire, over the provider cap of 4", n)
	}
	if _, ns, _ := apply.CacheAnchorStats(); ns != noSlot+1 {
		t.Errorf("anchors skipped for want of a slot rose by %d, want 1", ns-noSlot)
	}
}

// #723. A transcript with no tool exchange gets no anchor and no other change.
func TestASpliceWithNoToolExchangeAddsNoAnchor(t *testing.T) {
	written, noSlot, notFound := apply.CacheAnchorStats()
	out := spliceTwice(t, anchorBody(t, false, 2))
	if n := anchorWireMarks(out); n != 3 {
		t.Fatalf("%d breakpoints on the wire, want Claude Code's own 3 and no anchor", n)
	}
	if w, ns, nf := apply.CacheAnchorStats(); w != written || ns != noSlot || nf != notFound {
		t.Errorf("anchor counters moved with no tool exchange: written %d->%d, noSlot %d->%d, notFound %d->%d",
			written, w, noSlot, ns, notFound, nf)
	}
}

// #727 review, finding 1. A one-message span: the splice replaces one text message with the summary,
// so the message count does not change. The rebuild (and the anchor) must still run; it used to be
// skipped because the equal-count check knew only summarize's header, not cache_aware_summarizer's.
func TestAnEqualCountSpliceStillWritesTheAnchor(t *testing.T) {
	cc := map[string]any{"type": "ephemeral"}
	long := strings.Repeat("row of tool output with some words in it\n", 60)
	body, _ := json.Marshal(map[string]any{
		"model": "aws/claude-sonnet-5", "max_tokens": 64,
		"system": []any{map[string]any{"type": "text", "text": "sys" + strings.Repeat(" padding", 40), "cache_control": cc}},
		"messages": []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "TASK: read the file"}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "ONLY SPAN MESSAGE " + long}}},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "toolu_eq", "name": "Read", "input": map[string]any{"p": "a"}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_eq", "content": "KEPT OUTPUT " + long}}},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "read it"}}},
			map[string]any{"role": "user", "content": "one word"},
			map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": "<system-reminder>r</system-reminder>", "cache_control": cc}}},
		},
	})
	cfg := pipe(t, "pipeline: [cache_aware_summarizer]\ncomponents:\n  cache_aware_summarizer:\n"+
		"    keep_last_turns: 4\n    min_tokens: 1\n"+
		"    trigger:\n      min_messages: 1\n      min_request_tokens: 1\n      min_request_frac: 0\n")
	p, _ := cfg.Build(nil)
	st := store.NewMemory(store.Options{})
	model := &recordingModel{reply: "<summary>SUMMARY-MARKER of one message</summary>"}
	apply.BodyWithModel(context.Background(), p, st, bschemas.Anthropic, body, "", false, components.ModelSpec{Incoming: model})
	offload.WaitForAllSummariesForTest(5 * time.Second)
	out, _ := apply.BodyWithModel(context.Background(), p, st, bschemas.Anthropic, body, "", false, components.ModelSpec{Incoming: model})
	if !strings.Contains(string(out), "SUMMARY-MARKER") {
		t.Fatalf("the summarizer never spliced:\n%.400s", out)
	}
	if strings.Contains(string(out), "ONLY SPAN MESSAGE") {
		t.Fatal("the one-message span was not replaced")
	}
	if got, want := gjson.GetBytes(out, "messages.#").Int(), gjson.GetBytes(body, "messages.#").Int(); got != want {
		t.Fatalf("the splice changed the message count (%d -> %d); this test needs an equal-count splice", want, got)
	}
	if _, marked := toolResultMark(out, "toolu_eq"); !marked {
		t.Fatal("an equal-count splice wrote no anchor on the kept tool_result")
	}
}

// #727 review (both reviewers). The anchor is Anthropic-shaped (tool_result blocks); on an
// OpenAI-shaped body a splice must not count it as "not found".
func TestAnOpenAISpliceDoesNotCountAMissingAnchor(t *testing.T) {
	_, _, notFound := apply.CacheAnchorStats()
	long := strings.Repeat("verbose tool output\n", 60)
	body, _ := json.Marshal(map[string]any{
		"model": "gpt-x",
		"messages": []map[string]any{
			{"role": "system", "content": "you are helpful"},
			{"role": "user", "content": "do the task " + long},
			{"role": "assistant", "content": "thinking " + long},
			{"role": "user", "content": "go on " + long},
			{"role": "assistant", "content": "", "tool_calls": []map[string]any{{"id": "c1", "type": "function", "function": map[string]any{"name": "read", "arguments": "{}"}}}},
			{"role": "tool", "tool_call_id": "c1", "content": long},
			{"role": "assistant", "content": "done"},
			{"role": "user", "content": "the final question"},
		},
	})
	cfg := pipe(t, "pipeline: [cache_aware_summarizer]\ncomponents:\n  cache_aware_summarizer:\n"+
		"    keep_last_turns: 2\n    min_tokens: 1\n"+
		"    trigger:\n      min_messages: 1\n      min_request_tokens: 1\n      min_request_frac: 0\n")
	p, _ := cfg.Build(nil)
	st := store.NewMemory(store.Options{})
	model := &recordingModel{reply: "<summary>SUMMARY-MARKER openai</summary>"}
	apply.BodyWithModel(context.Background(), p, st, bschemas.OpenAI, body, "", false, components.ModelSpec{Incoming: model})
	offload.WaitForAllSummariesForTest(5 * time.Second)
	out, _ := apply.BodyWithModel(context.Background(), p, st, bschemas.OpenAI, body, "", false, components.ModelSpec{Incoming: model})
	if !strings.Contains(string(out), "SUMMARY-MARKER") {
		t.Fatalf("the summarizer never spliced:\n%.400s", out)
	}
	if _, _, nf := apply.CacheAnchorStats(); nf != notFound {
		t.Fatalf("an OpenAI-shaped splice counted %d missing anchor(s)", nf-notFound)
	}
}

// inPlaceRewriter stands in for the sweep's cold apply (#729): it rewrites one tool output IN PLACE (no
// message is added or removed) and, when name is set, names an anchor tool_result.
type inPlaceRewriter struct {
	shrink string // tool_use id whose output is replaced
	anchor string // CacheAnchorToolID to set; "" sets none
}

func (inPlaceRewriter) Name() string                 { return "inplace" }
func (inPlaceRewriter) Enabled(*components.Ctx) bool { return true }
func (r inPlaceRewriter) Offload(req *bschemas.BifrostChatRequest, _ *components.Report, c *components.Ctx) ([]string, error) {
	c.CacheAnchorToolID = r.anchor
	if r.shrink == "" {
		return nil, nil
	}
	for i := range req.Input {
		m := &req.Input[i]
		if m.Role == bschemas.ChatMessageRoleTool && m.ChatToolMessage != nil && m.ChatToolMessage.ToolCallID != nil &&
			*m.ChatToolMessage.ToolCallID == r.shrink {
			s := "<<cg:abc123>> dropped"
			m.Content = &bschemas.ChatMessageContent{ContentStr: &s}
			return []string{"abc123"}, nil
		}
	}
	return nil, nil
}

func runInPlace(t *testing.T, r inPlaceRewriter, body []byte) []byte {
	t.Helper()
	p := components.NewPipeline([]components.Component{r}, nil)
	return apply.BodyOpts(context.Background(), p, store.NewMemory(store.Options{}), apply.Opts{
		Provider: bschemas.Anthropic, Body: body, Session: "inplace-729",
	}).Body
}

// #729. A component that rewrites in place (the sweep's cold apply) and names an anchor gets the
// breakpoint on that tool_result, through the same writer as the splice's.
func TestAnInPlaceRewriteWritesTheNamedAnchor(t *testing.T) {
	written, _, _ := apply.CacheAnchorStats()
	out := runInPlace(t, inPlaceRewriter{shrink: "toolu_1", anchor: "toolu_2"}, anchorBody(t, true, 2))
	if !strings.Contains(string(out), "dropped") {
		t.Fatalf("fixture: the in-place rewrite did not reach the body:\n%.400s", out)
	}
	if _, marked := toolResultMark(out, "toolu_2"); !marked {
		t.Fatal("the named tool_result carries no cache_control after an in-place rewrite")
	}
	if n := anchorWireMarks(out); n != 4 {
		t.Fatalf("%d breakpoints on the wire, want Claude Code's 3 plus the anchor", n)
	}
	if w, _, _ := apply.CacheAnchorStats(); w != written+1 {
		t.Errorf("cache anchors written rose by %d, want 1", w-written)
	}
}

// #729, the budget guard: no free slot, no anchor, counted.
func TestAnInPlaceAnchorIsSkippedWhenNoSlotIsFree(t *testing.T) {
	_, noSlot, _ := apply.CacheAnchorStats()
	out := runInPlace(t, inPlaceRewriter{shrink: "toolu_1", anchor: "toolu_2"}, anchorBody(t, true, 3))
	if _, marked := toolResultMark(out, "toolu_2"); marked {
		t.Fatal("the anchor was written with no free slot")
	}
	if n := anchorWireMarks(out); n > 4 {
		t.Fatalf("%d breakpoints on the wire, over the provider cap of 4", n)
	}
	if _, ns, _ := apply.CacheAnchorStats(); ns != noSlot+1 {
		t.Errorf("anchors skipped for want of a slot rose by %d, want 1", ns-noSlot)
	}
}

// #729. A request the component did not change gets no anchor, even if one is named.
func TestAnUnchangedRequestGetsNoInPlaceAnchor(t *testing.T) {
	written, noSlot, notFound := apply.CacheAnchorStats()
	body := anchorBody(t, true, 2)
	out := runInPlace(t, inPlaceRewriter{anchor: "toolu_2"}, body)
	if _, marked := toolResultMark(out, "toolu_2"); marked {
		t.Fatal("an anchor was written on a request nothing changed")
	}
	if w, ns, nf := apply.CacheAnchorStats(); w != written || ns != noSlot || nf != notFound {
		t.Errorf("anchor counters moved on an unchanged request")
	}
}

// #729. Two turns of one conversation: the anchor stays on the same block and the bytes up to it do not
// move, so the provider's entry stays readable.
func TestTheInPlaceAnchorStaysOnTheSameBlockAsTheConversationGrows(t *testing.T) {
	r := inPlaceRewriter{shrink: "toolu_1", anchor: "toolu_2"}
	first := runInPlace(t, r, anchorBody(t, true, 2))
	// The second turn has one more exchange after the anchor.
	var m map[string]any
	_ = json.Unmarshal(anchorBody(t, true, 2), &m)
	msgs := m["messages"].([]any)
	grown := append(append([]any{}, msgs[:len(msgs)-1]...),
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "more"}}},
		map[string]any{"role": "user", "content": "and more"},
		msgs[len(msgs)-1])
	m["messages"] = grown
	gb, _ := json.Marshal(m)
	second := runInPlace(t, r, gb)
	idx := func(out []byte) int {
		for i, mm := range gjson.GetBytes(out, "messages").Array() {
			for _, b := range mm.Get("content").Array() {
				if b.Get("tool_use_id").String() == "toolu_2" && b.Get("cache_control").IsObject() {
					return i
				}
			}
		}
		return -1
	}
	if idx(first) < 0 || idx(first) != idx(second) {
		t.Fatalf("the anchor moved or vanished: turn 1 at message %d, turn 2 at message %d", idx(first), idx(second))
	}
	// Bytes up to and including the anchor are identical across the two turns.
	a := gjson.GetBytes(first, "messages").Array()
	b := gjson.GetBytes(second, "messages").Array()
	for i := 0; i <= idx(first); i++ {
		if a[i].Raw != b[i].Raw {
			t.Fatalf("message %d differs between turns, so the cached bytes are not stable", i)
		}
	}
}
