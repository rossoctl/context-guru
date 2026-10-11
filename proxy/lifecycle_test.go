package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/dash"
)

// Two turns through the real handler with lifecycle signals on: the rows carry the previous
// response's tool name and count, the tool_result count/size, a thread id shared by both
// turns, the gap, and the granted TTL tier; and no tool input or result text is stored.
func TestLifecycleSignalsEndToEnd(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	h, rec := dashHandler(t, up.URL, dash.Options{LifecycleSignals: true})
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	send := func(body string) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-context-guru-session", "sess-life")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	first := string(anthropicRequest("aws/claude-sonnet-5"))
	send(first)
	waitForRows(t, rec, 1)
	// Second turn: the transcript grew by an assistant tool_use and an errored tool_result.
	second := strings.Replace(first, `],"model"`, `,{"role":"assistant","content":[{"type":"tool_use","id":"tu_2","name":"Edit","input":{"secret":"INPUT-TEXT"}}]},`+
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_2","is_error":true,"content":"RESULT-TEXT"}]}],"model"`, 1)
	send(second)
	waitForRows(t, rec, 2)

	got, err := rec.DB().Lifecycles()
	if err != nil || len(got) != 2 {
		t.Fatalf("lifecycle rows = %d, err %v", len(got), err)
	}
	a, b := got[0], got[1]
	if a.ThreadID == "" || a.ThreadID != b.ThreadID || a.ThreadSeq != 1 || b.ThreadSeq != 2 {
		t.Errorf("thread continuity: %+v / %+v", a, b)
	}
	if a.PrevToolNames != "bash" || a.PrevToolCount != 1 || a.ResultCount != 1 || a.ResultBytes < 1000 || a.ResultError || a.GapMs != -1 {
		t.Errorf("first row: %+v", a)
	}
	if b.PrevToolNames != "Edit" || b.PrevToolCount != 1 || b.ResultCount != 1 || !b.ResultError ||
		b.PrevStopReason != "end_turn" || b.GapMs < 0 || b.SincePrevEndMs < 0 || b.SincePrevEndMs > b.GapMs {
		t.Errorf("second row: %+v", b)
	}
	if a.GrantedTTL != "5m" { // the fake response wrote 1500 tokens with no 1h tier
		t.Errorf("granted ttl = %q", a.GrantedTTL)
	}
	for _, l := range got {
		for _, s := range []string{l.PrevToolNames, l.PrevStopReason, l.GrantedTTL, l.ThreadID} {
			if strings.Contains(s, "INPUT-TEXT") || strings.Contains(s, "RESULT-TEXT") {
				t.Errorf("content leaked into a lifecycle row: %q", s)
			}
		}
	}
}

func TestLifecycleOffWritesNoRows(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	h, rec := dashHandler(t, up.URL, dash.Options{})
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages", strings.NewReader(string(anthropicRequest("aws/claude-sonnet-5"))))
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	waitForRows(t, rec, 1)
	if got, _ := rec.DB().Lifecycles(); len(got) != 0 {
		t.Errorf("flag off wrote %d lifecycle rows", len(got))
	}
}
