package apply

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// tailBody is a Claude Code-shaped request with CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1 (#795): 2
// system breakpoints, the client's last mark (msgMark, raw, or "" for none) on the user's text in
// messages[0], and then the environment as a `role: "system"` message of envBytes characters.
func tailBody(msgMark string, envBytes int) []byte {
	m := ""
	if msgMark != "" {
		m = `,"cache_control":` + msgMark
	}
	return []byte(`{"model":"claude-x",` +
		`"system":[{"type":"text","text":"billing"},{"type":"text","text":"s1","cache_control":{"type":"ephemeral"}},` +
		`{"type":"text","text":"s2","cache_control":{"type":"ephemeral"}}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"reminder"},{"type":"text","text":"save todo.txt"` + m + `}]},` +
		`{"role":"system","content":"` + strings.Repeat("e", envBytes) + `"}]}`)
}

// tailDelta is the change in each tail counter across f.
func tailDelta(f func()) (added, noSlot, atEnd, shortTail, noMark, notFound int64) {
	a0, s0, e0, t0, m0, f0 := TailBreakpointStats()
	f()
	a1, s1, e1, t1, m1, f1 := TailBreakpointStats()
	return a1 - a0, s1 - s0, e1 - e0, t1 - t0, m1 - m0, f1 - f0
}

// #795. The client's last mark sits on messages[0] and ~8.7 KB follow it: one mark is added on the
// last block, with the client's value, and the client's own marks stay where they were.
func TestTheTailBreakpointIsAddedWhenTheClientsMarkIsFarFromTheEnd(t *testing.T) {
	for _, mark := range []string{`{"type":"ephemeral"}`, `{"type":"ephemeral","ttl":"1h"}`} {
		body := tailBody(mark, 8667)
		var out []byte
		var ok bool
		added, _, _, _, _, _ := tailDelta(func() { out, ok = markTail(body) })
		if !ok || added != 1 {
			t.Fatalf("%s: ok=%v added=%d, want a breakpoint added", mark, ok, added)
		}
		if got := lastBlockMark(out).Raw; got != mark {
			t.Fatalf("tail mark = %s, want the client's %s (same type and ttl)", got, mark)
		}
		if got := gjson.GetBytes(out, "messages.0.content.1.cache_control").Raw; got != mark {
			t.Fatalf("the client's mark moved or changed: %q", got)
		}
		before, after := CountBreakpoints(body), CountBreakpoints(out)
		if after.Total() != before.Total()+1 || after.System != before.System {
			t.Fatalf("breakpoints %+v -> %+v, want one more in messages", before, after)
		}
		if gjson.GetBytes(out, "messages.1.content.0.text").String() != strings.Repeat("e", 8667) {
			t.Fatal("the system message's text changed")
		}
	}
}

// The cases where nothing is added, each with the counter that says why, and the body untouched.
func TestTheTailBreakpointIsNotAddedWhenItIsNotWanted(t *testing.T) {
	ccEnd := `{"model":"claude-x","messages":[{"role":"user","content":"` + strings.Repeat("u", 9000) + `"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok","cache_control":{"type":"ephemeral"}}]}]}`
	// Claude Code's later requests with the betas setting: the mark on the last tool_result and a
	// short system reminder after it (350 bytes measured).
	shortTail := `{"model":"claude-x","messages":[{"role":"user","content":[{"type":"text","text":"q","cache_control":{"type":"ephemeral"}}]},` +
		`{"role":"system","content":"` + strings.Repeat("r", 350) + `"}]}`
	fourMarks := strings.Replace(string(tailBody(`{"type":"ephemeral"}`, 8667)), `"text":"billing"`,
		`"text":"billing","cache_control":{"type":"ephemeral"}`, 1)
	nonObject := `{"messages":[{"role":"user","content":[{"type":"text","text":"q","cache_control":{"type":"ephemeral"}}]},` +
		`{"role":"user","content":[{"type":"text","text":"` + strings.Repeat("x", 5000) + `"},"bare"]}]}`
	thinking := `{"messages":[{"role":"user","content":[{"type":"text","text":"q","cache_control":{"type":"ephemeral"}}]},` +
		`{"role":"assistant","content":[{"type":"text","text":"` + strings.Repeat("x", 5000) + `"},{"type":"thinking","thinking":"t","signature":"s"}]}]}`
	// A last block that already holds a mark in a spelling lastMessageMark does not read.
	markedLast := `{"messages":[{"role":"user","content":[{"type":"text","text":"q","cache_control":{"type":"ephemeral"}}]},` +
		`{"role":"user","content":[{"type":"text","text":"` + strings.Repeat("x", 5000) + `","cachePoint":{"type":"default"}}]}]}`
	emptyText := `{"messages":[{"role":"user","content":[{"type":"text","text":"q","cache_control":{"type":"ephemeral"}}]},` +
		`{"role":"user","content":[{"type":"text","text":"` + strings.Repeat("x", 5000) + `"},{"type":"text","text":""}]}]}`
	for _, c := range []struct {
		name                                       string
		body                                       string
		noSlot, atEnd, shortTail, noMark, notFound int64
	}{
		{"0 client marks", string(tailBody("", 8667)), 0, 0, 0, 1, 0},
		{"mark at the end", ccEnd, 0, 1, 0, 0, 0},
		{"short tail after the mark", shortTail, 0, 0, 1, 0, 0},
		{"4 marks used", fourMarks, 1, 0, 0, 0, 0},
		{"non-object last block", nonObject, 0, 0, 0, 0, 1},
		{"thinking last block", thinking, 0, 0, 0, 0, 1},
		{"empty text last block", emptyText, 0, 0, 0, 0, 1},
		{"last block already marked", markedLast, 0, 1, 0, 0, 0},
		{"no messages", `{"messages":[]}`, 0, 0, 0, 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out []byte
			var ok bool
			added, noSlot, atEnd, shortTail, noMark, notFound := tailDelta(func() { out, ok = markTail([]byte(c.body)) })
			if ok || added != 0 || string(out) != c.body {
				t.Fatalf("ok=%v added=%d, want the body untouched:\n%s", ok, added, out)
			}
			if noSlot != c.noSlot || atEnd != c.atEnd || shortTail != c.shortTail || noMark != c.noMark || notFound != c.notFound {
				t.Fatalf("counters no_slot=%d at_end=%d short_tail=%d no_mark=%d not_found=%d, want %d %d %d %d %d",
					noSlot, atEnd, shortTail, noMark, notFound, c.noSlot, c.atEnd, c.shortTail, c.noMark, c.notFound)
			}
		})
	}
}

// Claude Code sends the environment message as a string. It becomes the one text block it means,
// so it can carry the mark; the text is unchanged.
func TestAStringLastMessageTakesTheTailBreakpointAsATextBlock(t *testing.T) {
	out, ok := markTail(tailBody(`{"type":"ephemeral"}`, 5000))
	last := gjson.GetBytes(out, "messages.1.content")
	if !ok || !last.IsArray() || len(last.Array()) != 1 || last.Get("0.type").String() != "text" ||
		last.Get("0.text").String() != strings.Repeat("e", 5000) || !last.Get("0.cache_control").IsObject() {
		t.Fatalf("string content not turned into one marked text block:\n%s", last.Raw)
	}
}

// THE LOOKBACK. The mark is added even when more than 20 blocks follow the client's mark: the
// client's mark still reads its own entry, and the next turn reads the new one.
func TestTheTailBreakpointIsAddedPastTheLookback(t *testing.T) {
	var msgs []string
	for i := 0; i < lookbackBlocks+5; i++ {
		msgs = append(msgs, `{"role":"user","content":[{"type":"text","text":"`+strings.Repeat("x", 300)+`"}]}`)
	}
	body := `{"messages":[{"role":"user","content":[{"type":"text","text":"q","cache_control":{"type":"ephemeral"}}]},` +
		strings.Join(msgs, ",") + `]}`
	if blocksAfter(gjson.Get(body, "messages").Array(), "messages.0.content.0.cache_control") <= lookbackBlocks {
		t.Fatal("test body is not past the lookback")
	}
	out, ok := markTail([]byte(body))
	if !ok || !lastBlockMark(out).IsObject() {
		t.Fatalf("not added past the lookback:\n%s", out)
	}
}
