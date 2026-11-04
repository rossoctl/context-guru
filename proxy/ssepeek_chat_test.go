package proxy

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/adjudicate"
	"github.com/rossoctl/context-guru/tooldefer"
	"github.com/tidwall/gjson"
)

// Chat Completions chunks as gpt-5.5 streams them through the gateway (#879): a role chunk, then
// content or reasoning deltas, then tool calls whose name arrives on the first chunk of each index.

func ccChunk(id, delta, finish string) string {
	f := "null"
	if finish != "" {
		f = fmt.Sprintf("%q", finish)
	}
	return `data: {"id":"` + id + `","object":"chat.completion.chunk","model":"gpt-5.5","choices":[{"index":0,"delta":` +
		delta + `,"finish_reason":` + f + `}]}` + "\n\n"
}

func ccRole(id string) string { return ccChunk(id, `{"role":"assistant","content":""}`, "") }
func ccText(id, s string) string {
	return ccChunk(id, `{"content":"`+s+`"}`, "")
}
func ccReason(id, s string) string { return ccChunk(id, `{"reasoning_content":"`+s+`"}`, "") }
func ccCallStart(id string, idx int, callID, name string) string {
	return ccChunk(id, fmt.Sprintf(`{"tool_calls":[{"index":%d,"id":%q,"type":"function","function":{"name":%q,"arguments":""}}]}`,
		idx, callID, name), "")
}
func ccCallArgs(id string, idx int, args string) string {
	return ccChunk(id, fmt.Sprintf(`{"tool_calls":[{"index":%d,"function":{"arguments":%q}}]}`, idx, args), "")
}
func ccFinish(id, why string) string { return ccChunk(id, `{}`, why) }
func ccUsage(id string) string {
	return `data: {"id":"` + id + `","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":5}}` + "\n\n"
}

const ccDone = "data: [DONE]\n\n"

func ccSplicer() (*sseSplicer, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	sp := newChatSSESplicer(rec)
	return sp, rec
}

var ccProxyTools = []string{adjudicate.ToolName, tooldefer.ClientSearchName}

func ccPass(sp *sseSplicer, stream string) (whole, withheld []byte, called []string) {
	sp.round(&http.Response{StatusCode: 200, Header: http.Header{}})
	return sp.pass(strings.NewReader(stream), expand.ToolName, ccProxyTools...)
}

// Text only: the client gets the stream byte for byte, and nothing is withheld.
func TestChatSpliceTextStreamsUntouched(t *testing.T) {
	for _, tc := range []struct{ name, stream string }{
		{"text", ccRole("a") + ccText("a", "Hel") + ccText("a", "lo") + ccFinish("a", "stop") + ccUsage("a") + ccDone},
		{"reasoning then text", ccRole("a") + ccReason("a", "think") + ccText("a", "Hi") + ccFinish("a", "stop") + ccDone},
		{"another tool", ccRole("a") + ccText("a", "ls") + ccCallStart("a", 0, "c1", "Bash") +
			ccCallArgs("a", 0, `{"x":"ls"}`) + ccFinish("a", "tool_calls") + ccDone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp, rec := ccSplicer()
			whole, withheld, called := ccPass(sp, tc.stream)
			if withheld != nil || len(called) != 0 || string(whole) != tc.stream {
				t.Fatalf("withheld=%q called=%v", withheld, called)
			}
			sp.terminate(nil)
			if got := rec.Body.String(); got != tc.stream {
				t.Fatalf("the client must get the stream byte for byte:\n got  %q\n want %q", got, tc.stream)
			}
			if !sp.ended {
				t.Fatal("the stream ended; the splicer must know")
			}
		})
	}
}

// Our search call: everything before it streams at once; the call and the rest are withheld. The
// follow-up round goes out as the rest of the same stream, with round 1's id.
func TestChatSpliceWithholdsOurCallAndSplicesTheFollowUp(t *testing.T) {
	sp, rec := ccSplicer()
	pre := ccRole("r1") + ccReason("r1", "need a tool") + ccText("r1", "Let me look.")
	held := ccCallStart("r1", 0, "c1", tooldefer.ClientSearchName) + ccCallArgs("r1", 0, `{"names":["Workflow"]}`) +
		ccFinish("r1", "tool_calls") + ccUsage("r1") + ccDone
	whole, withheld, called := ccPass(sp, pre+held)
	if string(whole) != pre+held || string(withheld) != held {
		t.Fatalf("withheld = %q", withheld)
	}
	if len(called) != 1 || called[0] != tooldefer.ClientSearchName {
		t.Fatalf("called = %v", called)
	}
	if rec.Body.String() != pre {
		t.Fatalf("the text before our call must stream at once, verbatim: %q", rec.Body.String())
	}
	if !sp.opened() {
		t.Fatal("the client holds text: the response is not a buffered one")
	}
	// The follow-up calls the loaded tool.
	r2 := ccRole("r2") + ccCallStart("r2", 0, "c2", "Workflow") + ccCallArgs("r2", 0, `{"x":"go"}`) +
		ccFinish("r2", "tool_calls") + ccUsage("r2") + ccDone
	_, w2, c2 := ccPass(sp, r2)
	if w2 != nil || len(c2) != 0 {
		t.Fatalf("round 2 withheld %q", w2)
	}
	sp.terminate(withheld)
	out := rec.Body.String()
	if strings.Contains(out, tooldefer.ClientSearchName) || strings.Contains(out, `"id":"r2"`) {
		t.Fatalf("the client sees our call or a second id: %q", out)
	}
	if strings.Count(out, ccDone) != 1 {
		t.Fatalf("one [DONE]: %q", out)
	}
	msg, ok := expand.AggregateSSE("openai", []byte(out))
	if !ok {
		t.Fatalf("the client stream does not rebuild: %q", out)
	}
	m := gjson.ParseBytes(msg)
	if m.Get("id").String() != "r1" || m.Get("choices.0.message.content").String() != "Let me look." ||
		m.Get("choices.0.message.tool_calls.#").Int() != 1 ||
		m.Get("choices.0.message.tool_calls.0.function.name").String() != "Workflow" ||
		m.Get("choices.0.message.tool_calls.0.function.arguments").String() != `{"x":"go"}` {
		t.Fatalf("the client's answer: %s", msg)
	}
}

// Our call as the first thing in the answer: the client holds only the role chunk, so the
// response counts as buffered.
func TestChatSpliceOpeningCallIsBuffered(t *testing.T) {
	sp, rec := ccSplicer()
	_, withheld, _ := ccPass(sp, ccRole("a")+ccCallStart("a", 0, "c1", tooldefer.ClientSearchName)+ccFinish("a", "tool_calls")+ccDone)
	if withheld == nil || sp.opened() || rec.Body.String() != ccRole("a") {
		t.Fatalf("opened=%v body=%q", sp.opened(), rec.Body.String())
	}
}

// A mixed response: the client's call streams as it arrives, ours is withheld from its first
// chunk, and the hand-back gives the client the rest exactly as it came.
func TestChatSpliceMixedResponseHandsBackVerbatim(t *testing.T) {
	sp, rec := ccSplicer()
	stream := ccRole("a") + ccCallStart("a", 0, "c1", "Bash") + ccCallArgs("a", 0, `{"x":"ls"}`) +
		ccCallStart("a", 1, "c2", tooldefer.ClientSearchName) + ccCallArgs("a", 1, `{"names":["Monitor"]}`) +
		ccFinish("a", "tool_calls") + ccDone
	_, withheld, called := ccPass(sp, stream)
	if !strings.Contains(rec.Body.String(), `"Bash"`) || strings.Contains(rec.Body.String(), tooldefer.ClientSearchName) {
		t.Fatalf("before the hand-back: %q", rec.Body.String())
	}
	if len(called) != 1 {
		t.Fatalf("called = %v", called)
	}
	sp.handBack(withheld)
	sp.terminate(withheld)
	if got := rec.Body.String(); got != stream {
		t.Fatalf("prefix + hand-back must rebuild the stream:\n got  %q\n want %q", got, stream)
	}
}

// A later round's tool-call indexes follow the ones the client holds, and its id becomes round 1's.
func TestChatSpliceRenumbersALaterRound(t *testing.T) {
	sp, rec := ccSplicer()
	ccPass(sp, ccRole("a")+ccCallStart("a", 0, "c1", "Bash")+ccCallArgs("a", 0, `{}`)+
		ccCallStart("a", 1, "c2", tooldefer.ClientSearchName)+ccFinish("a", "tool_calls")+ccDone)
	if sp.blocks != 1 {
		t.Fatalf("the client holds 1 call, splicer says %d", sp.blocks)
	}
	prefix := rec.Body.String()
	ccPass(sp, ccRole("b")+ccCallStart("b", 0, "c3", "Read")+ccCallArgs("b", 0, `{"p":1}`)+ccFinish("b", "tool_calls")+ccDone)
	spliced := strings.TrimPrefix(rec.Body.String(), prefix)
	if strings.Contains(spliced, `"index":0,"id"`) || !strings.Contains(spliced, `"tool_calls":[{"index":1,"id":"c3"`) ||
		!strings.Contains(spliced, `"tool_calls":[{"index":1,"function"`) {
		t.Fatalf("round 2's call must be index 1: %q", spliced)
	}
	if strings.Contains(spliced, `"id":"b"`) {
		t.Fatalf("round 2 must carry round 1's id: %q", spliced)
	}
	m, ok := expand.AggregateSSE("openai", rec.Body.Bytes())
	if !ok || gjson.GetBytes(m, "choices.0.message.tool_calls.#").Int() != 2 {
		t.Fatalf("the client must read 2 calls: %s", m)
	}
}

// A chunk that does not parse goes as it came, and does not hide a later call of ours.
func TestChatSpliceMalformedChunkFailsOpen(t *testing.T) {
	sp, rec := ccSplicer()
	bad := "data: {\"id\":\"a\",\"choices\":[{\"delta\":{\"content\":\"x\"\n\n"
	pre := ccRole("a") + bad + ccText("a", "ok")
	held := ccCallStart("a", 0, "c1", tooldefer.ClientSearchName) + ccFinish("a", "tool_calls") + ccDone
	_, withheld, _ := ccPass(sp, pre+held)
	if rec.Body.String() != pre || string(withheld) != held {
		t.Fatalf("body=%q withheld=%q", rec.Body.String(), withheld)
	}
	// The serve loop cannot rebuild a stream like this, so it hands back: no byte is lost.
	sp.handBack(withheld)
	if rec.Body.String() != pre+held {
		t.Fatalf("lost bytes: %q", rec.Body.String())
	}
}

// Truncated in both rounds: the client gets an error chunk, not a stream that stops mid-answer.
func TestChatSpliceTerminatesATruncatedTurn(t *testing.T) {
	sp, rec := ccSplicer()
	_, withheld, _ := ccPass(sp, ccRole("a")+ccText("a", "hi")+ccCallStart("a", 0, "c1", tooldefer.ClientSearchName))
	ccPass(sp, ccRole("b")+ccText("b", "more"))
	sp.terminate(withheld)
	if !strings.HasSuffix(rec.Body.String(), "without a terminator\"}}\n\n") {
		t.Fatalf("no error chunk: %q", rec.Body.String())
	}
}

// Past the retain bound nothing is withheld and nothing is dropped, on Chat as on Anthropic.
func TestChatSplicePastTheRetainBound(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(ccRole("a"))
	sb.WriteString(ccCallStart("a", 0, "c1", tooldefer.ClientSearchName))
	for sb.Len() <= sseRetainMaxBytes+1024 {
		sb.WriteString(ccCallArgs("a", 0, strings.Repeat("x", 200)))
	}
	sb.WriteString(ccFinish("a", "tool_calls") + ccDone)
	stream := sb.String()
	sp, rec := ccSplicer()
	whole, withheld, called := ccPass(sp, stream)
	if whole != nil || withheld != nil || len(called) != 1 {
		t.Fatalf("whole=%d withheld=%d called=%v", len(whole), len(withheld), called)
	}
	if rec.Body.String() != stream {
		t.Fatalf("the client must get the stream byte for byte: %d B of %d", rec.Body.Len(), len(stream))
	}
}

// Detection reads the call's first chunk only: an arguments chunk or text naming our tool is not a
// call.
func TestChatProxyToolCallName(t *testing.T) {
	for _, tc := range []struct {
		ev, want string
	}{
		{ccCallStart("a", 0, "c", tooldefer.ClientSearchName), tooldefer.ClientSearchName},
		{ccCallStart("a", 2, "c", adjudicate.ToolName), adjudicate.ToolName},
		{ccCallStart("a", 0, "c", "Bash"), ""},
		{ccCallArgs("a", 0, `{"q":"`+tooldefer.ClientSearchName+`"}`), ""},
		{ccText("a", tooldefer.ClientSearchName), ""},
		{ccDone, ""},
		{pkExpand(), ""}, // an Anthropic event
	} {
		if got := chatProxyToolCallName([]byte(tc.ev), ccProxyTools); got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.ev, got, tc.want)
		}
	}
}

// errAfter is a body that the upstream breaks off after its bytes, with a read error, not an EOF.
type errAfter struct{ r io.Reader }

func (e errAfter) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err == io.EOF {
		return n, errors.New("connection reset by peer")
	}
	return n, err
}

// One round, no call of ours, and the upstream cuts the answer off: the client gets an error chunk
// after the bytes it got, as on Anthropic, and not a stream that just stops.
func TestChatSpliceOneRoundCutGetsAnErrorChunk(t *testing.T) {
	const errChunk = "data: {\"error\":{\"type\":\"api_error\",\"message\":\"upstream ended the turn without a terminator\"}}\n\n"
	for _, tc := range []struct {
		name string
		body func(string) io.Reader
		sent string
	}{
		// Mid-event: the last chunk has no blank line after it. It still goes to the client.
		{"mid-event", func(s string) io.Reader { return strings.NewReader(s) }, ccRole("a") + ccText("a", "Hel") + `data: {"id":"a","choi`},
		// Between events, on a read error that is not an EOF.
		{"read error", func(s string) io.Reader { return errAfter{strings.NewReader(s)} }, ccRole("a") + ccText("a", "Hel")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp, rec := ccSplicer()
			sp.round(&http.Response{StatusCode: 200, Header: http.Header{}})
			_, withheld, _ := sp.pass(tc.body(tc.sent), expand.ToolName, ccProxyTools...)
			if withheld != nil {
				t.Fatalf("withheld = %q", withheld)
			}
			sp.terminate(withheld)
			if got := rec.Body.String(); got != tc.sent+errChunk {
				t.Fatalf("the client must get its bytes and then an error chunk:\n got  %q\n want %q", got, tc.sent+errChunk)
			}
		})
	}
}

// One round that ends a complete answer cleanly, with no [DONE] and no finish_reason: some gateways
// do that. The client gets the upstream's bytes exactly, with nothing added.
func TestChatSpliceOneRoundCleanCloseStaysExact(t *testing.T) {
	stream := ccRole("a") + ccText("a", "Hello")
	sp, rec := ccSplicer()
	_, withheld, _ := ccPass(sp, stream)
	sp.terminate(withheld)
	if got := rec.Body.String(); got != stream {
		t.Fatalf("the client must get the stream byte for byte:\n got  %q\n want %q", got, stream)
	}
}
