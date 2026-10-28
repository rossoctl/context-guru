package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tidwall/gjson"

	cf "github.com/rossoctl/context-guru/counterfactual"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/adjudicate"
)

// M4: the savings record ends with the body that goes upstream, AFTER expand.Inject and
// adjudicate.Inject appended their tool declarations: the provider bills those bytes, so the
// library's bytes-to-tokens ratio must count them. Asserted by comparing the Forwarded body the
// library was given with cfBodies of the bytes the upstream received, and its breakpoints with the
// upstream body's cache_control marks.
func TestSavingsRecordEndsWithTheInjectedForwardedBody(t *testing.T) {
	var fwd atomic.Value
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		fwd.Store(b)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"m1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],`+
			`"stop_reason":"end_turn","usage":{"input_tokens":20,"output_tokens":1,"cache_creation_input_tokens":50}}`)
	}))
	defer up.Close()
	h, rec, tap := cfHandlerYAML(t, up.URL, "shadow", "pipeline: [extract, extract_llm_sweep]\n")
	var got atomic.Pointer[cf.Request]
	h.savings.onEnd = func(r *cf.Request) { got.Store(r) }
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	body := `{"model":"claude-sonnet-5","max_tokens":64,` +
		`"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],` +
		`"tools":[{"name":"read_file","description":"read a file","input_schema":{"type":"object"}}],` +
		`"messages":[{"role":"user","content":"go"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"read_file","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":` +
		fmt.Sprintf("%q", bigToolOutput()) + `,"cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-context-guru-session", "sess-inject")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	sent, _ := fwd.Load().([]byte)
	if !expand.HasTool("anthropic", sent) || !adjudicate.HasTool("anthropic", sent) {
		t.Fatalf("precondition: both tools must be injected: %s", gjson.GetBytes(sent, "tools"))
	}
	r := got.Load()
	if r == nil {
		t.Fatal("the savings record never ended")
	}
	if want := cfBodies(sent); fmt.Sprint(r.Forwarded) != fmt.Sprint(want) {
		t.Fatalf("Forwarded given to the library differs from the body sent upstream:\n got %+v\nwant %+v", r.Forwarded, want)
	}
	if len(r.Forwarded.Pos) == 0 || r.Forwarded.Start[1] != 3 {
		t.Fatalf("the forwarded tools section must hold the client tool and the two injected ones: %+v", r.Forwarded.Start)
	}
	if fmt.Sprint(r.Breakpoints) != fmt.Sprint(cfBreakpoints(sent)) || len(r.Breakpoints) == 0 {
		t.Fatalf("breakpoints %+v, want %+v", r.Breakpoints, cfBreakpoints(sent))
	}
	// KNOWN LIMIT, pinned: no operation describes the injected declarations, so the tools section
	// differs between the client and forwarded bodies with no op to explain it. The library's sum
	// check fails there, and counterfactual mode keeps the legacy figures (check_sum). Safe, and the
	// reason is visible in cf_shadow / cf_fallback.
	waitForRows(t, rec, 1)
	e := tap.all()[0]
	res, _ := e.Counterfactual()
	tools := false
	for _, c := range res.Checks {
		tools = tools || (c.Check == "sum" && strings.Contains(c.Detail, "section=tools"))
	}
	if !tools || e.CounterfactualFallback() != "check_sum" {
		t.Fatalf("checks %+v fallback %q: the injection must be caught by the sum gate", res.Checks, e.CounterfactualFallback())
	}
}
