package proxy

import (
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/dash"
)

// Apply writes exactly the names the caller's own report offers, refuses a stale basis or an
// unknown option, and "restore" takes them back out.
func TestAdaptToolsApplyWritesOnlyTheReportedNames(t *testing.T) {
	f := newMgrFixture(t)
	f.h.API().SetToolFilterState(f.h.DashToolFilter())
	w, _ := f.signUp(t, "user@ibm.com", "u")
	jar := w.Result().Cookies()
	_, me := f.do(t, "GET", "/api/me", "", jar)
	tn, _ := me["tenant"].(map[string]any)
	id, _ := tn["id"].(string)

	e := &dash.Event{TS: time.Now().UnixMilli(), TenantID: id, SessionID: "s1", Model: "m",
		CacheRead: 1000, TokenAccounting: dash.AccountingComplete, DeclDigest: "d1"}
	e.Tools = 1
	f.rec.Record(e)
	f.rec.RecordInventory(id, "s1", time.Now().UnixMilli(), &dash.Inventory{Digest: "d1", UseFingerprint: 1,
		Decls: []dash.Decl{{Kind: dash.KindMCPTool, Name: "mcp__srv__q", Server: "srv", Tokens: 100}}}, false)
	var rep map[string]any
	for i := 0; i < 100; i++ {
		_, rep = f.do(t, "GET", "/api/adapt/tools", "", jar)
		if items, _ := rep["items"].([]any); len(items) == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	basis, _ := rep["basis_id"].(string)
	if basis == "" {
		t.Fatalf("no report: %v", rep)
	}
	config := func() string {
		_, me := f.do(t, "GET", "/api/me", "", jar)
		tn, _ := me["tenant"].(map[string]any)
		doc, _ := tn["effective_config_yaml"].(string)
		return doc
	}

	if w, _ := f.do(t, "POST", "/api/adapt/tools/apply", `{"option":"remove-unused","basis":"stale"}`, jar); w.Code != 409 {
		t.Fatalf("stale basis = %d %s", w.Code, w.Body)
	}
	if w, _ := f.do(t, "POST", "/api/adapt/tools/apply", `{"option":"Read"}`, jar); w.Code != 400 {
		t.Fatalf("unknown option = %d %s", w.Code, w.Body)
	}
	if w, _ := f.do(t, "POST", "/api/adapt/tools/apply", `{"option":"remove-unused"}`, nil); w.Code != 401 {
		t.Fatalf("no cookie = %d %s", w.Code, w.Body)
	}
	if strings.Contains(config(), "mcp__srv") {
		t.Fatal("a refused apply wrote the list")
	}
	w, out := f.do(t, "POST", "/api/adapt/tools/apply", `{"option":"remove-unused","basis":"`+basis+`"}`, jar)
	if w.Code != 200 || out["ok"] != true {
		t.Fatalf("apply = %d %s", w.Code, w.Body)
	}
	if !strings.Contains(config(), "mcp__srv") {
		t.Fatalf("apply did not write the server: %q", config())
	}
	undo, _ := out["undo"].(map[string]any)
	if w, _ := f.do(t, "POST", "/api/adapt/tools/apply", `{"option":"`+undo["option"].(string)+`"}`, jar); w.Code != 200 {
		t.Fatalf("undo = %d %s", w.Code, w.Body)
	}
	if strings.Contains(config(), "mcp__srv") {
		t.Fatalf("undo left the server filtered: %q", config())
	}
}
