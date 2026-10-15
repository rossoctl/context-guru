package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	waitInventory(t, f.rec, 1)
	_, rep = f.do(t, "GET", "/api/adapt/tools", "", jar)
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
	if w, _ := f.do(t, "POST", "/api/adapt/tools/apply", `{"option":"remove-unused"}`, jar); w.Code != 400 {
		t.Fatalf("missing basis = %d %s", w.Code, w.Body)
	}
	if w, _ := f.do(t, "POST", "/api/adapt/tools/apply", `{"option":"Read","basis":"`+basis+`"}`, jar); w.Code != 409 {
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

// On a single-tenant proxy, apply goes through the shared adapt helper: the running pipeline
// filters the names at once, from loopback only, and revert takes them back out.
func TestAdaptToolsApplyOnALocalProxy(t *testing.T) {
	h := localAdaptHandler(t, filepath.Join(t.TempDir(), "d.db.adapt.json"))
	h.API().SetToolFilterState(h.DashToolFilter())
	mux := h.Mux()
	do := func(method, path, body, addr string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.RemoteAddr = addr
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	e := &dash.Event{TS: time.Now().UnixMilli(), SessionID: "s1", Model: "m", CacheRead: 1000,
		TokenAccounting: dash.AccountingComplete, DeclDigest: "d1"}
	e.Tools = 1
	h.rec.Record(e)
	h.rec.RecordInventory("", "s1", time.Now().UnixMilli(), &dash.Inventory{Digest: "d1", UseFingerprint: 1,
		Decls: []dash.Decl{{Kind: dash.KindMCPTool, Name: "mcp__srv__q", Server: "srv", Tokens: 100}}}, false)
	var rep map[string]any
	waitInventory(t, h.rec, 1)
	json.Unmarshal(do("GET", "/api/adapt/tools", "", "127.0.0.1:1").Body.Bytes(), &rep)
	body := `{"option":"remove-unused","basis":"` + rep["basis_id"].(string) + `"}`
	if w := do("POST", "/api/adapt/tools/apply", body, "198.51.100.7:1"); w.Code != http.StatusForbidden {
		t.Fatalf("off-loopback apply = %d %s", w.Code, w.Body)
	}
	if w := do("POST", "/api/adapt/tools/apply", body, "127.0.0.1:1"); w.Code != http.StatusOK {
		t.Fatalf("apply = %d %s", w.Code, w.Body)
	}
	removed := func() string {
		tf, _ := h.static.Load().Pipe.Find("toolfilter").(interface{ Removed() []string })
		if tf == nil {
			return ""
		}
		return strings.Join(tf.Removed(), ",")
	}
	if got := removed(); got != "mcp__srv" {
		t.Fatalf("running pipeline filters %q", got)
	}
	if w := do("POST", "/api/adapt/tools/apply", `{"option":"revert"}`, "127.0.0.1:1"); w.Code != http.StatusOK {
		t.Fatalf("revert = %d %s", w.Code, w.Body)
	}
	if got := removed(); got != "" {
		t.Fatalf("after revert the pipeline filters %q", got)
	}
}

// The apply note is read back from what was written, and adding toolfilter to a pipeline that
// already runs it leaves the order alone.
func TestAdaptToolsNoteAndPipelineOrder(t *testing.T) {
	doc := "pipeline: [format, toolfilter, searchfold]\ncomponents:\n  toolfilter:\n    remove: [mcp__ops]\n"
	if got := applyNote([]string{"mcp__ops", "mcp__srv"}, doc, true); got !=
		"stopped filtering mcp__srv; still filtered because your configuration lists them: mcp__ops" {
		t.Errorf("restore note %q", got)
	}
	if got := applyNote([]string{"mcp__ops"}, doc, false); got != "filtering mcp__ops" {
		t.Errorf("apply note %q", got)
	}
	if got := strings.Join(withComponent([]string{"format", "toolfilter", "searchfold"}, "toolfilter", true), ","); got != "format,toolfilter,searchfold" {
		t.Errorf("present component moved: %s", got)
	}
	if got := strings.Join(withComponent([]string{"format"}, "toolfilter", true), ","); got != "format,toolfilter" {
		t.Errorf("absent component not added: %s", got)
	}
}

// A manager's dashboard default is the whole service, but a removal list is one account's: the
// report and the apply act on the manager's own traffic, never another tenant's.
func TestAdaptToolsApplyForAManagerWritesOnlyTheirOwn(t *testing.T) {
	f := newMgrFixture(t)
	f.h.API().SetToolFilterState(f.h.DashToolFilter())
	w, _ := f.signUp(t, "boss@ibm.com", "m")
	boss := w.Result().Cookies()
	w, _ = f.signUp(t, "user@ibm.com", "u")
	user := w.Result().Cookies()
	idOf := func(jar []*http.Cookie) string {
		_, me := f.do(t, "GET", "/api/me", "", jar)
		tn, _ := me["tenant"].(map[string]any)
		id, _ := tn["id"].(string)
		return id
	}
	for _, s := range []struct{ id, server string }{{idOf(boss), "mine"}, {idOf(user), "other"}} {
		e := &dash.Event{TS: time.Now().UnixMilli(), TenantID: s.id, SessionID: "s-" + s.server, Model: "m",
			CacheRead: 1000, TokenAccounting: dash.AccountingComplete, DeclDigest: "d"}
		e.Tools = 1
		f.rec.Record(e)
		f.rec.RecordInventory(s.id, "s-"+s.server, time.Now().UnixMilli(), &dash.Inventory{Digest: "d", UseFingerprint: 1,
			Decls: []dash.Decl{{Kind: dash.KindMCPTool, Name: "mcp__" + s.server + "__q", Server: s.server, Tokens: 100}}}, false)
	}
	var rep map[string]any
	waitInventory(t, f.rec, 2)
	_, rep = f.do(t, "GET", "/api/adapt/tools", "", boss)
	if items, _ := rep["items"].([]any); len(items) != 1 {
		t.Fatalf("a manager's default report is not their own: %v", rep["items"])
	}
	if w, _ := f.do(t, "POST", "/api/adapt/tools/apply", `{"option":"remove-unused","basis":"`+rep["basis_id"].(string)+`"}`, boss); w.Code != 200 {
		t.Fatalf("apply = %d %s", w.Code, w.Body)
	}
	_, me := f.do(t, "GET", "/api/me", "", boss)
	tn, _ := me["tenant"].(map[string]any)
	doc, _ := tn["effective_config_yaml"].(string)
	if !strings.Contains(doc, "mcp__mine") || strings.Contains(doc, "mcp__other") {
		t.Fatalf("manager apply wrote the wrong account's names: %q", doc)
	}
}

// waitInventory waits for the inventory writer (its own queue and flush cadence) to have stored
// n sessions, reading the database directly: the report route is rate-limited per account.
func waitInventory(t *testing.T, rec *dash.Recorder, n int) {
	t.Helper()
	for i := 0; i < 200; i++ {
		doc, err := rec.DB().AdaptTools(dash.Filter{TenantAll: true}, nil, dash.ToolFilterState{})
		if err != nil {
			t.Fatal(err)
		}
		if doc.Coverage.SessionsCaptured >= n {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("inventory for %d sessions never landed", n)
}

// The last line of defence: whatever an offer holds, apply writes no built-in, no bare tool and
// none of Context Guru's own entries.
func TestAdaptToolsApplyRefusesOwnAndNonMCPNames(t *testing.T) {
	h := localAdaptHandler(t, filepath.Join(t.TempDir(), "d.db.adapt.json"))
	mux := h.Mux()
	for _, bad := range []string{"mcp__plugin_context-guru_cg", "skill__context-guru:waste", "Read"} {
		basis := h.API().OfferAdapt("tools", "", map[string]any{"x": []string{bad}})
		r := httptest.NewRequest("POST", "/api/adapt/tools/apply", strings.NewReader(`{"option":"x","basis":"`+basis+`"}`))
		r.RemoteAddr = "127.0.0.1:1"
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: apply = %d %s", bad, w.Code, w.Body)
		}
	}
	if tf, _ := h.static.Load().Pipe.Find("toolfilter").(interface{ Removed() []string }); tf != nil && len(tf.Removed()) > 0 {
		t.Fatalf("a refused name was written: %v", tf.Removed())
	}
}
