package inband

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
)

const reqBody = `{"model":"claude","max_tokens":10,"messages":[{"role":"user","content":"look at <<cg:HASH>>"}],"tools":[{"name":"Bash","input_schema":{"type":"object"}}]}`

func msg(blocks ...string) []byte {
	return []byte(`{"role":"assistant","content":[` + strings.Join(blocks, ",") + `]}`)
}

func expandUse(id, hash string) string {
	return `{"type":"tool_use","id":"` + id + `","name":"` + expand.ToolName + `","input":{"id":"` + hash + `"}}`
}

func turn(st store.Store, m []byte, s Sinks) Turn {
	return Turn{Wire: "anthropic", Body: []byte(reqBody), Msg: m, Store: st, Session: "sess", ThreadID: "t", Sinks: s}
}

func TestAnswerResolvesAnExpandCall(t *testing.T) {
	st := store.NewMemory(store.Options{})
	st.Put("HASH", []byte("THE ORIGINAL"))
	var noted, bounced int
	res := Answer(turn(st, msg(expandUse("tu1", "HASH")), Sinks{
		Bounce:     func(n int) { bounced += n },
		NoteExpand: func(n int) { noted += n },
	}))
	if res.Bail || res.Next == nil || res.Calls != 1 || res.Resolved != 1 || !res.Reached {
		t.Fatalf("result %+v", res)
	}
	if !strings.Contains(string(res.Next), "THE ORIGINAL") {
		t.Fatalf("the follow-up lacks the original: %s", res.Next)
	}
	if noted == 0 || noted != bounced {
		t.Fatalf("noted %d, bounced %d: both must count the restored tokens", noted, bounced)
	}
	if !offload.KeptVerbatim(st, "sess", "THE ORIGINAL") {
		t.Fatal("the restored original is not kept verbatim")
	}
}

func TestAnswerContinuesWithAPlaceholderForAnUnknownID(t *testing.T) {
	st := store.NewMemory(store.Options{})
	res := Answer(turn(st, msg(expandUse("tu1", "GONE")), Sinks{}))
	if res.Bail || res.Resolved != 0 || res.Calls != 1 {
		t.Fatalf("result %+v", res)
	}
	if !strings.Contains(string(res.Next), expand.Unavailable("GONE")) {
		t.Fatalf("no placeholder in %s", res.Next)
	}
}

func TestAnswerBailsOnAClientToolOrNoCall(t *testing.T) {
	st := store.NewMemory(store.Options{})
	st.Put("HASH", []byte("x"))
	for name, m := range map[string][]byte{
		"client tool beside ours": msg(expandUse("tu1", "HASH"), `{"type":"tool_use","id":"tu2","name":"Bash","input":{}}`),
		"text only":               msg(`{"type":"text","text":"hi"}`),
	} {
		res := Answer(turn(st, m, Sinks{}))
		if !res.Bail || res.Next != nil {
			t.Errorf("%s: result %+v, want a bail", name, res)
		}
	}
}

func TestRepairExpandErrorsChargesTheRowOnce(t *testing.T) {
	st := store.NewMemory(store.Options{})
	st.Put("HASH", []byte("THE ORIGINAL"))
	body := []byte(`{"model":"claude","messages":[` +
		`{"role":"user","content":"look at <<cg:HASH>>"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"` + expand.ToolName + `","input":{"id":"HASH"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"No such tool available: ` + expand.ToolName + `","is_error":true}]}]}`)
	var noted, bounced int
	s := Sinks{NoteExpand: func(int) { noted++ }, Bounce: func(int) { bounced++ }}
	for i := 0; i < 3; i++ {
		out, n := RepairExpandErrors("anthropic", body, st, func([]byte) string { return "sess" }, s)
		if n != 1 || gjson.GetBytes(out, "messages.2.content.0.content").String() != "THE ORIGINAL" {
			t.Fatalf("repair %d: n=%d out=%s", i, n, out)
		}
	}
	if noted != 1 || bounced != 0 {
		t.Fatalf("row charged %d times, bounced %d; want 1 and 0", noted, bounced)
	}
}

func TestRetryRefusedSendsTheOriginalOnce(t *testing.T) {
	refused := &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"error":"no"}`))}
	var sent [][]byte
	var booked []int
	resp, err, body, retried := RetryRefused(refused, nil, []byte("deferred"), Refused{
		Orig: []byte("orig"), Search: true,
		Do: func(b []byte) (*http.Response, error) {
			sent = append(sent, b)
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
		},
		Book: func(status int, _ []byte, ok bool) {
			if ok {
				booked = append(booked, status)
			}
		},
	})
	if !retried || err != nil || resp.StatusCode != 200 || string(body) != "orig" || len(sent) != 1 || len(booked) != 1 || booked[0] != 400 {
		t.Fatalf("retried=%v err=%v resp=%v body=%s sent=%d booked=%v", retried, err, resp, body, len(sent), booked)
	}
}

func TestRetryRefusedLeavesOtherResponsesAlone(t *testing.T) {
	ok := &http.Response{StatusCode: 200}
	boom := errors.New("boom")
	for name, c := range map[string]struct {
		resp *http.Response
		err  error
	}{"200": {ok, nil}, "error": {nil, boom}, "500": {&http.Response{StatusCode: 500}, nil}} {
		resp, err, body, retried := RetryRefused(c.resp, c.err, []byte("b"), Refused{
			Orig: []byte("o"), Do: func([]byte) (*http.Response, error) { t.Fatal("sent"); return nil, nil },
		})
		if retried || resp != c.resp || err != c.err || string(body) != "b" {
			t.Errorf("%s: retried=%v body=%s", name, retried, body)
		}
	}
}

// The sweep's expand sink gets the size of the restored text in bytes (#1533), so the dashboard can convert it with
// the request's own ratio: the host's token count alone cannot be.
func TestRepairExpandErrorsReportsTheRestoredBytes(t *testing.T) {
	st := store.NewMemory(store.Options{})
	st.Put("HASH", []byte("THE ORIGINAL"))
	body := []byte(`{"model":"claude","messages":[` +
		`{"role":"user","content":"look at <<cg:HASH>>"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"` + expand.ToolName + `","input":{"id":"HASH"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"No such tool available: ` + expand.ToolName + `","is_error":true}]}]}`)
	var got []int
	s := Sinks{NoteSweepExpand: func(_ int, n offload.SweepExpandNote) { got = append(got, n.Bytes) }}
	RepairExpandErrors("anthropic", body, st, func([]byte) string { return "sess" }, s)
	if len(got) != 1 || got[0] != len("THE ORIGINAL") {
		t.Fatalf("restored bytes = %v, want [%d]", got, len("THE ORIGINAL"))
	}
}
