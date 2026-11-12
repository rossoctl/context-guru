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

// #1104 round 1: the hosted table refuses the keys with no session id (cg:swx:, cg:xres:), so two
// tenants never share a row, and every key it accepts starts with the tenant-scoped session id.
func TestHostedSessionStateRefusesKeysWithNoSessionID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	open := func() (*sql.DB, *store.Disk, *store.Memory) {
		db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
		if err != nil {
			t.Fatal(err)
		}
		d := openHostedSessionState(db, time.Hour)
		m := store.NewMemory(store.Options{})
		m.AttachDisk(d)
		return db, d, m
	}
	db, d, m := open()
	defer db.Close()
	// Tenant A's marker ref, keyed by a content hash only; and a session-keyed metadata key.
	m.Put(store.SweepExpandPrefix+"hash1", []byte(`{"owners":["tA:thread"]}`))
	m.Put(store.XResultPrefix+"hash2", []byte("x"))
	m.Put(store.SweepKeepPrefix+"tA:thread", []byte("1"))
	d.Flush()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM session_state`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("rows = %d, want 1 (the session-keyed key only)", n)
	}
	d.Close()
	// The restart: tenant B's store cannot read A's marker ref.
	db2, d2, m2 := open()
	defer db2.Close()
	defer d2.Close()
	if v, ok := m2.Get(store.SweepExpandPrefix + "hash1"); ok {
		t.Fatalf("tenant B read a marker ref after a restart: %q", v)
	}
}
