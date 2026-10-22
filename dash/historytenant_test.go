package dash

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func hostedHistoryAPI(t *testing.T, db *DB, home, tenant string) *http.ServeMux {
	t.Helper()
	rec := &Recorder{db: db, hub: NewHub(), done: make(chan struct{})}
	t.Cleanup(func() { rec.Close() })
	api := NewAPI(rec)
	api.home = home
	api.SetAuth(func(*http.Request) (Principal, bool) { return Principal{TenantID: tenant}, true })
	mux := http.NewServeMux()
	api.Mount(mux)
	return mux
}

// B2: tenant B presenting tenant A's session id, and A's invoked names, must see none of it.
func TestHistoryNeverCrossesTenants(t *testing.T) {
	db := openTestDB(t)
	if err := db.insertBatch([]*Event{
		{TS: 1000, TenantID: "a", SessionID: "shared", Model: "m", TokensBefore: 1},
		{TS: 1000, TenantID: "b", SessionID: "shared", Model: "m", TokensBefore: 1},
	}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"SecretMCPTool"} {
		if _, err := db.sql.Exec(`INSERT INTO tool_uses(tenant_id, session_id, name, skill, calls,
			first_ts, last_ts, project) VALUES ('a','shared',?, '', 3, 1, 2, '')`, n); err != nil {
			t.Fatal(err)
		}
	}
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".claude", "plugins", "marketplaces", "m", "skills", "serverskill"), 0o755)
	mux := hostedHistoryAPI(t, db, home, "b")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/history", nil))
	if rr.Code != 200 {
		t.Fatalf("status %d", rr.Code)
	}
	var doc HistoryDoc
	json.Unmarshal(rr.Body.Bytes(), &doc)
	for _, u := range doc.Used {
		if u.Name == "SecretMCPTool" {
			t.Errorf("tenant b sees tenant a's invocation %q", u.Name)
		}
	}
	// calledAnywhere must be tenant-scoped too.
	ca, err := db.calledAnywhereFor(Filter{Tenant: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if ca["SecretMCPTool"] {
		t.Error("calledAnywhere leaked another tenant's capability name")
	}
	// Hosted: the server's own $HOME is not this tenant's inventory.
	if doc.InstalledSkills != 0 || len(doc.NeverUsed) != 0 {
		t.Errorf("hosted mode reported the server's installed skills: %d, %v", doc.InstalledSkills, doc.NeverUsed)
	}
}

// Hosted facets must not offer project roots read from the server's transcript tree.
func TestHostedFacetsDoNotReadTheServersTranscripts(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	writeJSONL(t, home, "-srv", "s1", turnLine("s1", "/srv/secret-project", daysAgo(2), 5, 5))
	if err := db.insertBatch([]*Event{{TS: 1000, TenantID: "b", SessionID: "s1", Model: "m",
		Project: "/srv/secret-project/sub", TokensBefore: 1}}); err != nil {
		t.Fatal(err)
	}
	mux := hostedHistoryAPI(t, db, home, "b")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/facets", nil))
	var out map[string][]string
	json.Unmarshal(rr.Body.Bytes(), &out)
	for _, p := range out["project"] {
		if p == "/srv/secret-project" {
			t.Errorf("hosted facets rolled up using the server's transcript tree: %v", out["project"])
		}
	}
}
