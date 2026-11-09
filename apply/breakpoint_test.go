package apply

import (
	"strconv"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/expand"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// inBandRequest is a Claude Code-shaped client request with marks on its system blocks (sysMarks),
// on its tools (toolMarks) and on its last user message (msgMark, the raw cache_control or "").
func inBandRequest(sysMarks, toolMarks int, msgMark string) string {
	cc := `,"cache_control":{"type":"ephemeral"}`
	sys := []string{}
	for i := 0; i < 2; i++ {
		m := ""
		if i < sysMarks {
			m = cc
		}
		sys = append(sys, `{"type":"text","text":"system `+string(rune('a'+i))+`"`+m+`}`)
	}
	tools := []string{}
	for i := 0; i < 2; i++ {
		m := ""
		if i < toolMarks {
			m = cc
		}
		tools = append(tools, `{"name":"T`+string(rune('a'+i))+`","input_schema":{"type":"object"}`+m+`}`)
	}
	last := `{"type":"text","text":"look at <<cg:H>>"`
	if msgMark != "" {
		last += `,"cache_control":` + msgMark
	}
	last += `}`
	return `{"model":"claude-x","system":[` + strings.Join(sys, ",") + `],"tools":[` + strings.Join(tools, ",") +
		`],"messages":[{"role":"user","content":[{"type":"text","text":"task"}]},` +
		`{"role":"assistant","content":[{"type":"text","text":"ok"}]},` +
		`{"role":"user","content":[` + last + `]}]}`
}

// expandResponse is a round's reply that calls expand after a leading text block.
const expandResponse = `{"role":"assistant","content":[{"type":"text","text":"let me look"},` +
	`{"type":"tool_use","id":"toolu_x","name":"context_guru_expand","input":{"id":"H"}}]}`

// followUp builds the in-band follow-up the way the proxy does.
func followUp(t *testing.T, req string) []byte {
	t.Helper()
	out, ok := expand.Continuation("anthropic", []byte(req), []byte(expandResponse), map[string]string{"toolu_x": "THE ORIGINAL"})
	if !ok {
		t.Fatal("continuation failed")
	}
	return out
}

// lastBlockMark is the cache_control on the last block of the last message.
func lastBlockMark(body []byte) gjson.Result {
	return gjson.GetBytes(body, "messages.@reverse.0.content.@reverse.0.cache_control")
}

// #784. The client's last message breakpoint moves to the end of the follow-up, keeping its value.
// The count stays the client's at 0..4 marks in system/tools, and those marks stay where they were.
func TestTheInBandFollowUpCarriesTheClientsMessageBreakpointAtItsEnd(t *testing.T) {
	for _, c := range []struct {
		name            string
		sys, tools      int
		mark            string
		wantMoved, want int
	}{
		{"0 breakpoints", 0, 0, "", 0, 0},
		{"1 breakpoint, on the message", 0, 0, `{"type":"ephemeral"}`, 1, 1},
		{"3 breakpoints, Claude Code's shape", 2, 0, `{"type":"ephemeral"}`, 1, 3},
		{"4 breakpoints, the cap", 2, 1, `{"type":"ephemeral"}`, 1, 4},
		{"4 breakpoints, ttl 1h", 2, 1, `{"type":"ephemeral","ttl":"1h"}`, 1, 4},
		{"3 breakpoints, none in messages", 2, 1, "", 0, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			body := followUp(t, inBandRequest(c.sys, c.tools, c.mark))
			moved, noMark, _, _ := InBandBreakpointStats()
			out := MarkInBandFollowUp("anthropic", body)
			m2, nm2, _, _ := InBandBreakpointStats()
			if got := int(m2 - moved); got != c.wantMoved {
				t.Fatalf("moved rose by %d, want %d", got, c.wantMoved)
			}
			before, after := CountBreakpoints(body), CountBreakpoints(out)
			if after.Total() != c.want || after.Total() != before.Total() {
				t.Fatalf("breakpoints %d -> %d, want %d both: a move must keep the client's count", before.Total(), after.Total(), c.want)
			}
			if after.System != before.System || after.Tools != before.Tools {
				t.Fatalf("system/tools marks changed: %+v -> %+v", before, after)
			}
			if gjson.GetBytes(out, "system").Raw != gjson.GetBytes(body, "system").Raw ||
				gjson.GetBytes(out, "tools").Raw != gjson.GetBytes(body, "tools").Raw {
				t.Fatal("system or tools bytes changed")
			}
			if c.mark == "" {
				if string(out) != string(body) {
					t.Fatalf("no message breakpoint, yet the body changed:\n%s", out)
				}
				if nm2 != noMark+1 {
					t.Errorf("no_mark rose by %d, want 1", nm2-noMark)
				}
				return
			}
			if got := lastBlockMark(out).Raw; got != c.mark {
				t.Fatalf("last block mark = %s, want the client's %s (same type and ttl)", got, c.mark)
			}
			if gjson.GetBytes(out, "messages.2.content.0.cache_control").Exists() {
				t.Fatal("the client's mark was copied, not moved")
			}
			if lb := gjson.GetBytes(out, "messages.@reverse.0.content.@reverse.0"); lb.Get("type").String() != "tool_result" {
				t.Fatalf("the mark is not on the proxy's tool_result: %s", lb.Raw)
			}
		})
	}
}

// A client message whose content is a string (Claude Code sends some this way) is a mark source too,
// at message level, and the follow-up still gets it.
func TestAMessageLevelBreakpointMovesToo(t *testing.T) {
	req := `{"model":"claude-x","messages":[{"role":"user","content":"look at <<cg:H>>","cache_control":{"type":"ephemeral","ttl":"1h"}}]}`
	out := MarkInBandFollowUp("anthropic", followUp(t, req))
	if got := lastBlockMark(out).Get("ttl").String(); got != "1h" {
		t.Fatalf("ttl %q, want 1h:\n%s", got, out)
	}
	if gjson.GetBytes(out, "messages.0.cache_control").Exists() {
		t.Fatal("the message-level mark stayed behind")
	}
	if n := CountBreakpoints(out).Total(); n != 1 {
		t.Fatalf("%d breakpoints, want 1", n)
	}
}

// A last message with string content (a host-built follow-up may end so) takes the mark as the one
// text block it means.
func TestAStringLastMessageTakesTheMarkAsATextBlock(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"q","cache_control":{"type":"ephemeral"}}]},` +
		`{"role":"assistant","content":"a"},{"role":"user","content":"tool said x"}]}`)
	out := MarkInBandFollowUp("anthropic", body)
	last := gjson.GetBytes(out, "messages.2.content")
	if !last.IsArray() || last.Get("0.text").String() != "tool said x" || !last.Get("0.cache_control").IsObject() {
		t.Fatalf("string content not turned into a marked text block:\n%s", out)
	}
	if n := CountBreakpoints(out).Total(); n != 1 {
		t.Fatalf("%d breakpoints, want 1", n)
	}
}

// Round 2 of the loop: the mark round 1 moved moves on again, and the count never grows.
func TestASecondRoundMovesTheMarkAgain(t *testing.T) {
	r1 := MarkInBandFollowUp("anthropic", followUp(t, inBandRequest(2, 0, `{"type":"ephemeral"}`)))
	resp := `{"role":"assistant","content":[{"type":"tool_use","id":"toolu_y","name":"context_guru_expand","input":{"id":"H"}}]}`
	r2, ok := expand.Continuation("anthropic", r1, []byte(resp), map[string]string{"toolu_y": "AGAIN"})
	if !ok {
		t.Fatal("round 2 continuation failed")
	}
	out := MarkInBandFollowUp("anthropic", r2)
	if n := CountBreakpoints(out).Total(); n != 3 {
		t.Fatalf("%d breakpoints after 2 rounds, want 3", n)
	}
	if lb := gjson.GetBytes(out, "messages.@reverse.0.content.@reverse.0"); lb.Get("tool_use_id").String() != "toolu_y" || !lb.Get("cache_control").IsObject() {
		t.Fatalf("round 2's tool_result is not marked: %s", lb.Raw)
	}
	if again := MarkInBandFollowUp("anthropic", out); string(again) != string(out) {
		t.Fatal("a follow-up already marked at its end must stay byte-identical")
	}
}

// Only Anthropic messages carry cache_control; every other wire is untouched.
func TestOtherWiresAreANoOp(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"q","cache_control":{"type":"ephemeral"}}]},{"role":"tool","content":"x"}]}`)
	for _, w := range []string{"openai", "responses", "bedrock", ""} {
		if out := MarkInBandFollowUp(w, body); string(out) != string(body) {
			t.Errorf("%q: body changed", w)
		}
	}
}

// Fail open: shapes the placer cannot mark return the exact input bytes.
func TestUnmarkableFollowUpsReturnTheOriginalBytes(t *testing.T) {
	for name, body := range map[string]string{
		"not json":        `{"messages":[`,
		"no messages":     `{"model":"x"}`,
		"empty content":   `{"messages":[{"role":"user","content":[{"type":"text","text":"q","cache_control":{"type":"ephemeral"}}]},{"role":"user","content":[]}]}`,
		"thinking at end": `{"messages":[{"role":"user","content":[{"type":"text","text":"q","cache_control":{"type":"ephemeral"}}]},{"role":"assistant","content":[{"type":"thinking","thinking":"t"}]}]}`,
		"empty string":    `{"messages":[{"role":"user","content":[{"type":"text","text":"q","cache_control":{"type":"ephemeral"}}]},{"role":"user","content":""}]}`,
		"textual mention": `{"messages":[{"role":"user","content":[{"type":"text","text":"\"cache_control\" in prose"}]},{"role":"user","content":[{"type":"text","text":"x"}]}]}`,
	} {
		if out := MarkInBandFollowUp("anthropic", []byte(body)); string(out) != body {
			t.Errorf("%s: body changed:\n%s", name, out)
		}
	}
}

// #784, why expand keeps this off by default. The client never sees the in-band tool_use or
// tool_result, so its next request diverges from the follow-up at the assistant turn, before the
// moved mark, with and without fixed_restore. The provider reads an entry only for a request that
// carries the same bytes up to the mark, so no client request reads what the follow-up wrote.
func TestTheNextClientRequestDivergesBeforeTheMark(t *testing.T) {
	client := inBandRequest(2, 0, `{"type":"ephemeral"}`)
	n := int(gjson.Get(client, "messages.#").Int())
	follow := MarkInBandFollowUp("anthropic", followUp(t, client))
	if !lastBlockMark(follow).Exists() {
		t.Fatal("the follow-up carries no mark at its end; nothing to test")
	}
	// What the client gets back: the round's text, never our tool_use. It sends that turn back
	// with its next question.
	next, err := appendRaw([]byte(client), `{"role":"assistant","content":[{"type":"text","text":"let me look"},{"type":"text","text":"ANSWERED"}]}`,
		`{"role":"user","content":[{"type":"text","text":"next question"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	restored, ok := expand.InsertRestored("anthropic", next, []expand.Insertion{{At: n, Text: expand.RestoredText("H", "THE ORIGINAL")}})
	if !ok {
		t.Fatal("fixed restore could not insert")
	}
	fMsgs := gjson.GetBytes(follow, "messages").Array()
	for name, body := range map[string][]byte{"kept-verbatim": next, "fixed_restore": restored} {
		msgs := gjson.GetBytes(body, "messages").Array()
		d := firstDifference(fMsgs, msgs)
		if d > n {
			t.Fatalf("%s: the next client request shares the follow-up up to message %d, past the client's %d", name, d, n)
		}
		if d >= len(fMsgs)-1 {
			t.Fatalf("%s: it shares the follow-up's tail, so it could read the moved mark", name)
		}
	}
}

// firstDifference is the index of the first message that differs between a and b, cache marks
// aside (the provider does not hash them).
func firstDifference(a, b []gjson.Result) int {
	for i := range a {
		if i >= len(b) || unmarked(a[i].Raw) != unmarked(b[i].Raw) {
			return i
		}
	}
	return len(a)
}

func unmarked(raw string) string {
	out := []byte(raw)
	gjson.Get(raw, "content").ForEach(func(k, _ gjson.Result) bool {
		out, _ = sjson.DeleteBytes(out, "content."+k.String()+".cache_control")
		return true
	})
	out, _ = sjson.DeleteBytes(out, "cache_control")
	return string(out)
}

func appendRaw(body []byte, msgs ...string) ([]byte, error) {
	var err error
	for _, m := range msgs {
		if body, err = sjson.SetRawBytes(body, "messages.-1", []byte(m)); err != nil {
			return nil, err
		}
	}
	return body, nil
}

// The lookback guard: a follow-up that put more than lookbackBlocks blocks after the client's mark
// is left unmarked and counted, because the moved mark would lose the client's cached prefix.
func TestAMarkPastTheLookbackIsNotMoved(t *testing.T) {
	blocks := func(k int) string {
		b := make([]string, k)
		for i := range b {
			b[i] = `{"type":"tool_result","tool_use_id":"t` + strconv.Itoa(i) + `","content":"x"}`
		}
		return strings.Join(b, ",")
	}
	for _, c := range []struct {
		after int
		moved bool
	}{{lookbackBlocks, true}, {lookbackBlocks + 1, false}} {
		body := `{"messages":[{"role":"user","content":[{"type":"text","text":"q","cache_control":{"type":"ephemeral"}}]},` +
			`{"role":"user","content":[` + blocks(c.after) + `]}]}`
		_, _, _, far := InBandBreakpointStats()
		out := MarkInBandFollowUp("anthropic", []byte(body))
		_, _, _, far2 := InBandBreakpointStats()
		if got := lastBlockMark(out).Exists(); got != c.moved {
			t.Fatalf("%d blocks after the mark: moved=%v, want %v", c.after, got, c.moved)
		}
		if !c.moved && (string(out) != body || far2 != far+1) {
			t.Fatalf("%d blocks after the mark: body changed or too_far rose by %d, want 1", c.after, far2-far)
		}
	}
}

// Review #786 round 1: a last block that is not a JSON object must not be replaced by the mark.
// sjson would overwrite the whole element, and its bytes would be lost.
func TestANonObjectLastBlockKeepsItsBytes(t *testing.T) {
	for _, last := range []string{`"x"`, `42`, `null`} {
		body := `{"messages":[{"role":"user","content":[{"type":"text","text":"q","cache_control":{"type":"ephemeral"}}]},` +
			`{"role":"user","content":[{"type":"text","text":"keep"},` + last + `]}]}`
		_, _, nf, _ := InBandBreakpointStats()
		out := MarkInBandFollowUp("anthropic", []byte(body))
		_, _, nf2, _ := InBandBreakpointStats()
		if string(out) != body {
			t.Errorf("%s: body changed:\n%s", last, out)
		}
		if nf2 != nf+1 {
			t.Errorf("%s: not_found rose by %d, want 1", last, nf2-nf)
		}
	}
}

// The shared placer refuses a non-object target on the add path too (the #723 anchor's).
func TestThePlacerSkipsANonObjectTarget(t *testing.T) {
	body := `{"messages":[{"role":"user","content":["x"]}]}`
	var st placerStats
	out, ok := placeBreakpoint([]byte(body), blockPath(0, 0), ephemeralMark, "", &st)
	if ok || string(out) != body || st.notFound.Load() != 1 || st.written.Load() != 0 {
		t.Fatalf("ok=%v notFound=%d body=%s", ok, st.notFound.Load(), out)
	}
}

// Review #796: the provider rejects cache_control on an empty text block. A follow-up that ends in
// one keeps its bytes, the client's mark stays where it was, and not_found counts the skip.
func TestAnEmptyTextLastBlockTakesNoMark(t *testing.T) {
	body := `{"messages":[{"role":"user","content":[{"type":"text","text":"q","cache_control":{"type":"ephemeral"}}]},` +
		`{"role":"user","content":[{"type":"text","text":"keep"},{"type":"text","text":""}]}]}`
	_, _, nf, _ := InBandBreakpointStats()
	out := MarkInBandFollowUp("anthropic", []byte(body))
	_, _, nf2, _ := InBandBreakpointStats()
	if string(out) != body {
		t.Errorf("body changed:\n%s", out)
	}
	if nf2 != nf+1 {
		t.Errorf("not_found rose by %d, want 1", nf2-nf)
	}
}
