package proxy

import (
	"bytes"
	"fmt"
	"github.com/rossoctl/context-guru/internal/inbandtest"
	"github.com/rossoctl/context-guru/tooldefer"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// End to end (#884): a stateless Responses request that declares our search streams its text at
// once, the search is answered in band, and the client reads ONE answer under round 1's id.
// A request with store left at the default keeps the buffering and sees the same single answer.
// (A chained request is told apart by StatelessResponses, tested above.)
func TestResponsesSearchStreamsEndToEnd(t *testing.T) {
	respDecl := func(name string) string {
		return fmt.Sprintf(`{"type":"function","name":%q,"description":"Does the %s job. %s","parameters":{"type":"object"}}`,
			name, name, strings.Repeat("Detailed usage notes for this tool. ", 60))
	}
	body := func(extra string) []byte {
		return []byte(`{"model":"gpt-5.5","stream":true,"instructions":"agent","tools":[` + respDecl("Bash") + `,` + respDecl("Read") + `,` +
			respDecl("Workflow") + `,` + respDecl("Monitor") + `],` + extra + `"input":[{"role":"user","content":"go"}]}`)
	}
	r1 := inbandtest.RSCreated("resp_A") + inbandtest.RSText(0, "m1", "Let me look.") +
		inbandtest.RSEv("response.output_item.added", `"sequence_number":4,"output_index":1,"item":{"id":"fc_c1","type":"function_call","status":"in_progress","call_id":"c1","name":"`+tooldefer.ClientSearchName+`","arguments":""}`) +
		inbandtest.RSEv("response.output_item.done", `"sequence_number":5,"output_index":1,"item":{"id":"fc_c1","type":"function_call","status":"completed","call_id":"c1","name":"`+tooldefer.ClientSearchName+`","arguments":"{\"names\":[\"Workflow\"]}"}`) +
		inbandtest.RSCompleted("resp_A", inbandtest.RSMsgItem("m1", "Let me look."), `{"id":"fc_c1","type":"function_call","status":"completed","call_id":"c1","name":"`+tooldefer.ClientSearchName+`","arguments":"{\"names\":[\"Workflow\"]}"}`)
	r2 := inbandtest.RSCreated("resp_B") + inbandtest.RSText(0, "m2", "Done.") + inbandtest.RSCompleted("resp_B", inbandtest.RSMsgItem("m2", "Done."))
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
			if tc.spliced && !strings.HasPrefix(out, inbandtest.RSCreated("resp_A")+inbandtest.RSText(0, "m1", "Let me look.")) {
				t.Fatalf("the text must reach the client verbatim: %q", out)
			}
			if !tc.spliced && !strings.Contains(out, "resp_B") {
				t.Fatalf("a request that may chain keeps the buffering and gets the follow-up whole: %q", out)
			}
			if tc.spliced && strings.Contains(out, "resp_B") {
				t.Fatalf("a stateless client sees round 1's id only: %q", out)
			}
			ds2 := inbandtest.RSDatas(out)
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

// The provider refuses the request with our search and accepts the original: the retried original
// is not a spliced stream, and it reaches the client untouched.
func TestResponsesSearchRetriesOriginalOn400(t *testing.T) {
	respDecl := func(name string) string {
		return fmt.Sprintf(`{"type":"function","name":%q,"description":"Does the %s job. %s","parameters":{"type":"object"}}`,
			name, name, strings.Repeat("Detailed usage notes for this tool. ", 60))
	}
	body := []byte(`{"model":"gpt-5.5","stream":true,"store":false,"instructions":"agent","tools":[` + respDecl("Bash") + `,` + respDecl("Read") + `,` +
		respDecl("Workflow") + `,` + respDecl("Monitor") + `],"input":[{"role":"user","content":"go"}]}`)
	// No terminal event: a stream still wrapped in the splicer would get a response.failed added.
	answer := inbandtest.RSCreated("resp_A") + inbandtest.RSText(0, "m1", "Hello")
	up := &csUpstream{script: []string{answer}, reject400: true}
	us := httptest.NewServer(up)
	defer us.Close()
	h := csHandler(t, "ultrahigh", us.URL)
	ds := h.toolDefer()
	for i, s := range []string{"r1", "r2", "r3"} {
		ds.hist.Observe("", s, "", tooldefer.View("responses", body), int64(i+10))
	}
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/openai/v1/responses", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("x-context-guru-session", "rs1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	b := up.all()
	if len(b) != 2 || bytes.Contains(b[1], []byte(tooldefer.ClientSearchName)) {
		t.Fatalf("retry was not the original: %d rounds", len(b))
	}
	if resp.StatusCode != http.StatusOK || string(raw) != answer {
		t.Fatalf("the client must get the retried stream untouched: %d %q", resp.StatusCode, raw)
	}
}

// Codex opens a WebSocket first. 426 makes it fall back to HTTPS at once; 405 made it retry 5 times.
func TestResponsesWebSocketUpgradeGets426(t *testing.T) {
	h := csHandler(t, "ultrahigh", "http://127.0.0.1:1")
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	get := func(upgrade string) *http.Response {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/openai/v1/responses", nil)
		if upgrade != "" {
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", upgrade)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if got := get("websocket").StatusCode; got != http.StatusUpgradeRequired {
		t.Fatalf("websocket upgrade: %d", got)
	}
	if r := get(""); r.StatusCode != http.StatusMethodNotAllowed || r.Header.Get("Allow") != "POST" {
		t.Fatalf("plain GET: %d allow=%q", r.StatusCode, r.Header.Get("Allow"))
	}
}
