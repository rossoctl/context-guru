package apply_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
)

// TestRebuildKeepsAMessageNormalizeCouldNotParse: the count-changed rebuild emits ONLY
// slot-mapped messages, so a body message normalize skipped (it does not unmarshal into
// bifrost's ChatMessage) was DELETED from the forwarded request — an altered request,
// which is the wrong direction for fail-open. The rebuild must decline instead.
func TestRebuildKeepsAMessageNormalizeCouldNotParse(t *testing.T) {
	cfg := pipe(t, "pipeline: [summarize]\ncomponents:\n  summarize: {keep_last: 1, start_from_message: 0, min_tokens: 1}\n")
	p, _ := cfg.Build(nil)

	// Five body messages, four of them normalizable: summarize collapses them to three,
	// so the writeback goes down the count-changed REBUILD path.
	dump := strings.Repeat("verbose tool output ", 50)
	body := []byte(`{"model":"gpt-x","messages":[` +
		`{"role":"system","content":"you are helpful"},` +
		`{"role":123,"content":"a message bifrost cannot unmarshal"},` +
		`{"role":"tool","tool_call_id":"a","content":"` + dump + `"},` +
		`{"role":"tool","tool_call_id":"b","content":"` + dump + `"},` +
		`{"role":"user","content":"the final question"}]}`)

	out, _ := apply.BodyWithModel(context.Background(), p, store.NewMemory(store.Options{}),
		bschemas.OpenAI, body, "", false,
		components.ModelSpec{Incoming: stubModel{resp: "essential facts"}})

	if !strings.Contains(string(out), "cannot unmarshal") {
		t.Fatalf("the unparseable message was deleted from the forwarded request: %s", out)
	}
	if string(out) != string(body) {
		t.Fatalf("the rebuild must decline and forward the original:\n want=%s\n got =%s", body, out)
	}
}

// reorderDropper is a test-only count-CHANGING component: it drops one message and
// reorders the rest, which is what makes two synthetic tool messages sharing one body
// message non-contiguous in the output.
type reorderDropper struct{}

func (reorderDropper) Name() string                 { return "testreorder" }
func (reorderDropper) Enabled(*components.Ctx) bool { return true }
func (reorderDropper) Reformat(req *bschemas.BifrostChatRequest, _ *components.Report, _ *components.Ctx) error {
	if len(req.Input) < 4 {
		return nil
	}
	req.Input = []bschemas.ChatMessage{req.Input[1], req.Input[3], req.Input[2]}
	return nil
}

func init() {
	components.Register("testreorder", func([]byte) (components.Component, error) { return reorderDropper{}, nil })
}

// TestRebuildDoesNotDuplicateABodyMessage: an Anthropic user message with several
// tool_result blocks yields several synthetic messages sharing ONE body index. The
// rebuild de-duplicated them only when adjacent, so a count-changing component that
// leaves them non-contiguous emitted that raw message twice — duplicate tool_use_id,
// which Anthropic rejects with a 400.
func TestRebuildDoesNotDuplicateABodyMessage(t *testing.T) {
	cfg := pipe(t, "pipeline: [testreorder]\n")
	p, _ := cfg.Build(nil)

	body := []byte(`{"model":"claude-x","messages":[` +
		`{"role":"user","content":"go"},` +
		`{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"tu_1","content":"first output"},` +
		`{"type":"tool_result","tool_use_id":"tu_2","content":"second output"}]},` +
		`{"role":"user","content":"later"}]}`)

	out, _ := apply.Body(context.Background(), p, store.NewMemory(store.Options{}),
		bschemas.Anthropic, body, "", false)

	if n := strings.Count(string(out), "tu_1"); n != 1 {
		t.Fatalf("body message emitted %d times (duplicate tool_use_id): %s", n, out)
	}
	if n := gjson.GetBytes(out, "messages.#").Int(); n != 2 {
		t.Fatalf("expected 2 messages after the reorder, got %d: %s", n, out)
	}
}

// TestSpliceRewritesEveryMessageAtItsOwnSpan: the writeback edits each changed message's
// own bytes and splices them all back in ONE pass, using the byte offsets gjson already
// produced for the PRE-split body plus the shift the volatile-tail split reports.
//
// That shift is the sharp edge. splitVolatileTail rewrites the top-level `system` value
// before the writeback runs, which moves every following byte — so a request that BOTH
// splits and rewrites tool outputs is the case where a wrong offset shows up. The splice
// verifies each span against the body's own bytes and declines if they disagree, so a
// wrong shift means NO rewrite reaches the wire at all (fail-open, never corruption) and
// this test fails on the assertions below.
//
// Several rewrites in one request also exercise the accumulation: the old writeback ran
// one sjson.SetBytes over the WHOLE body per changed message.
func TestSpliceRewritesEveryMessageAtItsOwnSpan(t *testing.T) {
	cfg := pipe(t, "pipeline: [cachesplit, dedup]\n")
	p, _ := cfg.Build(nil)

	// A system block big enough, with a volatile git tail, so cachesplit's split fires and
	// shifts every message offset that follows it.
	stable := strings.Repeat("stable instruction line.\n", 400)
	sys := []any{map[string]any{
		"type": "text", "text": stable + "\nRecent commits:\ndeadbeef fix a thing\n",
		"cache_control": map[string]any{"type": "ephemeral"},
	}}

	// Four identical tool outputs: dedup rewrites the three repeats, so one request
	// carries several edits across several body messages.
	big := strings.Repeat("a verbose repeated tool output line\n", 60)
	msgs := []any{map[string]any{"role": "user", "content": "go"}}
	for i := 0; i < 4; i++ {
		msgs = append(msgs, map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "tu_" + strconv.Itoa(i), "content": big},
		}})
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": "the final question"})

	// Key ORDER is the point: `system` must precede `messages` so the split's rewrite of
	// the system value moves every message offset. json.Marshal of a map sorts keys
	// ("messages" < "model" < "system") and would put messages FIRST, where the split
	// shifts nothing and this test would pass with the shift hard-coded to zero. Real
	// Claude Code bodies happen to order it that way too, which is exactly why the
	// captured-traffic goldens cannot cover this path.
	sysEnc, err := json.Marshal(sys)
	if err != nil {
		t.Fatal(err)
	}
	msgsEnc, err := json.Marshal(msgs)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"claude-x","system":` + string(sysEnc) +
		`,"messages":` + string(msgsEnc) + `}`)

	out, changed := apply.Body(context.Background(), p, store.NewMemory(store.Options{}),
		bschemas.Anthropic, body, "", false)
	if !changed {
		t.Fatal("expected the split and the dedup rewrites to be forwarded")
	}
	if !gjson.ValidBytes(out) {
		t.Fatalf("the splice produced invalid JSON: %s", firstBytes(out))
	}
	// The split happened, i.e. the offsets the splice used really were shifted.
	if n := len(gjson.GetBytes(out, "system").Array()); n != 2 {
		t.Fatalf("volatile-tail split did not fire (%d system blocks); this test needs it "+
			"to exercise the shifted offsets", n)
	}
	// Every repeat was rewritten at its OWN block, and each kept its own tool_use_id —
	// a misplaced splice would land a marker on the wrong message or corrupt the id.
	for i := 1; i <= 4; i++ {
		got := gjson.GetBytes(out, "messages."+strconv.Itoa(i)+".content.0.content").String()
		id := gjson.GetBytes(out, "messages."+strconv.Itoa(i)+".content.0.tool_use_id").String()
		if id != "tu_"+strconv.Itoa(i-1) {
			t.Errorf("messages.%d: tool_use_id=%q, want tu_%d", i, id, i-1)
		}
		if i == 1 {
			if got != big { // the first occurrence is the one dedup keeps
				t.Errorf("messages.1: first occurrence must stay verbatim, got %d bytes", len(got))
			}
			continue
		}
		if !strings.Contains(got, "<<cg:") {
			t.Errorf("messages.%d: repeat was not rewritten: %q", i, firstBytes([]byte(got)))
		}
	}
	// Untouched messages keep their original bytes (invariant I1) — the splice copies
	// everything outside an edited span verbatim.
	for _, path := range []string{"messages.0", "messages.5", "model"} {
		if a, b := gjson.GetBytes(out, path).Raw, gjson.GetBytes(body, path).Raw; a != b {
			t.Errorf("%s changed: old=%s new=%s", path, b, a)
		}
	}
}

func firstBytes(b []byte) string {
	if len(b) > 200 {
		return string(b[:200]) + "..."
	}
	return string(b)
}

// TestArrayShapedToolResultIsCompacted: the Anthropic Messages API permits a
// tool_result whose `content` is an ARRAY of content blocks, and many clients emit
// that. Those blocks used to be skipped, so the whole message fell to the
// whole-message slot bifrost cannot model and 100% of the request's tool output was
// silently uncompactable. Non-text blocks (here an image) must still survive verbatim.
func TestArrayShapedToolResultIsCompacted(t *testing.T) {
	cfg := pipe(t, "pipeline: [dedup]\n")
	p, _ := cfg.Build(nil)

	big := strings.Repeat("a verbose repeated tool output line\n", 60)
	toolResult := func(id string, extra ...map[string]any) map[string]any {
		content := []any{map[string]any{"type": "text", "text": big}}
		for _, e := range extra {
			content = append(content, e)
		}
		return map[string]any{"type": "tool_result", "tool_use_id": id, "content": content}
	}
	image := map[string]any{"type": "image", "source": map[string]any{
		"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgo="}}
	body, _ := json.Marshal(map[string]any{
		"model": "claude-x",
		"messages": []any{
			map[string]any{"role": "user", "content": "go"},
			map[string]any{"role": "user", "content": []any{toolResult("tu_1")}},
			map[string]any{"role": "user", "content": []any{toolResult("tu_2", image)}},
		},
	})

	out, changed := apply.Body(context.Background(), p, store.NewMemory(store.Options{}),
		bschemas.Anthropic, body, "", false)
	if !changed {
		t.Fatalf("array-shaped tool_result content was invisible to the pipeline: %s", out)
	}
	// The duplicate's text was rewritten IN PLACE, at the text block's own path.
	got := gjson.GetBytes(out, "messages.2.content.0.content.0.text").String()
	if got == big || !strings.Contains(got, "<<cg:") {
		t.Fatalf("the duplicate tool output should carry a marker, got %q", got)
	}
	// The non-text sibling block is untouched.
	if a, b := gjson.GetBytes(out, "messages.2.content.0.content.1").Raw,
		gjson.GetBytes(body, "messages.2.content.0.content.1").Raw; a != b {
		t.Fatalf("the image block must survive verbatim:\n old=%s\n new=%s", b, a)
	}
	// The first occurrence keeps its full text (only the repeat is collapsed).
	if s := gjson.GetBytes(out, "messages.1.content.0.content.0.text").String(); s != big {
		t.Fatalf("the first occurrence must stay verbatim, got %d bytes", len(s))
	}
}
