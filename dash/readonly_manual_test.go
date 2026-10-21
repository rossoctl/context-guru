package dash

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// A file from before the Ping button cannot be migrated through a read-only handle; it is refused
// at open with a message that says what to do, not at the first query with "no such column".
func TestOpenReadOnlyNamesAMissingManualPingColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`ALTER TABLE requests DROP COLUMN manual_ping`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	_, err = OpenReadOnly(path)
	if err == nil || !strings.Contains(err.Error(), "manual_ping") || !strings.Contains(err.Error(), "migrate") {
		t.Fatalf("OpenReadOnly = %v, want an error naming manual_ping and the fix", err)
	}
	// A current file still opens.
	cur := filepath.Join(t.TempDir(), "cur.db")
	d, _ := Open(cur)
	d.Close()
	ro, err := OpenReadOnly(cur)
	if err != nil {
		t.Fatalf("a current file: %v", err)
	}
	ro.Close()
}
