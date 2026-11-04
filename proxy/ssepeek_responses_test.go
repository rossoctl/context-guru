package proxy

import (
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

// Responses events as the API streams them (#884). The shapes were checked against Codex 0.160.0
// and the OpenAI Python SDK 3.45.0 on a fake server (see the issue).

func rsEv(typ, body string) string {
	return "event: " + typ + "\ndata: {\"type\":\"" + typ + "\"," + body + "}\n\n"
}

func rsResp(id, status, output string) string {
	return `"response":{"id":"` + id + `","object":"response","status":"` + status + `","output":` + output +
		`,"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}`
}

func rsCreated(id string) string {
	return rsEv("response.created", `"sequence_number":0,`+rsResp(id, "in_progress", "[]"))
}

func rsMsgItem(id, text string) string {
	return fmt.Sprintf(`{"id":%q,"type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":%q}]}`, id, text)
}

func rsText(idx int, id, text string) string {
	return rsEv("response.output_item.added", fmt.Sprintf(`"sequence_number":1,"output_index":%d,"item":{"id":%q,"type":"message","status":"in_progress","role":"assistant","content":[]}`, idx, id)) +
		rsEv("response.output_text.delta", fmt.Sprintf(`"sequence_number":2,"output_index":%d,"item_id":%q,"content_index":0,"delta":%q`, idx, id, text)) +
		rsEv("response.output_item.done", fmt.Sprintf(`"sequence_number":3,"output_index":%d,"item":%s`, idx, rsMsgItem(id, text)))
}

func rsCallItem(id, name string) string {
	return fmt.Sprintf(`{"id":"fc_%s","type":"function_call","status":"completed","call_id":%q,"name":%q,"arguments":"{}"}`, id, id, name)
}

func rsCall(idx int, id, name string) string {
	return rsEv("response.output_item.added", fmt.Sprintf(`"sequence_number":4,"output_index":%d,"item":{"id":"fc_%s","type":"function_call","status":"in_progress","call_id":%q,"name":%q,"arguments":""}`, idx, id, id, name)) +
		rsEv("response.function_call_arguments.done", fmt.Sprintf(`"sequence_number":5,"output_index":%d,"item_id":"fc_%s","arguments":"{}"`, idx, id)) +
		rsEv("response.output_item.done", fmt.Sprintf(`"sequence_number":6,"output_index":%d,"item":%s`, idx, rsCallItem(id, name)))
}

func rsCompleted(id string, items ...string) string {
	return rsEv("response.completed", `"sequence_number":9,`+rsResp(id, "completed", "["+strings.Join(items, ",")+"]"))
}

func rsSplicer() (*sseSplicer, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	return newResponsesSSESplicer(rec), rec
}

func rsPass(sp *sseSplicer, stream string) (whole, withheld []byte, called []string) {
	sp.round(&http.Response{StatusCode: 200, Header: http.Header{}})
	return sp.pass(strings.NewReader(stream), expand.ToolName, adjudicate.ToolName, tooldefer.ClientSearchName)
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

func rsDatas(out string) []gjson.Result {
	var ds []gjson.Result
	for _, blk := range strings.Split(out, "\n\n") {
		if p := sseEventPayload([]byte(blk)); p != "" {
			ds = append(ds, gjson.Parse(p))
		}
	}
	return ds
}

// Text only, or a client tool: the client gets the stream byte for byte.
func TestResponsesSpliceTextStreamsUntouched(t *testing.T) {
	for name, stream := range map[string]string{
		"text":       rsCreated("a") + rsText(0, "m1", "Hello") + rsCompleted("a", rsMsgItem("m1", "Hello")),
		"other tool": rsCreated("a") + rsText(0, "m1", "ls") + rsCall(1, "c1", "Bash") + rsCompleted("a", rsMsgItem("m1", "ls"), rsCallItem("c1", "Bash")),
	} {
		t.Run(name, func(t *testing.T) {
			sp, rec := rsSplicer()
			whole, withheld, called := rsPass(sp, stream)
			if withheld != nil || len(called) != 0 || string(whole) != stream {
				t.Fatalf("withheld=%q called=%v", withheld, called)
			}
			sp.terminate(nil)
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
	pre := rsCreated("resp_A") + rsText(0, "m1", "Let me look.")
	held := rsCall(1, "c1", tooldefer.ClientSearchName) +
		rsCompleted("resp_A", rsMsgItem("m1", "Let me look."), rsCallItem("c1", tooldefer.ClientSearchName))
	_, withheld, called := rsPass(sp, pre+held)
	if string(withheld) != held || len(called) != 1 || called[0] != tooldefer.ClientSearchName {
		t.Fatalf("withheld=%q called=%v", withheld, called)
	}
	if rec.Body.String() != pre {
		t.Fatalf("the text before our call must stream at once, verbatim: %q", rec.Body.String())
	}
	if !sp.opened() {
		t.Fatal("the client holds text: the response is not a buffered one")
	}
	r2 := rsEv("response.created", `"sequence_number":0,`+rsResp("resp_B", "in_progress", "[]")) +
		rsEv("response.in_progress", `"sequence_number":1,`+rsResp("resp_B", "in_progress", "[]")) +
		rsText(0, "m2", "Done.") +
		rsCompleted("resp_B", rsMsgItem("m2", "Done."))
	_, w2, c2 := rsPass(sp, r2)
	if w2 != nil || len(c2) != 0 {
		t.Fatalf("round 2 withheld %q", w2)
	}
	sp.terminate(withheld)
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
	ds := rsDatas(out)
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
	pre := rsCreated("resp_A") + rsText(0, "m1", "One.")
	held := rsCall(1, "c1", tooldefer.ClientSearchName) +
		rsCompleted("resp_A", rsCallItem("c1", tooldefer.ClientSearchName)) // completed omits m1
	_, withheld, _ := rsPass(sp, pre+held)
	rsPass(sp, rsCreated("resp_B")+rsText(0, "m2", "Two.")+rsCompleted("resp_B", rsMsgItem("m2", "Two."), rsMsgItem("m3", "only in completed")))
	sp.terminate(withheld)
	ds := rsDatas(rec.Body.String())
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
	_, withheld, _ := rsPass(sp, rsCreated("a")+rsCall(0, "c1", tooldefer.ClientSearchName)+rsCompleted("a", rsCallItem("c1", tooldefer.ClientSearchName)))
	if withheld == nil || sp.opened() || rec.Body.String() != rsCreated("a") {
		t.Fatalf("opened=%v out=%q", sp.opened(), rec.Body.String())
	}
}

// The loop cannot answer after all: handBack gives the client the model's own stream, ending in a
// completed event that lists the call, as before the splice existed.
func TestResponsesSpliceHandBackIsRoundOneVerbatim(t *testing.T) {
	sp, rec := rsSplicer()
	pre := rsCreated("a") + rsText(0, "m1", "Hi")
	held := rsCall(1, "c1", tooldefer.ClientSearchName) + rsCompleted("a", rsMsgItem("m1", "Hi"), rsCallItem("c1", tooldefer.ClientSearchName))
	_, withheld, _ := rsPass(sp, pre+held)
	sp.handBack(withheld)
	sp.terminate(withheld)
	if rec.Body.String() != pre+held {
		t.Fatalf("got %q", rec.Body.String())
	}
}

// A follow-up cut off before its terminal event: the client gets a response.failed, not a socket
// that stops mid-answer.
func TestResponsesSpliceCutOffFollowUpFails(t *testing.T) {
	sp, rec := rsSplicer()
	pre := rsCreated("resp_A") + rsText(0, "m1", "Hi")
	held := rsCall(1, "c1", tooldefer.ClientSearchName) + rsCompleted("resp_A", rsMsgItem("m1", "Hi"), rsCallItem("c1", tooldefer.ClientSearchName))
	_, withheld, _ := rsPass(sp, pre+held)
	rsPass(sp, rsCreated("resp_B")+rsText(0, "m2", "Tw")[:60])
	sp.terminate(withheld)
	out := rec.Body.String()
	if !strings.Contains(out, "event: response.failed\n") || strings.Contains(out, "resp_B") {
		t.Fatalf("out=%q", out)
	}
}

// One round, cut off: the same.
func TestResponsesSpliceOneRoundCutGetsFailed(t *testing.T) {
	sp, rec := rsSplicer()
	rsPass(sp, rsCreated("a")+rsText(0, "m1", "Hi"))
	sp.terminate(nil)
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
		`{"store":"false"}`: false,
	} {
		if got := statelessResponses([]byte(body)); got != want {
			t.Errorf("%s: got %v want %v", body, got, want)
		}
	}
}

// End to end (#884): a stateless Responses request that declares our search streams its text at
// once, the search is answered in band, and the client reads ONE answer under round 1's id.
// A request with store left at the default keeps the buffering and sees the same single answer.
// (A chained request is told apart by statelessResponses, tested above.)
func TestResponsesSearchStreamsEndToEnd(t *testing.T) {
	respDecl := func(name string) string {
		return fmt.Sprintf(`{"type":"function","name":%q,"description":"Does the %s job. %s","parameters":{"type":"object"}}`,
			name, name, strings.Repeat("Detailed usage notes for this tool. ", 60))
	}
	body := func(extra string) []byte {
		return []byte(`{"model":"gpt-5.5","stream":true,"instructions":"agent","tools":[` + respDecl("Bash") + `,` + respDecl("Read") + `,` +
			respDecl("Workflow") + `,` + respDecl("Monitor") + `],` + extra + `"input":[{"role":"user","content":"go"}]}`)
	}
	r1 := rsCreated("resp_A") + rsText(0, "m1", "Let me look.") +
		rsEv("response.output_item.added", `"sequence_number":4,"output_index":1,"item":{"id":"fc_c1","type":"function_call","status":"in_progress","call_id":"c1","name":"`+tooldefer.ClientSearchName+`","arguments":""}`) +
		rsEv("response.output_item.done", `"sequence_number":5,"output_index":1,"item":{"id":"fc_c1","type":"function_call","status":"completed","call_id":"c1","name":"`+tooldefer.ClientSearchName+`","arguments":"{\"names\":[\"Workflow\"]}"}`) +
		rsCompleted("resp_A", rsMsgItem("m1", "Let me look."), `{"id":"fc_c1","type":"function_call","status":"completed","call_id":"c1","name":"`+tooldefer.ClientSearchName+`","arguments":"{\"names\":[\"Workflow\"]}"}`)
	r2 := rsCreated("resp_B") + rsText(0, "m2", "Done.") + rsCompleted("resp_B", rsMsgItem("m2", "Done."))
	for _, tc := range []struct {
		name, extra string
		spliced     bool
	}{
		{"stateless", `"store":false,`, true},
		{"store default", ``, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := &csUpstream{script: []string{r1, r2}}
			us := httptest.NewServer(up)
			defer us.Close()
			h := csHandler(t, "ultrahigh", us.URL)
			ds := h.toolDefer()
			for i, s := range []string{"r1", "r2", "r3"} {
				ds.hist.Observe("", s, "", tooldefer.View("responses", body("")), int64(i+10))
			}
			srv := httptest.NewServer(h.Mux())
			defer srv.Close()
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/openai/v1/responses", strings.NewReader(string(body(tc.extra))))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer sk-test")
			req.Header.Set("x-context-guru-session", "rs1")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			out := string(raw)
			if len(up.all()) != 2 || strings.Contains(out, tooldefer.ClientSearchName) {
				t.Fatalf("rounds=%d out=%s", len(up.all()), out)
			}
			if tc.spliced && !strings.HasPrefix(out, rsCreated("resp_A")+rsText(0, "m1", "Let me look.")) {
				t.Fatalf("the text must reach the client verbatim: %q", out)
			}
			if !tc.spliced && !strings.Contains(out, "resp_B") {
				t.Fatalf("a request that may chain keeps the buffering and gets the follow-up whole: %q", out)
			}
			if tc.spliced && strings.Contains(out, "resp_B") {
				t.Fatalf("a stateless client sees round 1's id only: %q", out)
			}
			ds2 := rsDatas(out)
			last := ds2[len(ds2)-1]
			if last.Get("type").String() != "response.completed" {
				t.Fatalf("last event: %s", last.Raw)
			}
			if tc.spliced && (last.Get("response.id").String() != "resp_A" || last.Get("response.output.#").Int() != 2) {
				t.Fatalf("merged completed: %s", last.Raw)
			}
		})
	}
}
