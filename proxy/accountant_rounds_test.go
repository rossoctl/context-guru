package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// M3: after an expand round the usage is the LAST round's, billed on a body the loop grew, while
// the savings record holds the first round's body. No Result may be attached, and PriceMode then
// keeps the legacy figures (fallback reason no_result).
func TestAccountantSkipsARequestWithExpandRounds(t *testing.T) {
	var calls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[` +
				`{"id":"call_1","type":"function","function":{"name":"context_guru_expand","arguments":"{\"id\":\"HASH\"}"}}` +
				`]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}],` +
			`"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer up.Close()
	h, rec, tap := cfHandler(t, up.URL)
	h.store.Put("HASH", []byte(strings.Repeat("the original content that had to come back\n", 20)))
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	body := `{"model":"gpt-x","tools":[{"type":"function","function":{"name":"read_file"}}],"messages":[` +
		`{"role":"user","content":"go"},` +
		`{"role":"tool","tool_call_id":"a","content":"` + strings.Repeat("a verbose repeated tool output line\\n", 60) + `"}]}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-context-guru-session", "sess-rounds")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if n := calls.Load(); n != 2 {
		t.Fatalf("precondition: expected an expand round (2 upstream calls), got %d", n)
	}
	waitForRows(t, rec, 1)
	e := tap.all()[0]
	if e.HasCounterfactual() {
		t.Fatal("a Result is attached to a request with expand rounds")
	}
	if fb := e.CounterfactualFallback(); fb != "no_result" {
		t.Fatalf("fallback = %q, want no_result", fb)
	}
}
