package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rossoctl/context-guru/tooldefer"
	"github.com/tidwall/gjson"
)

// tdSearchTurn is an assistant turn in which our search loaded tool.
func tdSearchTurn(tool string) string {
	return `{"role":"assistant","content":[{"type":"server_tool_use","id":"srvtoolu_1","name":"tool_search_tool_bm25","input":{"query":"` + tool + `","limit":1}},` +
		`{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_1","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"` + tool + `"}]}},` +
		`{"type":"text","text":"done"}]}`
}

// refUpstream answers like the provider: a 400 for a body whose transcript names a tool `tools`
// does not declare, else 200.
type refUpstream struct {
	mu     sync.Mutex
	bodies [][]byte
	// failFirst refuses the first non-probe request with a deferral error, so the retry path runs.
	failFirst bool
	// hidden, when set, is a tool the provider sees referenced in any body that holds the text
	// "REF:"+hidden: a reference shape tooldefer.Loaded does not read, so only the retry repairs it.
	hidden string
}

func (u *refUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	if probe := gjson.GetBytes(b, "tools.1.name").String(); strings.HasPrefix(probe, "cg_probe_") {
		fmt.Fprintf(w, `{"content":[{"type":"tool_search_tool_result","tool_use_id":"s","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":%q}]}},`+
			`{"type":"tool_use","id":"t","name":%q,"input":{"code":7}}],"usage":{"input_tokens":10,"output_tokens":5}}`, probe, probe)
		return
	}
	u.mu.Lock()
	u.bodies = append(u.bodies, b)
	fail := u.failFirst
	u.failFirst = false
	u.mu.Unlock()
	declared := map[string]bool{}
	gjson.GetBytes(b, "tools").ForEach(func(_, t gjson.Result) bool {
		declared[t.Get("name").String()] = true
		return true
	})
	refs := tooldefer.Loaded(b)
	if u.hidden != "" && bytes.Contains(b, []byte("REF:"+u.hidden)) {
		refs[u.hidden] = true
	}
	for n := range refs {
		if !declared[n] {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"type":"error","error":{"type":"invalid_request_error","message":"Tool reference '%s' not found in available tools"}}`, n)
			return
		}
	}
	if fail {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"tools.2.custom.defer_loading: Extra inputs are not permitted"}}`)
		return
	}
	fmt.Fprint(w, `{"type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":5,"cache_read_input_tokens":3000,"output_tokens":2}}`)
}

func (u *refUpstream) all() [][]byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([][]byte(nil), u.bodies...)
}

// #871 defect 1. A resumed session whose MCP server was removed: the transcript holds our search's
// load of mcp__gone__lookup and `tools` no longer declares it. The proxy adds a deferred stub, so
// the client sees 200, and the stub is outside the cached prefix (appended, deferred).
func TestToolDeferRemovedMCPToolIsStubbed(t *testing.T) {
	up := &refUpstream{}
	us := httptest.NewServer(up)
	defer us.Close()
	h := tdHandler(t, "ultrahigh", us.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	resumed := tdBody(tdSearchTurn("mcp__gone__lookup"), `{"role":"user","content":"again"}`)
	resp, b := tdPost(t, srv, resumed)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("client saw %d: %s", resp.StatusCode, b)
	}
	sent := up.all()
	if len(sent) != 1 || !strings.Contains(tdTools(sent[0]), ",mcp__gone__lookup*") {
		t.Fatalf("%d sends, last tools %s", len(sent), tdTools(sent[len(sent)-1]))
	}
}

// #871 defect 1, the retry. The deferred request is refused for a deferral cause, and the original
// (the client's own body, before the stub) fails on the missing reference: the retry repairs it, so
// the client never sees a 400, and deferral stays on (the refusal was not deferral's).
func TestToolDeferRetryRepairsAMissingReference(t *testing.T) {
	h := tdHandler(t, "ultrahigh", "http://127.0.0.1:1")
	body := tdBody(tdSearchTurn("mcp__gone__lookup"))
	resp := &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(
		`{"type":"error","error":{"message":"Tool reference 'mcp__gone__lookup' not found in available tools"}}`))}
	fixed, ok := repairOnMissingRef(resp, body)
	if !ok || !strings.HasSuffix(tdTools(fixed), ",mcp__gone__lookup*") {
		t.Fatalf("ok=%v tools=%s", ok, tdTools(fixed))
	}
	// Another 400 is left readable, byte for byte, for the client.
	other := `{"type":"error","error":{"message":"messages.3: text content blocks must be non-empty"}}`
	resp = &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(other))}
	if _, ok := repairOnMissingRef(resp, body); ok {
		t.Fatal("repaired an unrelated 400")
	}
	if got, _ := io.ReadAll(resp.Body); string(got) != other {
		t.Fatalf("the unrelated 400 body = %q", got)
	}
	_ = h

	// End to end: the request path's own repair already gives the original its stub, so the retry
	// of a deferral refusal succeeds and the client sees 200.
	up := &refUpstream{}
	us := httptest.NewServer(up)
	defer us.Close()
	h2 := tdHandler(t, "ultrahigh", us.URL)
	srv := httptest.NewServer(h2.Mux())
	defer srv.Close()
	tdPost(t, srv, tdBody()) // runs the probe
	up.mu.Lock()
	up.failFirst = true
	up.mu.Unlock()
	s2 := bytes.Replace(tdBody(tdSearchTurn("mcp__gone__lookup"), `{"role":"user","content":"again"}`), []byte("sess-1"), []byte("sess-2"), 1)
	r2, b2 := tdPost(t, srv, s2)
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("client saw %d: %s", r2.StatusCode, b2)
	}

	// A reference the request path cannot see: the deferred body is refused, the original fails on
	// the missing reference, and the retry sends it once more with a stub. The client sees 200, and
	// deferral stays on: the refusal was not deferral's.
	up3 := &refUpstream{hidden: "mcp__gone__x"}
	us3 := httptest.NewServer(up3)
	defer us3.Close()
	h3 := tdHandler(t, "ultrahigh", us3.URL)
	srv3 := httptest.NewServer(h3.Mux())
	defer srv3.Close()
	tdPost(t, srv3, tdBody())
	up3.mu.Lock()
	up3.failFirst = true
	up3.mu.Unlock()
	before := len(up3.all())
	s3 := bytes.Replace(tdBody(`{"role":"assistant","content":"REF:mcp__gone__x"}`, `{"role":"user","content":"again"}`), []byte("sess-1"), []byte("sess-3"), 1)
	r3, b3 := tdPost(t, srv3, s3)
	sent := up3.all()[before:]
	if r3.StatusCode != http.StatusOK || len(sent) != 3 || !strings.Contains(tdTools(sent[2]), "mcp__gone__x*") {
		t.Fatalf("client saw %d after %d sends: %s", r3.StatusCode, len(sent), b3)
	}
	if strings.Contains(tdTools(sent[2]), "Workflow*") {
		t.Fatalf("the repaired retry is not the original: %s", tdTools(sent[2]))
	}
	if h3.deferGate().Broken("", "claude-sonnet-5-5") {
		t.Fatal("a refusal the original also got turned deferral off")
	}
}
