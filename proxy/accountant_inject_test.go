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

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/adjudicate"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// M4: the savings record ends with the body that goes upstream, AFTER expand.Inject and
// adjudicate.Inject appended their tool declarations: the provider bills those bytes, so the
// library's bytes-to-tokens ratio must count them. Asserted by comparing the Forwarded body the
// library was given with accBodies of the bytes the upstream received, and its breakpoints with the
// upstream body's cache_control marks.
func TestSavingsRecordEndsWithTheInjectedForwardedBody(t *testing.T) {
	var fwd atomic.Value
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		fwd.Store(b)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"m1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],`+
			`"stop_reason":"end_turn","usage":{"input_tokens":200,"output_tokens":1,"cache_creation_input_tokens":300}}`)
	}))
	defer up.Close()
	h, rec, tap := accHandlerYAML(t, up.URL, "pipeline: [extract, extract_llm_sweep]\n")
	var got atomic.Pointer[accountant.Request]
	h.savings.onEnd = func(r *accountant.Request) { got.Store(r) }
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
	if want := accBodies(sent); fmt.Sprint(r.Forwarded) != fmt.Sprint(want) {
		t.Fatalf("Forwarded given to the library differs from the body sent upstream:\n got %+v\nwant %+v", r.Forwarded, want)
	}
	if len(r.Forwarded.Pos) == 0 || r.Forwarded.Start[1] != 3 {
		t.Fatalf("the forwarded tools section must hold the client tool and the two injected ones: %+v", r.Forwarded.Start)
	}
	if fmt.Sprint(r.Breakpoints) != fmt.Sprint(accBreakpoints(sent)) || len(r.Breakpoints) == 0 {
		t.Fatalf("breakpoints %+v, want %+v", r.Breakpoints, accBreakpoints(sent))
	}
	// The injected declarations report no operation of their own; envelopeOps finds them in the body
	// diff and reports each as an added declaration. The sum check passes, the override applies, and
	// each injection is a cost on a row of its own.
	waitForRows(t, rec, 1)
	e := tap.all()[0]
	res, _ := e.AccountantResult()
	for _, c := range res.Checks {
		if c.Check == "sum" {
			t.Fatalf("sum check failed: %+v", c)
		}
	}
	if fb := e.AccountantFallback(); fb != "" {
		t.Fatalf("fell back: %q", fb)
	}
	for _, name := range []string{"expand", "adjudicate"} {
		var row *dash.CompRow
		for i := range e.Components {
			if e.Components[i].Component == name {
				row = &e.Components[i]
			}
		}
		// This is the first request of its thread, so the library priced it as an estimate and the
		// books hold it out (issue #915): the row exists and books 0. The library's own figure for
		// the row is the cost.
		if row == nil || row.Kind != dash.KindEnvelope || row.SavedUSD != 0 {
			t.Fatalf("%s: row %+v, want an envelope row that books nothing on an estimated request", name, row)
		}
		price := modelinfo.Price{Input: 3e-6, CacheRead: 0.3e-6, CacheWrite: 3.75e-6, Output: 15e-6}
		if fig := e.AccountantFigure(price); fig.ByComponent[name] >= 0 {
			t.Fatalf("%s: the library's figure is %v, want a cost", name, fig.ByComponent[name])
		}
	}
	if !e.PriceEstimated || e.EstimatedSavedUSD == 0 {
		t.Fatalf("the first request of a thread must be held as an estimate: estimated %v held %v", e.PriceEstimated, e.EstimatedSavedUSD)
	}
	var sum float64
	for _, c := range e.Components {
		sum += c.SavedUSD
	}
	if d := sum - (e.BaselineCostUSD - e.CostUSD); d > 1e-12 || d < -1e-12 {
		t.Fatalf("rows sum %v, baseline delta %v", sum, e.BaselineCostUSD-e.CostUSD)
	}
}
