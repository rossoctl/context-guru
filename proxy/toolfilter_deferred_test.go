package proxy

import (
	"net/http"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/dash"
)

// A deferred MCP tool is a name in a message, which the proxy filter cannot remove. The page no
// longer offers the switch; this is the server-side refusal, so a hand-made POST (or a page that
// missed a switch) cannot store an exclusion that saves nothing and hides the item.
func TestToolFilterRefusesADeferredTool(t *testing.T) {
	f := newMgrFixture(t)
	jar, id := f.signUpJar(t, "a@ibm.com")
	f.rec.RecordInventory(id, "s1", time.Now().UnixMilli(), &dash.Inventory{Digest: "d1", Decls: []dash.Decl{
		{Kind: dash.KindMCPTool, Name: "mcp__dfr__a", Server: "dfr", Tokens: 10, Deferred: true},
		{Kind: dash.KindMCPTool, Name: "mcp__arr__b", Server: "arr", Tokens: 400},
	}}, false, "")
	deadline := time.Now().Add(5 * time.Second)
	for {
		ok, err := f.rec.DB().IsDeferredDecl(id, "mcp__dfr__a")
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the deferred row was never written")
		}
		time.Sleep(10 * time.Millisecond)
	}

	for _, body := range []string{
		`{"kind":"mcp_server","server":"dfr","action":"exclude"}`,
		`{"kind":"mcp_tool","name":"mcp__dfr__a","server":"dfr","action":"exclude"}`,
	} {
		if w, _ := f.do(t, "POST", "/api/toolfilter", body, jar); w.Code != http.StatusConflict {
			t.Errorf("%s = %d %s, want 409", body, w.Code, w.Body)
		}
	}
	// A tools-array server is filterable as before, and an include is never refused.
	for _, body := range []string{
		`{"kind":"mcp_server","server":"arr","action":"exclude"}`,
		`{"kind":"mcp_server","server":"dfr","action":"include"}`,
	} {
		if w, _ := f.do(t, "POST", "/api/toolfilter", body, jar); w.Code != http.StatusOK {
			t.Errorf("%s = %d %s, want 200", body, w.Code, w.Body)
		}
	}
}
