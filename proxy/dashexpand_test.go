package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/dash"
)

// TestDashboardRecordsExpands: the expand loop recorded restorations only to the
// aggregator, so dash.Event.Expands/ExpandTokens were permanently 0 — the "Restorations"
// tile read zero while /stats showed the true count, and Overview.SavedAdjusted
// (SavedUnique − ExpandTokens) OVER-REPORTED net savings by the whole bounce.
func TestDashboardRecordsExpands(t *testing.T) {
	var calls int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			// The model asks for the offloaded original back.
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[` +
				`{"id":"call_1","type":"function","function":{"name":"context_guru_expand","arguments":"{\"id\":\"HASH\"}"}}` +
				`]},"finish_reason":"tool_calls"}]}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}],` +
			`"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer up.Close()

	h, rec := dashHandler(t, up.URL, dash.Options{})
	h.store.Put("HASH", []byte(strings.Repeat("the original content that had to come back\n", 20)))
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	body := `{"model":"gpt-x","tools":[{"type":"function","function":{"name":"read_file"}}],"messages":[` +
		`{"role":"user","content":"go"},` +
		`{"role":"tool","tool_call_id":"a","content":"` + strings.Repeat("a verbose repeated tool output line\\n", 60) + `"},` +
		`{"role":"tool","tool_call_id":"b","content":"` + strings.Repeat("a verbose repeated tool output line\\n", 60) + `"}]}`
	resp, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if calls != 2 {
		t.Fatalf("expected the expand continuation (2 upstream calls), got %d", calls)
	}
	waitForRows(t, rec, 1)

	page, err := rec.DB().Requests(dash.Filter{}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 {
		t.Fatalf("captured %d rows; want 1", page.Total)
	}
	e := page.Requests[0]
	if e.Expands != 1 {
		t.Errorf("row expands = %d; want 1", e.Expands)
	}
	if e.ExpandTokens <= 0 {
		t.Errorf("row expand_tokens = %d; want the restored content's tokens", e.ExpandTokens)
	}

	o, err := rec.DB().Overview(dash.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if o.Expands != 1 || o.ExpandTokens <= 0 {
		t.Fatalf("overview expands=%d expand_tokens=%d", o.Expands, o.ExpandTokens)
	}
	if o.ExpandRate <= 0 {
		t.Errorf("expand_rate = %v; want > 0", o.ExpandRate)
	}
	if o.SavedUnique <= 0 {
		t.Fatalf("nothing was compacted (saved_unique=%d); the assertion below would be vacuous", o.SavedUnique)
	}
	if o.SavedAdjusted != o.SavedUnique-o.ExpandTokens {
		t.Errorf("saved_adjusted = %d; want saved_unique(%d) − expand_tokens(%d)",
			o.SavedAdjusted, o.SavedUnique, o.ExpandTokens)
	}
	if o.SavedAdjusted >= o.SavedUnique {
		t.Errorf("a restoration must REDUCE net savings: adjusted=%d unique=%d",
			o.SavedAdjusted, o.SavedUnique)
	}
}
