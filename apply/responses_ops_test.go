package apply_test

import (
	"context"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// systemReducer rewrites every system-role message, which on the Responses wire is the instructions.
type systemReducer struct{}

func (systemReducer) Name() string                 { return "responses-system-test" }
func (systemReducer) Enabled(*components.Ctx) bool { return true }
func (systemReducer) Reformat(req *bschemas.BifrostChatRequest, _ *components.Report, _ *components.Ctx) error {
	for i := range req.Input {
		if req.Input[i].Role == bschemas.ChatMessageRoleSystem {
			schema.SetMessageText(&req.Input[i], "short")
		}
	}
	return nil
}

func responsesRun(t *testing.T, c components.Component, body []byte) apply.Result {
	t.Helper()
	p := components.NewPipeline([]components.Component{c}, nil)
	return apply.BodyOpts(context.Background(), p, store.NewMemory(store.Options{}), apply.Opts{
		Provider: bschemas.OpenAI, API: "responses", Body: body,
	})
}

// checkItemChains is checkChains for the Responses wire: the positions are the items of input.
func checkItemChains(t *testing.T, body []byte, res apply.Result) {
	t.Helper()
	in := gjson.GetBytes(body, "input").Array()
	out := gjson.GetBytes(res.Body, "input").Array()
	if len(in) != len(out) {
		t.Fatalf("item count changed: %d -> %d", len(in), len(out))
	}
	last, got := map[int]int{}, map[int]int{}
	for _, o := range res.Trace.Ops {
		if o.At.Section != accountant.SectionMessages {
			t.Fatalf("op in section %v, want messages", o.At.Section)
		}
		i := o.At.Index
		if i < 0 || i >= len(in) {
			t.Fatalf("op at item %d, body has %d items", i, len(in))
		}
		if prev, ok := last[i]; ok {
			if o.SizeBefore.Bytes != prev {
				t.Fatalf("op chain broken at item %d: %d after %d", i, o.SizeBefore.Bytes, prev)
			}
		} else if o.SizeBefore.Bytes != len(in[i].Raw) {
			t.Fatalf("first op on item %d: SizeBefore %d, item is %d bytes", i, o.SizeBefore.Bytes, len(in[i].Raw))
		}
		last[i] = o.SizeAfter.Bytes
		got[i] += o.SizeBefore.Bytes - o.SizeAfter.Bytes
	}
	for i := range in {
		want := len(in[i].Raw) - len(out[i].Raw)
		if got[i] != want {
			t.Fatalf("item %d: ops remove %d bytes, bodies differ by %d", i, got[i], want)
		}
		if a, ok := last[i]; ok && a != len(out[i].Raw) {
			t.Fatalf("last op on item %d: SizeAfter %d, forwarded item is %d bytes", i, a, len(out[i].Raw))
		}
	}
}

// One tool output on the Responses wire: the operation is at the function_call_output's index in
// input, not at its normalized index (instructions take normalized index 0), and it carries the
// item's wire bytes.
func TestResponsesTraceOpsCarryTheInputItemIndex(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","instructions":"agent","input":[` +
		`{"role":"user","content":"run it"},` +
		`{"type":"function_call","call_id":"c1","name":"bash","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c1","output":"` + strings.Repeat("line of tool output\\n", 40) + `"}]}`)
	res := responsesRun(t, responseReducer{}, body)
	if !res.Changed {
		t.Fatal("the tool output was not rewritten")
	}
	if len(res.Trace.Ops) != 1 || res.Trace.OpsDropped != 0 {
		t.Fatalf("ops = %+v, dropped %d; want one op", res.Trace.Ops, res.Trace.OpsDropped)
	}
	if got := res.Trace.Ops[0].At.Index; got != 2 {
		t.Fatalf("op at input %d, want 2 (the function_call_output)", got)
	}
	checkItemChains(t, body, res)
}

// A multi-block item: one function_call_output whose output holds two text blocks gives two
// normalized messages. Both operations land on that one item, and they chain from the item's bytes
// before to its bytes after. A multi-block message before it shifts the normalized indexes and must
// not shift the wire index.
func TestResponsesTraceOpsOnAMultiBlockItemLandOnThatItem(t *testing.T) {
	big := strings.Repeat("verbose tool output line\\n", 40)
	body := []byte(`{"model":"gpt-5.5","instructions":"agent","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"a"},{"type":"input_text","text":"b"}]},` +
		`{"type":"function_call","call_id":"c1","name":"bash","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c1","output":[` +
		`{"type":"input_text","text":"` + big + `"},{"type":"input_text","text":"` + big + `"}]}]}`)
	res := responsesRun(t, responseReducer{}, body)
	if !res.Changed {
		t.Fatal("the tool output was not rewritten")
	}
	if len(res.Trace.Ops) != 2 || res.Trace.OpsDropped != 0 {
		t.Fatalf("ops = %+v, dropped %d; want two ops", res.Trace.Ops, res.Trace.OpsDropped)
	}
	for _, o := range res.Trace.Ops {
		if o.At.Index != 2 {
			t.Fatalf("op at input %d, want 2 (the function_call_output)", o.At.Index)
		}
	}
	checkItemChains(t, body, res)
}

// instructions are the system section on this wire, not an input item. A component change there
// cannot be placed as a message operation, so it is dropped, never put on an input item.
func TestResponsesInstructionsChangeIsDroppedNotMisplaced(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","instructions":"` + strings.Repeat("long instructions ", 30) + `","input":[` +
		`{"role":"user","content":"run it"}]}`)
	res := responsesRun(t, systemReducer{}, body)
	if !res.Changed {
		t.Fatal("the instructions were not rewritten")
	}
	if len(res.Trace.Ops) != 0 || res.Trace.OpsDropped == 0 {
		t.Fatalf("ops = %+v, dropped %d; want the change dropped", res.Trace.Ops, res.Trace.OpsDropped)
	}
}

// A string input is one position. A rewrite of it is one operation at index 0 with the string's
// wire bytes.
func TestResponsesTraceOpsOnAStringInput(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","input":"` + strings.Repeat("long prompt ", 30) + `"}`)
	res := responsesRun(t, userReducer{}, body)
	if !res.Changed {
		t.Fatal("the input was not rewritten")
	}
	if len(res.Trace.Ops) != 1 || res.Trace.OpsDropped != 0 {
		t.Fatalf("ops = %+v, dropped %d; want one op", res.Trace.Ops, res.Trace.OpsDropped)
	}
	o := res.Trace.Ops[0]
	before, after := gjson.GetBytes(body, "input").Raw, gjson.GetBytes(res.Body, "input").Raw
	if o.At.Index != 0 || o.SizeBefore.Bytes != len(before) || o.SizeAfter.Bytes != len(after) {
		t.Fatalf("op = %+v, want index 0, %d -> %d bytes", o, len(before), len(after))
	}
}

// userReducer rewrites every user-role message.
type userReducer struct{}

func (userReducer) Name() string                 { return "responses-user-test" }
func (userReducer) Enabled(*components.Ctx) bool { return true }
func (userReducer) Reformat(req *bschemas.BifrostChatRequest, _ *components.Report, _ *components.Ctx) error {
	for i := range req.Input {
		if req.Input[i].Role == bschemas.ChatMessageRoleUser {
			schema.SetMessageText(&req.Input[i], "short")
		}
	}
	return nil
}

// A summary replaces input items 1..2 with one summary item. The pipeline reports it as one splice at
// the input index of the first removed item, sized in the items' wire bytes, so the library's sum
// check holds: the operation removes exactly the bytes the input lost.
func TestResponsesSummarySpliceCarriesInputItemBytes(t *testing.T) {
	cfg := pipe(t, "pipeline: [summarize]\ncomponents:\n  summarize: {keep_first: 2, keep_last: 1, start_from_message: 0, min_tokens: 1, trigger: {min_request_frac: 0}}\n")
	p, _ := cfg.Build(nil)
	st := store.NewMemory(store.Options{})
	body := []byte(`{"model":"gpt-5.6","instructions":"keep this instruction","input":[` +
		`{"role":"user","content":"task"},` +
		`{"role":"assistant","content":"` + strings.Repeat("long answer ", 50) + `"},` +
		`{"role":"user","content":"more context"},` +
		`{"role":"user","content":"final question"}]}`)
	o := apply.Opts{Provider: bschemas.OpenAI, API: "responses", Body: body,
		Models: components.ModelSpec{Incoming: stubModel{resp: "essential facts"}}}
	apply.BodyOpts(context.Background(), p, st, o)
	if !offload.WaitForAllSummariesForTest(5 * time.Second) {
		t.Fatal("background summary did not finish")
	}
	res := apply.BodyOpts(context.Background(), p, st, o)
	if gjson.GetBytes(res.Body, "input.#").Int() != 3 {
		t.Fatalf("the summary did not apply: %s", res.Body)
	}
	if len(res.Trace.Ops) != 1 || res.Trace.OpsDropped != 0 {
		t.Fatalf("ops = %+v, dropped %d; want one splice", res.Trace.Ops, res.Trace.OpsDropped)
	}
	op := res.Trace.Ops[0]
	sp := op.MessageSplice
	if sp == nil || sp.At != 1 || sp.Removed != 2 || sp.Inserted != 1 || op.At.Index != 1 {
		t.Fatalf("splice = %+v at %+v, want At 1, Removed 2, Inserted 1", sp, op.At)
	}
	in, out := gjson.GetBytes(body, "input").Array(), gjson.GetBytes(res.Body, "input").Array()
	if want := len(in[1].Raw) + len(in[2].Raw); op.SizeBefore.Bytes != want {
		t.Fatalf("SizeBefore %d, removed items are %d bytes", op.SizeBefore.Bytes, want)
	}
	if want := len(out[1].Raw); op.SizeAfter.Bytes != want {
		t.Fatalf("SizeAfter %d, summary item is %d bytes", op.SizeAfter.Bytes, want)
	}
}
