package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// M3: after an expand round the usage is the LAST round's, billed on a body the loop grew, while
// the savings record holds the first round's body. No Result may be attached (PriceMode then keeps
// the legacy figures, reason no_result) and shadow logs cf_shadow_skipped instead of a figure.
func TestCounterfactualSkipsARequestWithExpandRounds(t *testing.T) {
	for _, mode := range []string{"shadow", "counterfactual"} {
		t.Run(mode, func(t *testing.T) {
			var buf syncBuf
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prev) })

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
			h, rec, tap := cfHandler(t, up.URL, mode)
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
			if mode == "counterfactual" {
				if fb := e.CounterfactualFallback(); fb != "no_result" {
					t.Fatalf("fallback = %q, want no_result", fb)
				}
				return
			}
			if n := strings.Count(buf.String(), `"msg":"cf_shadow"`); n != 0 {
				t.Fatalf("shadow logged a figure for an expand-round request:\n%s", buf.String())
			}
			if !strings.Contains(buf.String(), `"msg":"cf_shadow_skipped"`) || !strings.Contains(buf.String(), `"reason":"expand_rounds"`) {
				t.Fatalf("no cf_shadow_skipped line:\n%s", buf.String())
			}
		})
	}
}
