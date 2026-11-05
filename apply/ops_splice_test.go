package apply_test

import (
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
)

// midSummarizer replaces messages 1..2 with one summary message: a count-changing run.
type midSummarizer struct{}

func (midSummarizer) Name() string                 { return "midsum" }
func (midSummarizer) Enabled(*components.Ctx) bool { return true }
func (midSummarizer) Reformat(req *bschemas.BifrostChatRequest, _ *components.Report, _ *components.Ctx) error {
	if len(req.Input) < 4 {
		return nil
	}
	sum := req.Input[1]
	schema.SetMessageText(&sum, "s")
	out := []bschemas.ChatMessage{req.Input[0], sum}
	req.Input = append(out, req.Input[3:]...)
	return nil
}

func init() {
	components.Register("midsum", func([]byte) (components.Component, error) { return midSummarizer{}, nil })
}

func TestBodyOptsCountChangeEmitsSplice(t *testing.T) {
	long := strings.Repeat("some long earlier turn text ", 20)
	body := `{"model":"claude-x","messages":[
		{"role":"user","content":"first"},
		{"role":"assistant","content":"` + long + `"},
		{"role":"user","content":"` + long + `"},
		{"role":"assistant","content":"tail one"},
		{"role":"user","content":"final"}
	]}`
	res := traceOf(t, "midsum", bschemas.Anthropic, body)
	if !res.Changed {
		t.Fatal("precondition: the count-changing rewrite must be written back")
	}
	if res.Trace.OpsDropped != 0 || len(res.Trace.Ops) != 1 || res.Trace.Ops[0].MessageSplice == nil {
		t.Fatalf("ops = %+v dropped = %d", res.Trace.Ops, res.Trace.OpsDropped)
	}
	s := res.Trace.Ops[0].MessageSplice
	// Wire bytes: the two removed JSON message elements, and the one inserted.
	in := gjson.Get(body, "messages").Array()
	out := gjson.GetBytes(res.Body, "messages").Array()
	if s.At != 1 || s.Removed != 2 || s.Inserted != 1 ||
		res.Trace.Ops[0].SizeBefore.Bytes != len(in[1].Raw)+len(in[2].Raw) ||
		res.Trace.Ops[0].SizeAfter.Bytes != len(out[1].Raw) {
		t.Fatalf("splice = %+v op = %+v", s, res.Trace.Ops[0])
	}
}

// dropFirstExchange replaces Input[1..3] with a summary: the first tool exchange of the transcript below.
type dropFirstExchange struct{}

func (dropFirstExchange) Name() string                 { return "dropfirst" }
func (dropFirstExchange) Enabled(*components.Ctx) bool { return true }
func (dropFirstExchange) Reformat(req *bschemas.BifrostChatRequest, _ *components.Report, _ *components.Ctx) error {
	if len(req.Input) < 6 {
		return nil
	}
	sum := req.Input[1]
	schema.SetMessageText(&sum, "s")
	out := []bschemas.ChatMessage{req.Input[0], sum}
	req.Input = append(out, req.Input[4:]...)
	return nil
}

func init() {
	components.Register("dropfirst", func([]byte) (components.Component, error) { return dropFirstExchange{}, nil })
}

// context-guru#910: summarize hit ops_dropped on most requests. A user message that holds a tool_result and a
// text block ("CALL_TOOL again") has the same text on every turn. The rebuild matched survivors by bytes and took
// the FIRST unused slot, so the text of a survivor matched the text of a removed turn. It then emitted that
// turn's wire message, which holds an orphan tool_result, in the wrong place. The sizes did not add up, and
// every operation was dropped. A survivor must keep its own wire message, and the splice must be reported.
func TestSpliceKeepsSurvivorsWithIdenticalSiblingText(t *testing.T) {
	tu := func(id string) string {
		return `{"role":"assistant","content":[{"type":"tool_use","id":"` + id + `","name":"Bash","input":{"command":"` + id + `"}}]}`
	}
	tr := func(id, out string) string {
		return `{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id + `","content":"` + out + `"},{"type":"text","text":"CALL_TOOL again"}]}`
	}
	body := `{"model":"claude-x","messages":[
		{"role":"user","content":"first"},` + tu("a") + `,` + tr("a", "out a") + `,` + tu("b") + `,` + tr("b", "out b") + `,` + tu("c") + `,` + tr("c", "out c") + `]}`
	res := traceOf(t, "dropfirst", bschemas.Anthropic, body)
	if !res.Changed {
		t.Fatal("precondition: the count-changing rewrite must be written back")
	}
	out := gjson.GetBytes(res.Body, "messages").Array()
	var ids []string
	for _, m := range out {
		ids = append(ids, gjson.Get(m.Raw, "content.0.id").String()+gjson.Get(m.Raw, "content.0.tool_use_id").String())
	}
	// first, summary, then the exchanges b and c in order. Nothing of a.
	want := []string{"", "", "b", "b", "c", "c"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("forwarded messages carry ids %q, want %q", ids, want)
	}
	if res.Trace.OpsDropped != 0 || len(res.Trace.Ops) != 1 || res.Trace.Ops[0].MessageSplice == nil {
		t.Fatalf("ops = %+v dropped = %d: the splice was not reported", res.Trace.Ops, res.Trace.OpsDropped)
	}
}
