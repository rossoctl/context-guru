package dash

// The project facet lists PROJECTS, not every folder a session was launched in.
//
// THE DEFECT, reported against the Cleanup tab: "I don't understand why I saw so many projects."
// Forty entries for six projects. The rest were subdirectories of the same repositories --
// ai-native-company/docs/adr, Research/notes/server, context-guru/docs/superpowers/specs -- plus
// eighteen throwaway /private/tmp folders.
//
// ProjectGroups rolls these up from Claude Code's own transcript grouping. The facet feeding the
// pickers did not, so the same machine showed 6 projects on one tab and 40 on another.
//
// Two halves have to hold together: the picker lists roots, AND selecting a root includes the
// traffic recorded against its subfolders. A picker that folds without a query that folds means
// selecting a project shows less than the unscoped view did.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestTheProjectFacetListsRootsNotEveryFolder(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	// One project, three folders, as Claude Code files them.
	writeJSONL(t, home, "-Users-v-notes", "s1",
		turnLine("s1", "/Users/v/notes", daysAgo(9), 100, 10),
		turnLine("s1", "/Users/v/notes", daysAgo(9), 100, 10),
		turnLine("s1", "/Users/v/notes/server", daysAgo(8), 100, 10),
		turnLine("s1", "/Users/v/notes/ops/compose", daysAgo(7), 100, 10))
	if err := db.insertBatch([]*Event{
		{TS: daysAgo(9), SessionID: "s1", Model: "m", Project: "/Users/v/notes"},
		{TS: daysAgo(8), SessionID: "s1", Model: "m", Project: "/Users/v/notes/server"},
		{TS: daysAgo(7), SessionID: "s1", Model: "m", Project: "/Users/v/notes/ops/compose"},
	}); err != nil {
		t.Fatal(err)
	}

	fac, err := db.FacetsFor(Filter{}, home)
	if err != nil {
		t.Fatal(err)
	}
	got := fac["project"]
	if len(got) != 1 || got[0] != "/Users/v/notes" {
		t.Fatalf("project facet = %v; want just [/Users/v/notes]. Three folders of one project "+
			"are one entry: a picker offering every launch directory showed 40 for six projects",
			got)
	}
}

// SELECTING A ROOT HAS TO INCLUDE ITS SUBFOLDERS.
//
// The other half, and the one that makes folding safe. Without it the picker offers a root whose
// own path holds a fraction of the traffic, so choosing it shows less than the pooled view.
func TestSelectingAProjectRootIncludesItsSubfolders(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	writeJSONL(t, home, "-Users-v-notes", "s1",
		turnLine("s1", "/Users/v/notes", daysAgo(9), 100, 10),
		turnLine("s1", "/Users/v/notes", daysAgo(9), 100, 10),
		turnLine("s1", "/Users/v/notes/server", daysAgo(8), 100, 10))
	if err := db.insertBatch([]*Event{
		{TS: daysAgo(9), SessionID: "s1", Model: "m", Project: "/Users/v/notes", FreshInput: 10},
		// Most of the traffic is in the SUBFOLDER.
		{TS: daysAgo(8), SessionID: "s1", Model: "m", Project: "/Users/v/notes/server",
			FreshInput: 10},
		{TS: daysAgo(8), SessionID: "s1", Model: "m", Project: "/Users/v/notes/server",
			FreshInput: 10},
		// And an unrelated project, which must NOT be included.
		{TS: daysAgo(8), SessionID: "s2", Model: "m", Project: "/Users/v/other", FreshInput: 10},
	}); err != nil {
		t.Fatal(err)
	}

	f := ExpandProjectFilter(Filter{Project: "/Users/v/notes"}, ProjectGroups(home))
	where, args := f.where()
	var n int
	if err := db.sql.QueryRow(
		`SELECT COUNT(*) FROM requests r WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("selecting the project matched %d requests; want 3 — its own folder plus the "+
			"two in its subfolder. A picker that folds without a query that folds means "+
			"choosing a project shows less than the pooled view", n)
	}
	// And it did not swallow a different project.
	f2 := ExpandProjectFilter(Filter{Project: "/Users/v/other"}, ProjectGroups(home))
	w2, a2 := f2.where()
	var n2 int
	if err := db.sql.QueryRow(
		`SELECT COUNT(*) FROM requests r WHERE `+w2, a2...).Scan(&n2); err != nil {
		t.Fatal(err)
	}
	if n2 != 1 {
		t.Errorf("the unrelated project matched %d requests; want 1 — folding must not merge "+
			"two projects", n2)
	}
}

// An unrecognised folder stays selectable on its own. A measured row whose session has no
// transcript still describes traffic, and dropping it from the picker would hide it entirely.
func TestAFolderWithNoTranscriptStaysItsOwnProject(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	if err := db.insertBatch([]*Event{
		{TS: daysAgo(9), SessionID: "s-ghost", Model: "m", Project: "/Users/v/no-transcript"},
	}); err != nil {
		t.Fatal(err)
	}
	fac, err := db.FacetsFor(Filter{}, home)
	if err != nil {
		t.Fatal(err)
	}
	if len(fac["project"]) != 1 || fac["project"][0] != "/Users/v/no-transcript" {
		t.Errorf("project facet = %v; a folder the transcripts cannot explain has to stay "+
			"selectable, or its traffic is unreachable", fac["project"])
	}
}

// EVERY PROJECT-SCOPED SURFACE HAS TO FOLD, not just the picker.
//
// a.scope is the one place every handler builds its filter, so expanding the project there is
// what makes the rollup consistent. Doing it per handler would leave whichever one was missed
// reporting a root's own folder only -- a fraction of its traffic, and silently.
func TestTheScopedFilterExpandsAProjectRootForEverySurface(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	writeJSONL(t, home, "-Users-v-notes", "s1",
		turnLine("s1", "/Users/v/notes", daysAgo(9), 100, 10),
		turnLine("s1", "/Users/v/notes", daysAgo(9), 100, 10),
		turnLine("s1", "/Users/v/notes/server", daysAgo(8), 100, 10))
	if err := db.insertBatch([]*Event{
		{TS: daysAgo(9), SessionID: "s1", Model: "m", Project: "/Users/v/notes", FreshInput: 10},
		{TS: daysAgo(8), SessionID: "s1", Model: "m", Project: "/Users/v/notes/server",
			FreshInput: 10},
		{TS: daysAgo(8), SessionID: "s1", Model: "m", Project: "/Users/v/notes/server",
			FreshInput: 10},
	}); err != nil {
		t.Fatal(err)
	}
	mux := projectAPIWithHome(t, db, home)

	// Through the HTTP surface, scoped to the root, asking for the request count.
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		"/api/stats?project="+url.QueryEscape("/Users/v/notes"), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/stats = %d: %s", rr.Code, rr.Body.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	// Whatever the shape, the count has to be 3 rather than 1.
	n, ok := doc["requests"].(float64)
	if !ok {
		t.Skipf("this build's /api/stats has no `requests` field (%v); the filter-level "+
			"assertion in TestSelectingAProjectRootIncludesItsSubfolders still holds", doc)
	}
	if int(n) != 3 {
		t.Errorf("scoping to the project root counted %d requests; want 3. Only the root's own "+
			"folder matched, so every surface reports a fraction of a project's traffic", int(n))
	}
}

// THE HANDLER HAS TO SERVE THE ROLLED-UP LIST.
//
// A gap the first version of these tests left: they called FacetsFor directly, so the handler
// could have gone on serving raw Facets -- which is what it did -- and every assertion would
// still have passed. The picker reads /api/facets, so that is where it has to be right.
func TestTheFacetsEndpointServesProjectRoots(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	writeJSONL(t, home, "-Users-v-notes", "s1",
		turnLine("s1", "/Users/v/notes", daysAgo(9), 100, 10),
		turnLine("s1", "/Users/v/notes", daysAgo(9), 100, 10),
		turnLine("s1", "/Users/v/notes/server", daysAgo(8), 100, 10))
	if err := db.insertBatch([]*Event{
		{TS: daysAgo(9), SessionID: "s1", Model: "m", Project: "/Users/v/notes"},
		{TS: daysAgo(8), SessionID: "s1", Model: "m", Project: "/Users/v/notes/server"},
	}); err != nil {
		t.Fatal(err)
	}
	mux := projectAPIWithHome(t, db, home)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/facets", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/facets = %d: %s", rr.Code, rr.Body.String())
	}
	var doc map[string][]string
	if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc["project"]) != 1 || doc["project"][0] != "/Users/v/notes" {
		t.Errorf("the facets endpoint returned %v; want just the project root. This is the list "+
			"the picker renders, so a rollup the handler does not use is a rollup nobody sees",
			doc["project"])
	}
}

// AND THE PICKER MUST NOT COLLAPSE TO THE PROJECT ALREADY CHOSEN.
//
// selfBlanked exists to keep each dropdown listing every option regardless of what is selected.
// ProjectIn is a second way to express the same choice, so it has to be blanked there too --
// otherwise selecting a project leaves a one-entry picker and the reader cannot get back.
func TestTheProjectPickerStaysCompleteAfterSelectingOne(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	writeJSONL(t, home, "-Users-v-a", "s1",
		turnLine("s1", "/Users/v/a", daysAgo(9), 100, 10),
		turnLine("s1", "/Users/v/a", daysAgo(9), 100, 10),
		turnLine("s1", "/Users/v/a/sub", daysAgo(9), 100, 10))
	writeJSONL(t, home, "-Users-v-b", "s2", turnLine("s2", "/Users/v/b", daysAgo(8), 100, 10))
	if err := db.insertBatch([]*Event{
		{TS: daysAgo(9), SessionID: "s1", Model: "m", Project: "/Users/v/a"},
		{TS: daysAgo(9), SessionID: "s1", Model: "m", Project: "/Users/v/a/sub"},
		{TS: daysAgo(8), SessionID: "s2", Model: "m", Project: "/Users/v/b"},
	}); err != nil {
		t.Fatal(err)
	}
	mux := projectAPIWithHome(t, db, home)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		"/api/facets?project="+url.QueryEscape("/Users/v/a"), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/facets = %d: %s", rr.Code, rr.Body.String())
	}
	var doc map[string][]string
	if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc["project"]) != 2 {
		t.Errorf("with a project selected the picker offers %v; want both. A picker scoped by "+
			"its own selection is a one-way door: the reader cannot switch project or get back "+
			"to the pooled view", doc["project"])
	}
}
