package proxy_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/expand"
)

// THE DOMINANT REAL CASE: an expand call arriving ALONGSIDE a real tool call.
//
// The response loop cannot satisfy this -- only the client can execute the real tool, so the response
// must be relayed and ResponseCalls sets otherTools, which bails. Live traffic: 102 of 107 refused
// turns carried two or more tool_use blocks, 5 carried one. So the client answers the expand call
// itself with "Tool 'context_guru_expand' not found", the model loses its recovery path, and it
// re-runs the original tool -- a full tool execution plus fresh output, enlarging the transcript that
// provoked the cut.
//
// The fix is on the REQUEST path: when the client sends its results back, its own failed tool_result
// for the expand call is replaced with the stashed original before the request goes upstream. This
// asserts the model actually receives the content.
func TestExpandRestoredOnMixedToolTurn(t *testing.T) {
	var lastUp []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastUp, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"thanks"}}]}`))
	}))
	defer upstream.Close()

	h, st := buildHandler(t, "pipeline: []\n", upstream.URL)
	st.Put("HASH", []byte("THE ORIGINAL CONTENT"))
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	// What the client sends back after a MIXED turn: it executed Read fine, and could not execute
	// the proxy-injected expand tool, so it wrote its own failure for that one.
	body := `{"model":"gpt-x","messages":[
      {"role":"user","content":"go"},
      {"role":"assistant","tool_calls":[
        {"id":"c_read","type":"function","function":{"name":"Read","arguments":"{}"}},
        {"id":"c_exp","type":"function","function":{"name":"context_guru_expand","arguments":"{\"id\":\"HASH\"}"}}]},
      {"role":"tool","tool_call_id":"c_read","content":"file contents here"},
      {"role":"tool","tool_call_id":"c_exp","content":"Tool 'context_guru_expand' not found"}
    ],"tools":[{"type":"function","function":{"name":"Read"}}]}`

	before := expand.Restored()
	resp, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	up := string(lastUp)
	if !strings.Contains(up, "THE ORIGINAL CONTENT") {
		t.Errorf("the model never received the content it asked for; the client's failure was forwarded "+
			"verbatim. upstream body: %.500s", up)
	}
	if strings.Contains(up, "not found") {
		t.Errorf("the client's 'not found' text reached the model: %.300s", up)
	}
	if !strings.Contains(up, "file contents here") {
		t.Error("the real tool's result must be preserved untouched")
	}
	if got := expand.Restored() - before; got != 1 {
		t.Errorf("expected 1 restored tool_result, counted %d", got)
	}
}
