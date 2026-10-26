package dash

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// hostedAPI mounts the dashboard API in hosted mode for one tenant, over db and a transcript tree
// at home that the test controls.
func hostedAPI(t *testing.T, db *DB, home, tenant string) *http.ServeMux {
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

// Hosted facets must not offer project roots read from the server's transcript tree.
func TestHostedFacetsDoNotReadTheServersTranscripts(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	writeJSONL(t, home, "-srv", "s1", turnLine("s1", "/srv/secret-project", daysAgo(2), 5, 5))
	if err := db.insertBatch([]*Event{{TS: 1000, TenantID: "b", SessionID: "s1", Model: "m",
		Project: "/srv/secret-project/sub", TokensBefore: 1}}); err != nil {
		t.Fatal(err)
	}
	mux := hostedAPI(t, db, home, "b")
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
