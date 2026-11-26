package inband

import (
	"fmt"
	"github.com/rossoctl/context-guru/internal/inbandtest"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/adjudicate"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/tooldefer"
	"github.com/tidwall/gjson"
)

// Responses events as the API streams them (#884). The shapes were checked against Codex 0.160.0
// and the OpenAI Python SDK 3.45.0 on a fake server (see the issue).

func rsSplicer() (*SSESplicer, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	return NewResponsesSSESplicer(rec, testCopyHeaders), rec
}

func rsPass(sp *SSESplicer, stream string) (whole, withheld []byte, called []string) {
	sp.Round(&http.Response{StatusCode: 200, Header: http.Header{}})
	return sp.Pass(strings.NewReader(stream), expand.ToolName, adjudicate.ToolName, tooldefer.ClientSearchName)
}

func rsTypes(out string) []string {
	var ts []string
	for _, blk := range strings.Split(out, "\n\n") {
		if p := sseEventPayload([]byte(blk)); p != "" {
			ts = append(ts, gjson.Get(p, "type").String())
		}
	}
	return ts
}

// Text only, or a client tool: the client gets the stream byte for byte.
func TestResponsesSpliceTextStreamsUntouched(t *testing.T) {
	for name, stream := range map[string]string{
		"text":       inbandtest.RSCreated("a") + inbandtest.RSText(0, "m1", "Hello") + inbandtest.RSCompleted("a", inbandtest.RSMsgItem("m1", "Hello")),
		"other tool": inbandtest.RSCreated("a") + inbandtest.RSText(0, "m1", "ls") + inbandtest.RSCall(1, "c1", "Bash") + inbandtest.RSCompleted("a", inbandtest.RSMsgItem("m1", "ls"), inbandtest.RSCallItem("c1", "Bash")),
	} {
		t.Run(name, func(t *testing.T) {
			sp, rec := rsSplicer()
			whole, withheld, called := rsPass(sp, stream)
			if withheld != nil || len(called) != 0 || string(whole) != stream {
				t.Fatalf("withheld=%q called=%v", withheld, called)
			}
			sp.Terminate(nil)
			if rec.Body.String() != stream {
				t.Fatalf("the client must get the stream byte for byte:\n got  %q\n want %q", rec.Body.String(), stream)
			}
		})
	}
}

// Our search call: the text before it streams at once, the call and the rest are withheld, and
// the follow-up is spliced in under round 1's id with ONE response.completed that lists every item.
func TestResponsesSpliceWithholdsOurCallAndMergesTheRounds(t *testing.T) {
	sp, rec := rsSplicer()
	pre := inbandtest.RSCreated("resp_A") + inbandtest.RSText(0, "m1", "Let me look.")
	held := inbandtest.RSCall(1, "c1", tooldefer.ClientSearchName) +
		inbandtest.RSCompleted("resp_A", inbandtest.RSMsgItem("m1", "Let me look."), inbandtest.RSCallItem("c1", tooldefer.ClientSearchName))
	_, withheld, called := rsPass(sp, pre+held)
	if string(withheld) != held || len(called) != 1 || called[0] != tooldefer.ClientSearchName {
		t.Fatalf("withheld=%q called=%v", withheld, called)
	}
	if rec.Body.String() != pre {
		t.Fatalf("the text before our call must stream at once, verbatim: %q", rec.Body.String())
	}
	if !sp.Opened() {
		t.Fatal("the client holds text: the response is not a buffered one")
	}
	r2 := inbandtest.RSEv("response.created", `"sequence_number":0,`+inbandtest.RSResp("resp_B", "in_progress", "[]")) +
		inbandtest.RSEv("response.in_progress", `"sequence_number":1,`+inbandtest.RSResp("resp_B", "in_progress", "[]")) +
		inbandtest.RSText(0, "m2", "Done.") +
		inbandtest.RSCompleted("resp_B", inbandtest.RSMsgItem("m2", "Done."))
	_, w2, c2 := rsPass(sp, r2)
	if w2 != nil || len(c2) != 0 {
		t.Fatalf("round 2 withheld %q", w2)
	}
	sp.Terminate(withheld)
	out := rec.Body.String()
	if strings.Contains(out, tooldefer.ClientSearchName) || strings.Contains(out, "resp_B") {
		t.Fatalf("the client sees our call or a second id: %q", out)
	}
	ts := rsTypes(out)
	count := map[string]int{}
	for _, ty := range ts {
		count[ty]++
	}
	if count["response.created"] != 1 || count["response.in_progress"] != 0 || count["response.completed"] != 1 {
		t.Fatalf("event types: %v", ts)
	}
	ds := inbandtest.RSDatas(out)
	last := ds[len(ds)-1]
	if last.Get("type").String() != "response.completed" || last.Get("response.id").String() != "resp_A" {
		t.Fatalf("last event: %s", last.Raw)
	}
	if got := last.Get("response.output.#").Int(); got != 2 ||
		last.Get("response.output.0.id").String() != "m1" || last.Get("response.output.1.id").String() != "m2" {
		t.Fatalf("output must hold both items in order: %s", last.Get("response.output").Raw)
	}
	if last.Get("response.usage.input_tokens").Int() != 10 || last.Get("response.usage.total_tokens").Int() != 16 {
		t.Fatalf("usage must sum both rounds: %s", last.Get("response.usage").Raw)
	}
	// Round 2's text item takes output_index 1, past the one the client holds.
	for _, d := range ds {
		if d.Get("item.id").String() == "m2" && d.Get("output_index").Int() != 1 {
			t.Fatalf("round 2 item not renumbered: %s", d.Raw)
		}
	}
	// Round 2's events continue the client's count: each is numbered by the events written so far.
	for i, d := range ds {
		if d.Get("item.id").String() == "m2" && d.Get("sequence_number").Int() < 4 {
			t.Fatalf("round 2 event %d not renumbered: %s", i, d.Raw)
		}
	}
	if !sp.ended {
		t.Fatal("the terminal event went out")
	}
}

// The follow-up's completed lists an item the stream never carried, or leaves one out: the merged
// output is built from what the client SAW plus the follow-up's own list.
func TestResponsesSpliceKeepsWhatTheClientSaw(t *testing.T) {
	sp, rec := rsSplicer()
	pre := inbandtest.RSCreated("resp_A") + inbandtest.RSText(0, "m1", "One.")
	held := inbandtest.RSCall(1, "c1", tooldefer.ClientSearchName) +
		inbandtest.RSCompleted("resp_A", inbandtest.RSCallItem("c1", tooldefer.ClientSearchName)) // completed omits m1
	_, withheld, _ := rsPass(sp, pre+held)
	rsPass(sp, inbandtest.RSCreated("resp_B")+inbandtest.RSText(0, "m2", "Two.")+inbandtest.RSCompleted("resp_B", inbandtest.RSMsgItem("m2", "Two."), inbandtest.RSMsgItem("m3", "only in completed")))
	sp.Terminate(withheld)
	ds := inbandtest.RSDatas(rec.Body.String())
	out := ds[len(ds)-1].Get("response.output")
	var ids []string
	out.ForEach(func(_, it gjson.Result) bool { ids = append(ids, it.Get("id").String()); return true })
	if strings.Join(ids, ",") != "m1,m2,m3" {
		t.Fatalf("output ids = %v", ids)
	}
}

// Our call as the first thing in the answer: the client holds only response.created, so the
// response counts as buffered.
func TestResponsesSpliceOpeningCallIsBuffered(t *testing.T) {
	sp, rec := rsSplicer()
	_, withheld, _ := rsPass(sp, inbandtest.RSCreated("a")+inbandtest.RSCall(0, "c1", tooldefer.ClientSearchName)+inbandtest.RSCompleted("a", inbandtest.RSCallItem("c1", tooldefer.ClientSearchName)))
	if withheld == nil || sp.Opened() || rec.Body.String() != inbandtest.RSCreated("a") {
		t.Fatalf("opened=%v out=%q", sp.Opened(), rec.Body.String())
	}
}

// The loop cannot answer after all: handBack gives the client the model's own stream, ending in a
// completed event that lists the call, as before the splice existed.
func TestResponsesSpliceHandBackIsRoundOneVerbatim(t *testing.T) {
	sp, rec := rsSplicer()
	pre := inbandtest.RSCreated("a") + inbandtest.RSText(0, "m1", "Hi")
	held := inbandtest.RSCall(1, "c1", tooldefer.ClientSearchName) + inbandtest.RSCompleted("a", inbandtest.RSMsgItem("m1", "Hi"), inbandtest.RSCallItem("c1", tooldefer.ClientSearchName))
	_, withheld, _ := rsPass(sp, pre+held)
	sp.HandBack(withheld)
	sp.Terminate(withheld)
	if rec.Body.String() != pre+held {
		t.Fatalf("got %q", rec.Body.String())
	}
}

// A follow-up cut off before its terminal event: the client gets a response.failed, not a socket
// that stops mid-answer.
func TestResponsesSpliceCutOffFollowUpFails(t *testing.T) {
	sp, rec := rsSplicer()
	pre := inbandtest.RSCreated("resp_A") + inbandtest.RSText(0, "m1", "Hi")
	held := inbandtest.RSCall(1, "c1", tooldefer.ClientSearchName) + inbandtest.RSCompleted("resp_A", inbandtest.RSMsgItem("m1", "Hi"), inbandtest.RSCallItem("c1", tooldefer.ClientSearchName))
	_, withheld, _ := rsPass(sp, pre+held)
	rsPass(sp, inbandtest.RSCreated("resp_B")+inbandtest.RSText(0, "m2", "Tw")[:60])
	sp.Terminate(withheld)
	out := rec.Body.String()
	if !strings.Contains(out, "event: response.failed\n") || strings.Contains(out, "resp_B") {
		t.Fatalf("out=%q", out)
	}
}

// One round, cut off: the same.
func TestResponsesSpliceOneRoundCutGetsFailed(t *testing.T) {
	sp, rec := rsSplicer()
	rsPass(sp, inbandtest.RSCreated("a")+inbandtest.RSText(0, "m1", "Hi"))
	sp.Terminate(nil)
	ts := rsTypes(rec.Body.String())
	if ts[len(ts)-1] != "response.failed" {
		t.Fatalf("types=%v", ts)
	}
}

func TestStatelessResponses(t *testing.T) {
	for body, want := range map[string]bool{
		`{"store":false,"input":[]}`: true,
		`{"input":[]}`:               false,
		`{"store":true}`:             false,
		`{"store":false,"previous_response_id":"resp_1"}`: false,
		`{"store":false,"previous_response_id":""}`:       true,
		`{"store":false,"previous_response_id":null}`:     true,
		`{"store":"false"}`:                               false,
	} {
		if got := StatelessResponses([]byte(body)); got != want {
			t.Errorf("%s: got %v want %v", body, got, want)
		}
	}
}

// A gateway that ends the stream with [DONE] after the terminal event gets nothing added.
func TestResponsesSpliceDoneIsATerminator(t *testing.T) {
	stream := inbandtest.RSCreated("a") + inbandtest.RSText(0, "m1", "Hi") + "data: [DONE]\n\n"
	sp, rec := rsSplicer()
	rsPass(sp, stream)
	sp.Terminate(nil)
	if rec.Body.String() != stream {
		t.Fatalf("got %q", rec.Body.String())
	}
}

// The merged usage sums the nested counts too, and is written when the last round has none.
func TestResponsesSpliceSumsUsage(t *testing.T) {
	u := func(in, cached, out, reason int) string {
		return fmt.Sprintf(`"usage":{"input_tokens":%d,"input_tokens_details":{"cached_tokens":%d},"output_tokens":%d,"output_tokens_details":{"reasoning_tokens":%d},"total_tokens":%d}`,
			in, cached, out, reason, in+out)
	}
	done := func(id, usage string, items ...string) string {
		return inbandtest.RSEv("response.completed", `"sequence_number":9,"response":{"id":"`+id+`","status":"completed","output":[`+strings.Join(items, ",")+`]`+usage+`}`)
	}
	for name, last := range map[string]string{
		"with usage": done("resp_B", `,`+u(20, 10, 4, 2), inbandtest.RSMsgItem("m2", "Two.")),
		"no usage":   done("resp_B", ``, inbandtest.RSMsgItem("m2", "Two.")),
	} {
		t.Run(name, func(t *testing.T) {
			sp, rec := rsSplicer()
			held := inbandtest.RSCall(1, "c1", tooldefer.ClientSearchName) +
				done("resp_A", `,`+u(100, 64, 6, 3), inbandtest.RSMsgItem("m1", "One."), inbandtest.RSCallItem("c1", tooldefer.ClientSearchName))
			_, withheld, _ := rsPass(sp, inbandtest.RSCreated("resp_A")+inbandtest.RSText(0, "m1", "One.")+held)
			rsPass(sp, inbandtest.RSCreated("resp_B")+inbandtest.RSText(0, "m2", "Two.")+last)
			sp.Terminate(withheld)
			ds := inbandtest.RSDatas(rec.Body.String())
			got := ds[len(ds)-1].Get("response.usage")
			want := map[string]int64{"input_tokens": 120, "output_tokens": 10, "total_tokens": 130,
				"input_tokens_details.cached_tokens": 74, "output_tokens_details.reasoning_tokens": 5}
			if name == "no usage" {
				want = map[string]int64{"input_tokens": 100, "output_tokens": 6, "total_tokens": 106,
					"input_tokens_details.cached_tokens": 64, "output_tokens_details.reasoning_tokens": 3}
			}
			for k, v := range want {
				if got.Get(k).Int() != v {
					t.Errorf("%s = %d, want %d (%s)", k, got.Get(k).Int(), v, got.Raw)
				}
			}
		})
	}
}

// conversation and item_reference hold history at the provider, so the request is not stateless (#1298).
func TestStatelessResponsesServerHeldHistoryForms(t *testing.T) {
	for _, body := range []string{
		`{"store":false,"conversation":"conv_1"}`,
		`{"store":false,"conversation":{"id":"conv_1"}}`,
		`{"store":false,"input":[{"type":"item_reference","id":"msg_1"}]}`,
	} {
		if StatelessResponses([]byte(body)) {
			t.Errorf("%s: reported stateless", body)
		}
	}
}

// The payload of an event comes from the shared SSE rules: CRLF, several data lines, [DONE] (#1499).
func TestSSEEventPayloadRules(t *testing.T) {
	for ev, want := range map[string]string{
		"event: x\r\ndata: {\"a\":1}\r\n\r\n": `{"a":1}`,
		"data: {\"a\":\ndata: 1}\n\n":         "{\"a\":\n1}",
		"data: [DONE]\n\n":                    "",
		"event: ping\n\n":                     "",
	} {
		if got := sseEventPayload([]byte(ev)); got != want {
			t.Errorf("%q: got %q, want %q", ev, got, want)
		}
	}
}
