package all_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// stubModel is a fixed LLM used to drive the model-based components in tests.
type stubModel struct {
	resp string
	err  error
}

func (m stubModel) Complete(context.Context, string) (string, error) { return m.resp, m.err }

var errBoom = errors.New("boom")

func strp(s string) *string { return &s }

func newComp(t *testing.T, name, yaml string) components.Offload {
	t.Helper()
	c, err := components.New(name, []byte(yaml))
	if err != nil {
		t.Fatalf("New(%s): %v", name, err)
	}
	off, ok := c.(components.Offload)
	if !ok {
		t.Fatalf("%s is not an Offload", name)
	}
	return off
}

// TestSummarizeRestructures: [system,u1,tool,u2] with keep_last=1 becomes
// [system, <summary>, u2]; the summary carries a marker and the replaced span is
// stashed for expand.
func TestSummarizeRestructures(t *testing.T) {
	off := newComp(t, "summarize", "keep_last: 1\nstart_from_message: 0\nmin_tokens: 1\n")
	st := store.NewMemory(store.Options{})
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		{Role: bschemas.ChatMessageRoleSystem, Content: &bschemas.ChatMessageContent{ContentStr: strp("you are helpful")}},
		{Role: bschemas.ChatMessageRoleUser, Content: &bschemas.ChatMessageContent{ContentStr: strp("do the task")}},
		toolMsg(strings.Repeat("verbose tool output line\n", 40)),
		{Role: bschemas.ChatMessageRoleUser, Content: &bschemas.ChatMessageContent{ContentStr: strp("continue")}},
	}}
	c := &components.Ctx{Ctx: context.Background(), Store: st, Model: components.ModelSpec{Incoming: stubModel{resp: "essential facts"}}}
	var rep components.Report
	keys, err := off.Offload(req, &rep, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected 1 stashed span, got %d (skipped=%v)", len(keys), rep.Skipped)
	}
	if len(req.Input) != 3 {
		t.Fatalf("expected [system, summary, u2], got %d messages", len(req.Input))
	}
	if req.Input[0].Role != bschemas.ChatMessageRoleSystem || schema.MessageText(req.Input[0]) != "you are helpful" {
		t.Fatal("msg0 must be preserved")
	}
	sm := schema.MessageText(req.Input[1])
	if !strings.Contains(sm, "History Summary") || !strings.Contains(sm, "<<cg:") || !strings.Contains(sm, "essential facts") {
		t.Fatalf("summary message missing wrapper/marker/summary: %q", sm)
	}
	if schema.MessageText(req.Input[2]) != "continue" {
		t.Fatal("last message must be kept verbatim")
	}
	if _, ok := st.Get(keys[0]); !ok {
		t.Fatal("replaced span must be stashed for expand recovery")
	}
}

// TestSummarizeNoModelSkips: NeedsModel with no model available must no-op.
func TestSummarizeNoModelSkips(t *testing.T) {
	off := newComp(t, "summarize", "keep_last: 1\nstart_from_message: 0\nmin_tokens: 1\n")
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		{Role: bschemas.ChatMessageRoleSystem, Content: &bschemas.ChatMessageContent{ContentStr: strp("s")}},
		toolMsg(strings.Repeat("x ", 200)),
		{Role: bschemas.ChatMessageRoleUser, Content: &bschemas.ChatMessageContent{ContentStr: strp("u")}},
	}}
	c := &components.Ctx{Ctx: context.Background(), Store: store.NewMemory(store.Options{})} // no Model
	var rep components.Report
	keys, err := off.Offload(req, &rep, c)
	if err != nil || len(keys) != 0 || !rep.Skipped || len(req.Input) != 3 {
		t.Fatalf("no-model summarize must skip untouched: keys=%d skipped=%v len=%d err=%v", len(keys), rep.Skipped, len(req.Input), err)
	}
}

// TestSummarizeModelErrorFailsOpen: a model error must surface as an error (the
// pipeline reverts the component) and leave the transcript untouched.
func TestSummarizeModelErrorFailsOpen(t *testing.T) {
	off := newComp(t, "summarize", "keep_last: 1\nstart_from_message: 0\nmin_tokens: 1\n")
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		{Role: bschemas.ChatMessageRoleSystem, Content: &bschemas.ChatMessageContent{ContentStr: strp("s")}},
		toolMsg(strings.Repeat("output ", 100)),
		{Role: bschemas.ChatMessageRoleUser, Content: &bschemas.ChatMessageContent{ContentStr: strp("u")}},
	}}
	before := len(req.Input)
	c := &components.Ctx{Ctx: context.Background(), Store: store.NewMemory(store.Options{}),
		Model: components.ModelSpec{Incoming: stubModel{err: errBoom}}}
	var rep components.Report
	_, err := off.Offload(req, &rep, c)
	if err == nil {
		t.Fatal("model error must be returned so the pipeline reverts")
	}
	if len(req.Input) != before {
		t.Fatal("transcript must be untouched on model error")
	}
}

// TestSummarizeEmptyResponseSkips: an empty model response is a no-op, not a
// broken summary.
func TestSummarizeEmptyResponseSkips(t *testing.T) {
	off := newComp(t, "summarize", "keep_last: 1\nstart_from_message: 0\nmin_tokens: 1\n")
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		{Role: bschemas.ChatMessageRoleSystem, Content: &bschemas.ChatMessageContent{ContentStr: strp("s")}},
		toolMsg(strings.Repeat("output ", 100)),
		{Role: bschemas.ChatMessageRoleUser, Content: &bschemas.ChatMessageContent{ContentStr: strp("u")}},
	}}
	c := &components.Ctx{Ctx: context.Background(), Store: store.NewMemory(store.Options{}),
		Model: components.ModelSpec{Incoming: stubModel{resp: "   "}}}
	var rep components.Report
	keys, err := off.Offload(req, &rep, c)
	if err != nil || len(keys) != 0 || !rep.Skipped || len(req.Input) != 3 {
		t.Fatalf("empty summary must skip untouched: keys=%d skipped=%v len=%d err=%v", len(keys), rep.Skipped, len(req.Input), err)
	}
}

// TestExtractRLMUsesModel: strategy=rlm runs the MODEL's filter (not silently the
// deterministic projection) and its result is spliced in.
//
// The stub answers with the reduced VALUE, which is what the rlm/single leg asks for — it
// prompts for "a smaller value of the same shape", not for a program. It used to answer
// with a Starlark program, and that program's SOURCE was accepted as the reduction and
// spliced into the transcript in place of the tool output: smaller, sane, and completely
// unrelated to the input. The gate now requires the result to derive from the input, so
// that no longer passes anywhere — including here.
func TestExtractRLMUsesModel(t *testing.T) {
	// economic_gate: false — this is a MECHANISM test (does the model-written filter
	// run and reduce?), and its small fixture output is genuinely uneconomic, so the
	// #28 gate would correctly suppress the call. Gate economics are tested in
	// components/offload/extract_econ_test.go against the dollar figures directly.
	off := newComp(t, "extract_llm", "strategy: rlm\nmin_tokens: 1\neconomic_gate: false\nmodel:\n  source: config\n")
	st := store.NewMemory(store.Options{})
	pad := strings.Repeat("padding ", 40) // so reduction beats the marker cost (D1 guard)
	body := `[{"id":1,"name":"keep this one ` + pad + `"},{"id":2,"name":"drop it ` + pad + `"}]`
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		{Role: bschemas.ChatMessageRoleUser, Content: &bschemas.ChatMessageContent{ContentStr: strp("find keep")}},
		toolMsg(body),
	}}
	kept := `[{"id":1,"name":"keep this one ` + pad + `"}]`
	c := &components.Ctx{Ctx: context.Background(), Store: st, Model: components.ModelSpec{Static: stubModel{resp: kept}}}
	var rep components.Report
	if keys, err := off.Offload(req, &rep, c); err != nil || len(keys) != 1 {
		t.Fatalf("rlm should run the model filter: keys=%v err=%v skipped=%v", keys, err, rep.Skipped)
	}
	if strings.Contains(schema.MessageText(req.Input[1]), "drop it") {
		t.Fatal("rlm/code filter should have dropped the non-keep record")
	}
}

// TestExtractCodeUsesModel: the code strategy runs the model's Starlark filter and
// keeps only the matching records (a contained subset), with a marker.
func TestExtractCodeUsesModel(t *testing.T) {
	// economic_gate: false — this is a MECHANISM test (does the model-written filter
	// run and reduce?), and its small fixture output is genuinely uneconomic, so the
	// #28 gate would correctly suppress the call. Gate economics are tested in
	// components/offload/extract_econ_test.go against the dollar figures directly.
	off := newComp(t, "extract_llm", "strategy: code\nmin_tokens: 1\neconomic_gate: false\nmodel:\n  source: config\n")
	st := store.NewMemory(store.Options{})
	pad := strings.Repeat("padding ", 40) // so reduction beats the marker cost (D1 guard)
	body := `[{"id":1,"name":"keep this ` + pad + `"},{"id":2,"name":"drop this ` + pad + `"},{"id":3,"name":"keep that ` + pad + `"}]`
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		{Role: bschemas.ChatMessageRoleUser, Content: &bschemas.ChatMessageContent{ContentStr: strp("find the keep records")}},
		toolMsg(body),
	}}
	filter := "data = json.decode(INPUT)\nOUTPUT = json.encode([r for r in data if \"keep\" in r[\"name\"]])\n"
	c := &components.Ctx{Ctx: context.Background(), Store: st, Model: components.ModelSpec{Static: stubModel{resp: filter}}}
	var rep components.Report
	keys, err := off.Offload(req, &rep, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("extract code should have reduced the tool output (skipped=%v)", rep.Skipped)
	}
	out := schema.MessageText(req.Input[1])
	if strings.Contains(out, "drop this") {
		t.Fatalf("code filter should have dropped non-keep records: %q", out)
	}
	if !strings.Contains(out, "keep this") || !strings.Contains(out, "<<cg:") {
		t.Fatalf("expected kept records + marker: %q", out)
	}
}

// With the extract split, the deterministic noise collapse is the separate `extract`
// component. Repeated identical lines are collapsed by it (no LLM needed), with a
// recovery marker.
func TestDeterministicExtractCollapsesRepeats(t *testing.T) {
	off := newComp(t, "extract", "min_tokens: 1\n")
	st := store.NewMemory(store.Options{})
	lines := make([]string, 30)
	for i := range lines {
		lines[i] = "irrelevant filler line"
	}
	lines[15] = "the keep marker line"
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		{Role: bschemas.ChatMessageRoleUser, Content: &bschemas.ChatMessageContent{ContentStr: strp("find keep")}},
		toolMsg(strings.Join(lines, "\n")),
	}}
	c := &components.Ctx{Ctx: context.Background(), Store: st}
	var rep components.Report
	if _, err := off.Offload(req, &rep, c); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(schema.MessageText(req.Input[1]), "<<cg:") {
		t.Fatal("deterministic extract must collapse repeated lines with a marker")
	}
	if !strings.Contains(schema.MessageText(req.Input[1]), "the keep marker line") {
		t.Fatal("the unique informative line must be kept verbatim")
	}
}

// extract_llm with no model available is a clean no-op (deterministic collapse is a
// separate component now — extract_llm never silently falls back to it).
func TestExtractLLMNilModelSkips(t *testing.T) {
	// economic_gate: false — this is a MECHANISM test (does the model-written filter
	// run and reduce?), and its small fixture output is genuinely uneconomic, so the
	// #28 gate would correctly suppress the call. Gate economics are tested in
	// components/offload/extract_econ_test.go against the dollar figures directly.
	off := newComp(t, "extract_llm", "strategy: code\nmin_tokens: 1\neconomic_gate: false\nmodel:\n  source: config\n")
	st := store.NewMemory(store.Options{})
	body := strings.Repeat("some log line\n", 40)
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		{Role: bschemas.ChatMessageRoleUser, Content: &bschemas.ChatMessageContent{ContentStr: strp("find keep")}},
		toolMsg(body),
	}}
	c := &components.Ctx{Ctx: context.Background(), Store: st} // no model
	var rep components.Report
	keys, err := off.Offload(req, &rep, c)
	if err != nil || len(keys) != 0 || !rep.Skipped {
		t.Fatalf("extract_llm with no model must skip: keys=%d skipped=%v err=%v", len(keys), rep.Skipped, err)
	}
	if strings.Contains(schema.MessageText(req.Input[1]), "<<cg:") {
		t.Fatal("extract_llm must not touch content without a model")
	}
}
