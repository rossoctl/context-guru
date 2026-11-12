package proxyapp

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/store"
	_ "modernc.org/sqlite"
)

// #1104: the table a hosted proxy opens is metadata-only. A content key (the summary text) never
// reaches the file; a metadata key does.
func TestHostedSessionStateIsMetadataOnly(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "s.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	d := openHostedSessionState(db, time.Hour)
	if d == nil {
		t.Fatal("no disk")
	}
	defer d.Close()
	m := store.NewMemory(store.Options{})
	m.AttachDisk(d)
	m.Put(store.SumPrefix+"t1:s", []byte("summary text"))
	m.Put(store.BilledPrefix+"t1:s", []byte("1"))
	d.Flush()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM session_state`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("rows = %d, want 1 (the metadata key only)", n)
	}
}
