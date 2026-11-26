package dash

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// The Bob backend label is written, read back and served on /api/requests.
func TestBackendLabelRoundTripsToAPIRequests(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	e := mkEvent(time.Now().UnixMilli(), "s1", "premium-ide", 100, 100)
	e.Backend = "B"
	seed(t, rec, e)
	_, body := get(t, a, "/api/requests", "")
	rows, _ := body["requests"].([]any)
	if len(rows) != 1 {
		t.Fatalf("/api/requests rows = %v", body)
	}
	if got := rows[0].(map[string]any)["backend"]; got != "B" {
		t.Fatalf("backend = %v, want B", got)
	}
}

// A database from before the column opens, gains it, and its old row reads "".
func TestAnExistingDatabaseGainsTheBackendColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.insertBatch([]*Event{mkEvent(1, "s", "m", 10, 10)}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`ALTER TABLE requests DROP COLUMN backend`); err != nil {
		t.Fatalf("simulate an old file: %v", err)
	}
	raw.Close()
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopening a database without the column: %v", err)
	}
	defer db2.Close()
	old, err := db2.Request(1, false)
	if err != nil {
		t.Fatal(err)
	}
	if old.Backend != "" {
		t.Fatalf("old row backend = %q, want empty", old.Backend)
	}
	e := mkEvent(2, "s", "m", 10, 10)
	e.Backend = "A"
	if err := db2.insertBatch([]*Event{e}); err != nil {
		t.Fatal(err)
	}
	if got, err := db2.Request(2, false); err != nil || got.Backend != "A" {
		t.Fatalf("new row backend = %v, err %v; want A", got, err)
	}
}

// Only a known label is stored. Anything else is stored as "".
func TestOnlyAKnownBackendLabelIsStored(t *testing.T) {
	for in, want := range map[string]string{"A": "A", "B": "B", "other": "other", "": "", "a": "", "C": "", "chatcmpl-x": ""} {
		if got := backendOf(in); got != want {
			t.Errorf("backendOf(%q) = %q, want %q", in, got, want)
		}
	}
}
