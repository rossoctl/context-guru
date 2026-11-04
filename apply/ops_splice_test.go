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
