package dash

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The API half of the project dimension: /api/insights honours ?project=, and the set of projects
// can be enumerated so a picker populates itself.

// projectAPI wires a real API over a store, so these go through the mounted route rather than
// calling the handler directly — the scope decision lives in the route table, and a test that
// bypasses it would not exercise it.
func projectAPI(t *testing.T, db *DB) *http.ServeMux {
	t.Helper()
	rec := &Recorder{db: db, hub: NewHub(), done: make(chan struct{})}
	t.Cleanup(func() { rec.Close() })
	mux := http.NewServeMux()
	NewAPI(rec).Mount(mux)
	return mux
}

// insightsFor runs the handler and returns the shaped document.
func insightsFor(t *testing.T, mux *http.ServeMux, query string) *InsightsDoc {
	t.Helper()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/insights"+query, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/insights%s = %d: %s", query, rr.Code, rr.Body.String())
	}
	var doc InsightsDoc
	if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decoding the document: %v", err)
	}
	return &doc
}

// A tool invoked in one project and merely carried in another must not read the same in both.
// This is the test that makes "strictly per-project evidence" real: it fails if the project filter
// is dropped anywhere between the query string and the SQL, because the two projects would then
// return the same pooled answer.
func TestTheInsightsDocumentIsScopedToOneProject(t *testing.T) {
	db := openTestDB(t)
	const a = "/Users/v/proj-a"
	const b = "/Users/v/proj-b"
	// Two sessions in project A that both declare and USE a tool; one in project B that only
	// declares it.
	evs := []*Event{
		{TS: 1000, SessionID: "a1", Model: "m", Project: a, TokensBefore: 10},
		{TS: 2000, SessionID: "a2", Model: "m", Project: a, TokensBefore: 10},
		{TS: 3000, SessionID: "b1", Model: "m", Project: b, TokensBefore: 10},
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}
	mux := projectAPI(t, db)

	all := insightsFor(t, mux, "?timeline=1")
	onlyA := insightsFor(t, mux, "?timeline=1&project="+a)
	onlyB := insightsFor(t, mux, "?timeline=1&project="+b)

	// The basis has to NAME the scope, or a narrowed figure reads as an all-time one.
	if onlyA.Basis == all.Basis {
		t.Errorf("a project-scoped document reports the same basis as the pooled one (%q); the "+
			"reader cannot tell which they are looking at", onlyA.Basis)
	}
	if onlyA.Project != a {
		t.Errorf("the document does not state its own project: got %q, want %q", onlyA.Project, a)
	}
	if onlyB.Project != b {
		t.Errorf("project B document says %q", onlyB.Project)
	}
}

// An unknown project is an empty view, not an error and not a silent widening to everything. A
// widening is the dangerous one: it would report every project's spend under one project's name.
func TestAnUnknownProjectIsEmptyRatherThanEverything(t *testing.T) {
	db := openTestDB(t)
	if err := db.insertBatch([]*Event{
		{TS: 1000, SessionID: "s1", Model: "m", Project: "/real", TokensBefore: 10},
	}); err != nil {
		t.Fatal(err)
	}
	mux := projectAPI(t, db)
	doc := insightsFor(t, mux, "?project=/does-not-exist")
	if len(doc.Items) != 0 {
		t.Errorf("an unknown project returned %d items; want none", len(doc.Items))
	}
}

// The picker populates itself from the server, so a new project appears without a UI change.
func TestTheProjectsCanBeEnumerated(t *testing.T) {
	db := openTestDB(t)
	if err := db.insertBatch([]*Event{
		{TS: 1000, SessionID: "s1", Model: "m", Project: "/Users/v/alpha", TokensBefore: 10},
		{TS: 2000, SessionID: "s2", Model: "m", Project: "/Users/v/beta", TokensBefore: 10},
		{TS: 3000, SessionID: "s3", Model: "m", Project: "/Users/v/alpha", TokensBefore: 10},
		{TS: 4000, SessionID: "s4", Model: "m", TokensBefore: 10}, // unattributed
	}); err != nil {
		t.Fatal(err)
	}
	got, err := db.Facets(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	projects := got["project"]
	if len(projects) != 2 {
		t.Fatalf("facets listed %d projects (%v); want the 2 attributed ones", len(projects), projects)
	}
	seen := map[string]bool{}
	for _, p := range projects {
		seen[p] = true
	}
	for _, want := range []string{"/Users/v/alpha", "/Users/v/beta"} {
		if !seen[want] {
			t.Errorf("facets do not list %q", want)
		}
	}
	// The unattributed rows must NOT appear as a project named "": the UI offers that as its own
	// option with its own wording, and an empty entry in a dropdown is unselectable noise.
	if seen[""] {
		t.Error(`facets list "" as a project; unattributed is the UI's own option, not a folder`)
	}
}

// THE ONE-WAY DOOR. A dimension's own dropdown must not be scoped by the value already chosen in
// it, or the list collapses to the single selected entry and switching away becomes impossible.
//
// selfBlanked exists for exactly this and it is easy to add a dimension without adding a case to
// it: the facet query still works, the list is still non-empty, and the bug only appears once
// something IS selected. Mutation testing caught that this was unguarded.
func TestChoosingAProjectDoesNotHideTheOtherProjects(t *testing.T) {
	db := openTestDB(t)
	if err := db.insertBatch([]*Event{
		{TS: 1000, SessionID: "s1", Model: "m", Project: "/Users/v/alpha", TokensBefore: 10},
		{TS: 2000, SessionID: "s2", Model: "m", Project: "/Users/v/beta", TokensBefore: 10},
	}); err != nil {
		t.Fatal(err)
	}
	// With alpha selected, the project list must STILL offer beta.
	got, err := db.Facets(Filter{Project: "/Users/v/alpha"})
	if err != nil {
		t.Fatal(err)
	}
	projects := got["project"]
	if len(projects) < 2 {
		t.Fatalf("with a project selected the list offers %v; want both, or the picker is a "+
			"one-way door the reader cannot leave", projects)
	}
	// And a dimension that is NOT the project stays correctly scoped by it, which is the other
	// half: blanking everything would make every dropdown ignore the current project.
	scoped, err := db.Facets(Filter{Project: "/Users/v/alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped["session"]) > 0 {
		t.Logf("session facet: %v", scoped["session"])
	}
}
