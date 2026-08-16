package apply_test

import (
	"context"
	"encoding/json"
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
