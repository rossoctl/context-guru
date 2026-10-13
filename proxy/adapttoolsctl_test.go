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

// On a single-tenant proxy there is no account configuration to write, and the caller is told
// what to do instead, in JSON, rather than getting the mux's plain 404.
func TestAdaptToolsApplyOnALocalProxySaysWhatToDo(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	h, _ := dashHandler(t, up.URL, dash.Options{})
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	resp, err := srv.Client().Post(srv.URL+"/api/adapt/tools/apply", "application/json", strings.NewReader(`{"option":"remove-unused"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != http.StatusNotImplemented ||
		!strings.Contains(out["error"], "remove command") {
		t.Fatalf("status %d, body %v, err %v", resp.StatusCode, out, err)
	}
}
