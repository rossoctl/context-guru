package testdb

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenUsesProductionSettings(t *testing.T) {
	db := Open(t, filepath.Join(t.TempDir(), "a.db"))
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v", mode, err)
	}
	var bt int
	if err := db.QueryRow(`PRAGMA busy_timeout`).Scan(&bt); err != nil || bt != 5000 {
		t.Fatalf("busy_timeout = %d, %v", bt, err)
	}
	if n := db.Stats().MaxOpenConnections; n != maxOpenConns {
		t.Fatalf("max open conns = %d", n)
	}
}

func TestReopenKeepsTheRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.db")
	db := Open(t, path)
	if _, err := db.Exec(`CREATE TABLE x (v INTEGER); INSERT INTO x VALUES (7)`); err != nil {
		t.Fatal(err)
	}
	db = Reopen(t, db, path)
	var v int
	if err := db.QueryRow(`SELECT v FROM x`).Scan(&v); err != nil || v != 7 {
		t.Fatalf("v = %d, %v", v, err)
	}
}

// A file locked by another connection must fail with the SQLite error inside the bound.
func TestOpenFailsWithTheRealErrorOnALockedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.db")
	// A rollback-journal file with an exclusive lock held: the switch to WAL cannot get a lock.
	locker, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close()
	locker.SetMaxOpenConns(1)
	if _, err := locker.Exec(`CREATE TABLE x (v INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := locker.Exec(`BEGIN EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = open(path)
	if err == nil {
		t.Fatal("open succeeded on a locked file")
	}
	if d := time.Since(start); d > openBound+2*time.Second {
		t.Fatalf("open took %v, want at most %v", d, openBound)
	}
	if !strings.Contains(err.Error(), "locked") && !strings.Contains(err.Error(), "busy") {
		t.Fatalf("error does not name the lock: %v", err)
	}
}
