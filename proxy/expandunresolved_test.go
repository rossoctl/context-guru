package proxy_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/expand"
)

// NOTHING RESOLVED must be answered BY THE PROXY, never relayed to the client.
//
// The old behaviour replayed the model own tool_use when no id resolved. A client that does not
// implement this tool then answers "Tool context_guru_expand not found", the model loses its recovery
// path, and it re-runs the original tool instead -- a full tool execution plus fresh output, enlarging
// the transcript that provoked the cut. Measured on LOCA: 48 of 108 attempts refused in one arm, with
// exact-repeat tool calls at 9.9-25.3% against 0.4% in a lossless baseline.
func TestUnresolvedExpandIsAnsweredNotRelayed(t *testing.T) {
	var calls int
	var secondBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			// The model asks for an id that is NOT in the store.
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[` +
				`{"id":"call_1","type":"function","function":{"name":"context_guru_expand","arguments":"{\"id\":\"GONE\"}"}}` +
				`]},"finish_reason":"tool_calls"}]}`))
			return
		}
		secondBody = b
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"understood, it is gone"}}]}`))
	}))
	defer upstream.Close()

	h, st := buildHandler(t, "pipeline: []\n", upstream.URL)
	st.Put("HASH", []byte("SOMETHING ELSE")) // store has content, but not under GONE
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	body := expandableBody("HASH")
	resp, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	final, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if calls < 2 {
		t.Fatalf("the proxy relayed the unresolved call instead of answering it: only %d upstream call(s). "+
			"A client without this tool answers \"Tool not found\" and the model re-runs the original tool.", calls)
	}
	if !strings.Contains(string(secondBody), "no longer available") {
		t.Errorf("the continuation should tell the model the content is gone; got %.400s", secondBody)
	}
	if strings.Contains(string(final), "context_guru_expand") {
		t.Errorf("the client was handed a tool_use for a tool it may not implement: %.300s", final)
	}
	// And the miss must be COUNTED, split by cause -- it was invisible in /stats before.
	mal, miss := expand.Unresolved()
	if mal+miss == 0 {
		t.Error("an unresolved expand call was not counted; reversibility can fail silently again")
	}
	t.Logf("unresolved counters: malformed=%d missing=%d", mal, miss)
}

// INTERCEPTION MUST OUTLIVE ADVERTISEMENT.
//
// Under InjectAuto the tool is advertised only on requests carrying a marker, and interception used to
// be gated on the same test. But a model that saw the tool on one turn calls it on a later turn, and
// that later turn often has no marker -- so nothing intercepted and the tool_use was relayed to a
// client with no such tool. Interception now also covers any session that has EVER been offered it.
func TestExpandInterceptedAfterAdvertisementStops(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		calls++
		w.Header().Set("Content-Type", "application/json")
		// On the SECOND client request (which carries no marker) the model calls expand anyway.
		if calls == 2 {
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[` +
				`{"id":"call_1","type":"function","function":{"name":"context_guru_expand","arguments":"{\"id\":\"HASH\"}"}}` +
				`]},"finish_reason":"tool_calls"}]}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	h, st := buildHandler(t, "pipeline: []\n", upstream.URL)
	st.Put("HASH", []byte("THE ORIGINAL CONTENT"))
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	// Turn 1 CARRIES a marker, so the tool is advertised and the session is remembered.
	turn1 := strings.Replace(string(expandableBody("HASH")), `{"model":`,
		`{"metadata":{"user_id":"sess-decouple"},"model":`, 1)
	r1, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json",
		strings.NewReader(turn1))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(r1.Body)
	r1.Body.Close()

	// Turn 2 carries NO marker -- same session id, since buildHandler's bodies share one.
	plain := `{"metadata":{"user_id":"sess-decouple"},"model":"gpt-x","messages":[{"role":"user","content":"carry on"}],"tools":[{"type":"function","function":{"name":"Read"}}]}`
	r2, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json", strings.NewReader(plain))
	if err != nil {
		t.Fatal(err)
	}
	final, _ := io.ReadAll(r2.Body)
	r2.Body.Close()

	if strings.Contains(string(final), "context_guru_expand") {
		t.Errorf("a marker-free turn relayed the expand tool_use to the client: %.300s", final)
	}
	t.Logf("upstream calls=%d final=%.120s", calls, final)
}
