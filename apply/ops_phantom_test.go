package apply_test

import (
	"context"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// userShrinker rewrites every non-tool message, which includes the read-only sibling text slots
// of an Anthropic user message that also carries a tool_result.
type userShrinker struct{}

func (userShrinker) Name() string                 { return "usershrink" }
func (userShrinker) Enabled(*components.Ctx) bool { return true }
func (userShrinker) Reformat(req *bschemas.BifrostChatRequest, _ *components.Report, _ *components.Ctx) error {
	for i := range req.Input {
		if req.Input[i].Role != bschemas.ChatMessageRoleTool {
			schema.SetMessageText(&req.Input[i], "x")
		}
	}
	return nil
}

func init() {
	components.Register("usershrink", func([]byte) (components.Component, error) { return userShrinker{}, nil })
}

func traceOf(t *testing.T, comp string, provider bschemas.ModelProvider, body string) apply.Result {
	t.Helper()
	p, err := pipe(t, "pipeline: ["+comp+"]\n").Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	return apply.BodyOpts(context.Background(), p, store.NewMemory(store.Options{}), apply.Opts{
		Provider: provider, Body: []byte(body),
	})
}

// A change on a read-only anthropicSiblingText slot is never written back, so no op may claim it.
func TestTraceOpsExcludeSiblingTextChange(t *testing.T) {
	long := strings.Repeat("a long sibling text that the component rewrites to nothing\\n", 8)
	res := traceOf(t, "usershrink", bschemas.Anthropic, `{"model":"claude-x","messages":[
		{"role":"user","content":[{"type":"text","text":"`+long+`"},{"type":"tool_result","tool_use_id":"a","content":"out"}]}
	]}`)
	if res.Changed {
		t.Fatalf("precondition: the sibling change must not be written back")
	}
	if len(res.Trace.Ops) != 0 || res.Trace.OpsDropped == 0 {
		t.Fatalf("ops = %+v dropped = %d, want none and >0", res.Trace.Ops, res.Trace.OpsDropped)
	}
}

// A message bifrost cannot round-trip (an unmodelled field) has its change discarded and the
// original bytes forwarded; no op may claim that saving.
func TestTraceOpsExcludeDiscardedChange(t *testing.T) {
	res := traceOf(t, "attr1", bschemas.OpenAI, `{"model":"gpt-x","messages":[
		{"role":"user","content":"run it"},
		{"role":"tool","tool_call_id":"a","unmodelled_field":{"k":1},"content":"`+
		strings.Repeat("a long original tool output line that the component rewrites\\n", 8)+`"}
	]}`)
	if res.Changed {
		t.Fatalf("precondition: the change must be discarded, not written")
	}
	if len(res.Trace.Ops) != 0 || res.Trace.OpsDropped == 0 {
		t.Fatalf("ops = %+v dropped = %d, want none and >0", res.Trace.Ops, res.Trace.OpsDropped)
	}
}

// Control: a lossless change that is written keeps its op and drops nothing.
func TestTraceOpsKeepWrittenChange(t *testing.T) {
	res := traceOf(t, "attr1", bschemas.OpenAI, string(toolBody()))
	if !res.Changed || len(res.Trace.Ops) != 1 || res.Trace.OpsDropped != 0 || res.Trace.Ops[0].At.Index != 1 {
		t.Fatalf("changed=%v ops = %+v dropped = %d", res.Changed, res.Trace.Ops, res.Trace.OpsDropped)
	}
}
