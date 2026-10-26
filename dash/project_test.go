package dash

import (
	"strings"
	"testing"
)

// The project dimension, end to end through the store: written on an Event, readable back, and
// filterable.
//
// WHY A ROUND TRIP AND NOT THREE UNIT TESTS. A new column has to be named in four places that
// cannot disagree — the CREATE TABLE, the additive migration, the INSERT's column list with its
// placeholder count, and the Exec argument order. A wrong arg ORDER still compiles and still
// inserts; it writes the project into the next column along. Only reading the value back through a
// query catches that, which is what this does.
func TestTheProjectReachesTheStoreAndComesBack(t *testing.T) {
	db := openTestDB(t)
	const a = "/Users/v/OpenSource/context-guru"
	const b = "/Users/v/Research/notes"
	if err := db.insertBatch([]*Event{
		{TS: 1000, SessionID: "s1", Model: "m", Project: a, TokensBefore: 10},
		{TS: 2000, SessionID: "s2", Model: "m", Project: b, TokensBefore: 20},
		{TS: 3000, SessionID: "s3", Model: "m", TokensBefore: 30}, // no project: unattributed
	}); err != nil {
		t.Fatal(err)
	}

	// Read back through the Go READ PATH, not off the raw column.
	//
	// Querying `SELECT project FROM requests` directly proved nothing: the column gets a value
	// either way, so an Exec argument written one position early still passed — mutation testing
	// caught that, and this is the correction. Going through Requests()/scanRequest() means the
	// value has to arrive in the field it belongs to.
	page, err := db.Requests(Filter{}, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range page.Requests {
		got[e.SessionID] = e.Project
	}
	for sess, want := range map[string]string{"s1": a, "s2": b, "s3": ""} {
		if got[sess] != want {
			t.Errorf("session %s stored project %q; want %q", sess, got[sess], want)
		}
	}
}

// Filtering is the point of the column: a per-project view is a query, not a client-side filter,
// because the verdicts and the evidence floor are computed server-side over the filter's window.
func TestTheProjectFilterNarrowsToOneFolder(t *testing.T) {
	db := openTestDB(t)
	const a = "/Users/v/proj-a"
	const b = "/Users/v/proj-b"
	if err := db.insertBatch([]*Event{
		{TS: 1000, SessionID: "s1", Model: "m", Project: a, TokensBefore: 10},
		{TS: 2000, SessionID: "s2", Model: "m", Project: a, TokensBefore: 10},
		{TS: 3000, SessionID: "s3", Model: "m", Project: b, TokensBefore: 10},
		{TS: 4000, SessionID: "s4", Model: "m", TokensBefore: 10},
	}); err != nil {
		t.Fatal(err)
	}

	count := func(f Filter) int {
		page, err := db.Requests(f, 100, 0)
		if err != nil {
			t.Fatal(err)
		}
		return len(page.Requests)
	}
	if n := count(Filter{Project: a}); n != 2 {
		t.Errorf("project %s returned %d requests; want 2", a, n)
	}
	if n := count(Filter{Project: b}); n != 1 {
		t.Errorf("project %s returned %d requests; want 1", b, n)
	}
	// NO project set means EVERY project, which is the view the dashboard opens on. Not "the
	// unattributed ones": an empty filter field is absence, and reading it as a value would
	// silently hide every attributed request behind a default.
	if n := count(Filter{}); n != 4 {
		t.Errorf("an unset project filter returned %d requests; want all 4", n)
	}
	// An unknown project is an empty result, not an error — the documented filterFrom contract.
	if n := count(Filter{Project: "/nope"}); n != 0 {
		t.Errorf("an unknown project returned %d requests; want 0", n)
	}
}

// The canary the project's own rules ask for: drive a credential-shaped directory name through the
// WRITE path and assert it is not on disk. ProjectPath is unit-tested separately; this pins that
// the write actually calls it, which a mutation removing the call otherwise survives.
func TestACredentialShapedFolderNameNeverReachesTheRequestsTable(t *testing.T) {
	db := openTestDB(t)
	const canary = "sk-ant-api03-ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ"
	if err := db.insertBatch([]*Event{
		{TS: 1000, SessionID: "s1", Model: "m", Project: "/Users/v/" + canary, TokensBefore: 10},
	}); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := db.sql.QueryRow(`SELECT project FROM requests`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, canary) {
		t.Fatalf("the canary is on disk in the project column: %q", stored)
	}
	if stored != Redacted {
		t.Errorf("project = %q; want %q, which says a value was refused rather than absent",
			stored, Redacted)
	}
}
