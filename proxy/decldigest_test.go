package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/dash"
)

// A captured request records the declaration set it carried, so /api/adapt/tools prices a
// declaration on exactly the requests that carried it rather than guessing from the tool count.
func TestCapturedRequestRecordsItsDeclarationDigest(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	h, rec := dashHandler(t, up.URL, dash.Options{})
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	var body map[string]any
	if err := json.Unmarshal(anthropicRequest("aws/claude-sonnet-5"), &body); err != nil {
		t.Fatal(err)
	}
	body["tools"] = []any{map[string]any{"name": "mcp__srv__q", "description": "query",
		"input_schema": map[string]any{"type": "object"}}}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-context-guru-session", "sess-digest")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	waitForRows(t, rec, 1)

	// The inventory has its own writer and flush cadence, so wait for it too.
	var doc *dash.AdaptToolsDoc
	for i := 0; i < 100; i++ {
		if doc, err = rec.DB().AdaptTools(dash.Filter{TenantAll: true}, nil, dash.ToolFilterState{}); err != nil {
			t.Fatal(err)
		}
		if doc.Coverage.SessionsCaptured > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if doc.Coverage.RequestsExact != 1 {
		t.Fatalf("coverage %+v: the request did not record its declaration digest", doc.Coverage)
	}
}
