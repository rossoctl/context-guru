package proxy

// A request on the Responses route must reach the dashboard inventory on the Responses wire
// (#817). Passed the provider ("openai") instead, the flat tools below read as OpenAI Chat tools
// with no name and are filed as one server tool named "function".

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

func TestResponsesRouteFeedsTheInventoryOnTheResponsesWire(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	h, rec := dashHandler(t, up.URL, dash.Options{})
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	body := `{"model":"gpt-5.6","instructions":"stable","input":[{"role":"user","content":"task"},` +
		`{"type":"function_call","call_id":"c1","name":"ResTool","arguments":"{}"}],` +
		`"tools":[{"type":"function","name":"ResTool","parameters":{"type":"object"}},` +
		`{"type":"function","name":"OtherTool","parameters":{"type":"object"}}]}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/openai/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-context-guru-session", "sess-resp")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	waitForRows(t, rec, 1)

	price := func(string) (modelinfo.Price, bool) { return modelinfo.Price{Input: 1e-6}, true }
	var names map[string]bool
	for i := 0; i < 400; i++ { // the inventory is written by its own goroutine
		rep, err := rec.DB().ToolReportFor(dash.Filter{TenantAll: true}, price)
		if err != nil {
			t.Fatal(err)
		}
		names = map[string]bool{}
		for _, s := range append(append([]dash.ToolStat{}, rep.Tools...), rep.Aside...) {
			names[s.Name] = true
		}
		if len(names) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !names["ResTool"] || !names["OtherTool"] || names["function"] {
		t.Errorf("inventory names = %v, want ResTool and OtherTool read off the flat Responses tools", names)
	}
}
