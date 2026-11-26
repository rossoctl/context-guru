package apply_test

import (
	"math/rand"
	"strconv"
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

// dropExchanges is a count-changing component for the tests below. It removes every tool exchange whose
// tool_use id is in dropIDs (the assistant message, its tool messages, and the sibling user text that follows
// them up to the next assistant message) and puts one summary message where the first removed exchange began.
// The tests do not run in parallel, so the package variable is safe.
type dropExchanges struct{}

var dropIDs map[string]bool

func (dropExchanges) Name() string                 { return "dropex" }
func (dropExchanges) Enabled(*components.Ctx) bool { return true }
func (dropExchanges) Reformat(req *bschemas.BifrostChatRequest, _ *components.Report, _ *components.Ctx) error {
	var out []bschemas.ChatMessage
	summarized := false
	in := req.Input
	for i := 0; i < len(in); i++ {
		m := in[i]
		if m.Role == bschemas.ChatMessageRoleAssistant && m.ChatAssistantMessage != nil &&
			len(m.ChatAssistantMessage.ToolCalls) > 0 &&
			m.ChatAssistantMessage.ToolCalls[0].ID != nil && dropIDs[*m.ChatAssistantMessage.ToolCalls[0].ID] {
			j := i + 1
			for j < len(in) && in[j].Role != bschemas.ChatMessageRoleAssistant {
				j++
			}
			if !summarized {
				sum := m
				sum.ChatAssistantMessage = nil
				schema.SetMessageText(&sum, "summary")
				out = append(out, sum)
				summarized = true
			}
			i = j - 1
			continue
		}
		out = append(out, m)
	}
	req.Input = out
	return nil
}

func init() {
	components.Register("dropex", func([]byte) (components.Component, error) { return dropExchanges{}, nil })
}

func spliceTU(id string) string {
	return `{"role":"assistant","content":[{"type":"tool_use","id":"` + id + `","name":"Bash","input":{"command":"` + id + `"}}]}`
}

func spliceTR(id, text string) string {
	return `{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id + `","content":"out ` + id + `"},{"type":"text","text":"` + text + `"}]}`
}

// checkWireOrder asserts the invariant on a forwarded body: every tool_result follows the message that holds
// its tool_use, and the kept messages are exactly the wanted raw messages, in order, each once.
func checkWireOrder(t *testing.T, body []byte, want []string) {
	t.Helper()
	msgs := gjson.GetBytes(body, "messages").Array()
	seenUse := map[string]bool{}
	var got []string
	for i, m := range msgs {
		for _, blk := range m.Get("content").Array() {
			if blk.Get("type").String() == "tool_result" && !seenUse[blk.Get("tool_use_id").String()] {
				t.Fatalf("message %d holds a tool_result for %q before its tool_use: %s", i, blk.Get("tool_use_id").String(), body)
			}
		}
		for _, blk := range m.Get("content").Array() {
			if blk.Get("type").String() == "tool_use" {
				seenUse[blk.Get("id").String()] = true
			}
		}
		got = append(got, m.Raw)
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("forwarded messages:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func summaryMsg() string { return `{"role":"assistant","content":"summary"}` }

// Round-1 finding of #930, trace A: a standalone user text and a kept exchange's sibling text have the same
// bytes. The standalone message must keep its own wire message.
func TestSpliceStandaloneTextEqualToKeptSiblingText(t *testing.T) {
	dropIDs = map[string]bool{"d": true}
	u0 := `{"role":"user","content":"CALL_TOOL again"}`
	b := []string{u0, spliceTU("b"), spliceTR("b", "CALL_TOOL again"), spliceTU("d"), spliceTR("d", "x")}
	body := `{"model":"claude-x","messages":[` + strings.Join(b, ",") + `]}`
	res := traceOf(t, "dropex", bschemas.Anthropic, body)
	if !res.Changed {
		t.Fatal("precondition: the count-changing rewrite must be written back")
	}
	checkWireOrder(t, res.Body, []string{b[0], b[1], b[2], summaryMsg()})
}

// Round-1 finding of #930, trace B.
func TestSpliceKeptPlainUserBeforeExchangeWithSameText(t *testing.T) {
	dropIDs = map[string]bool{"d": true}
	b := []string{`{"role":"user","content":"continue"}`, spliceTU("a"), spliceTR("a", "continue"), spliceTU("d"), spliceTR("d", "x")}
	body := `{"model":"claude-x","messages":[` + strings.Join(b, ",") + `]}`
	res := traceOf(t, "dropex", bschemas.Anthropic, body)
	if !res.Changed {
		t.Fatal("precondition: the count-changing rewrite must be written back")
	}
	checkWireOrder(t, res.Body, []string{b[0], b[1], b[2], summaryMsg()})
}

// Two kept exchanges with the same sibling text, a standalone text of that text before and after each.
func TestSpliceRepeatedSiblingTextAdversarial(t *testing.T) {
	dropIDs = map[string]bool{"d": true}
	s := "CALL_TOOL again"
	u := `{"role":"user","content":"` + s + `"}`
	a1 := `{"role":"assistant","content":"ok1"}`
	a2 := `{"role":"assistant","content":"ok2"}`
	a3 := `{"role":"assistant","content":"ok3"}`
	b := []string{u, a1, u, spliceTU("a"), spliceTR("a", s), a2, u, spliceTU("b"), spliceTR("b", s), a3, u, spliceTU("d"), spliceTR("d", s)}
	body := `{"model":"claude-x","messages":[` + strings.Join(b, ",") + `]}`
	res := traceOf(t, "dropex", bschemas.Anthropic, body)
	if !res.Changed {
		t.Fatal("precondition: the count-changing rewrite must be written back")
	}
	checkWireOrder(t, res.Body, append(append([]string{}, b[:11]...), summaryMsg()))
}

// A seeded property test: random transcripts of standalone texts and tool exchanges with repeated sibling text,
// a random subset of the exchanges dropped. Every result must keep the invariant and the exact kept sequence.
func TestSplicePropertyKeepsWireInvariant(t *testing.T) {
	texts := []string{"CALL_TOOL again", "continue", "x"}
	rng := rand.New(rand.NewSource(930))
	changed := 0
	const cases = 400
	for n := 0; n < cases; n++ {
		var wire []string
		var ids []string
		textOf := func() string { return texts[rng.Intn(len(texts))] }
		turns := 2 + rng.Intn(6)
		for k := 0; k < turns; k++ {
			if rng.Intn(2) == 0 {
				wire = append(wire, `{"role":"user","content":"`+textOf()+`"}`)
				wire = append(wire, `{"role":"assistant","content":"ok `+strconv.Itoa(k)+`"}`)
				continue
			}
			id := "t" + strconv.Itoa(k)
			ids = append(ids, id)
			wire = append(wire, spliceTU(id), spliceTR(id, textOf()))
			wire = append(wire, `{"role":"assistant","content":"ok `+strconv.Itoa(k)+`"}`)
		}
		if len(ids) == 0 {
			continue
		}
		dropIDs = map[string]bool{}
		for _, id := range ids {
			if rng.Intn(2) == 0 {
				dropIDs[id] = true
			}
		}
		if len(dropIDs) == 0 {
			dropIDs[ids[rng.Intn(len(ids))]] = true
		}
		// The expected kept sequence: drop the wire messages of a dropped exchange, summary at the first drop.
		var want []string
		sum := false
		for i := 0; i < len(wire); i++ {
			id := gjson.Get(wire[i], "content.0.id").String()
			if id != "" && dropIDs[id] {
				if !sum {
					want = append(want, summaryMsg())
					sum = true
				}
				i += 1 // skip the tool_result message; the plain assistant after it is kept
				continue
			}
			want = append(want, wire[i])
		}
		body := `{"model":"claude-x","messages":[` + strings.Join(wire, ",") + `]}`
		res := traceOf(t, "dropex", bschemas.Anthropic, body)
		if !res.Changed {
			continue
		}
		changed++
		checkWireOrder(t, res.Body, want)
	}
	if changed < cases/2 {
		t.Fatalf("only %d of %d generated cases were rewritten; the test is not exercising the rebuild", changed, cases)
	}
}
