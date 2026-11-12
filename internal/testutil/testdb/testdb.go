// Package testdb opens SQLite files for tests the way production does.
//
// Tests that open their own database with a different DSN (no WAL, no busy timeout, an unlimited
// pool) fail under CPU and disk load in ways production never does. Use Open and Reopen instead of
// sql.Open in a test.
//
// This package imports no other package of this module, so any test package can use it without an
// import cycle. For that reason it cannot call the production DSN builder (dash.dsn, which is
// private and sits in a package that imports store). It copies the same pragmas. Keep them equal
// to dash/store.go dsn and tooldefer/file.go Open.
package testdb

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite" // the driver production uses
)

// openBound is the longest Open waits for the first connection. A locked file fails with the real
// SQLite error inside this time, not with a hang.
const openBound = 10 * time.Second

// maxOpenConns is the pool size of tooldefer.Open, the smallest production pool.
const maxOpenConns = 4

// DSN returns the production DSN for a file path: busy_timeout first (the driver runs the pragmas
// in order, and journal_mode needs a lock), then WAL and synchronous NORMAL.
func DSN(path string) string {
	return "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
}

// Open opens the SQLite file at path with the production settings. It closes the database when the
// test ends. It fails the test with the real error.
func Open(t testing.TB, path string) *sql.DB {
	t.Helper()
	db, err := open(path)
	if err != nil {
		t.Fatalf("testdb: open %s: %v", path, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// OpenUnchecked is Open without the first connection. It is for a test that opens a broken file
// (corrupt, unwritable, read-only) on purpose and checks how the code under test fails. It never
// fails the test on a bad file.
func OpenUnchecked(t testing.TB, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", DSN(path))
	if err != nil {
		t.Fatalf("testdb: open %s: %v", path, err)
	}
	db.SetMaxOpenConns(maxOpenConns)
	t.Cleanup(func() { db.Close() })
	return db
}

// Reopen closes db, waits for the close to finish, and opens the same file again. Use it in a
// restart test. Close returns only after every connection is closed, so the second open sees the
// file as the first left it, with the WAL checkpointed.
func Reopen(t testing.TB, db *sql.DB, path string) *sql.DB {
	t.Helper()
	if err := db.Close(); err != nil {
		t.Fatalf("testdb: close before reopen of %s: %v", path, err)
	}
	return Open(t, path)
}

func open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", DSN(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(maxOpenConns)
	ctx, cancel := context.WithTimeout(context.Background(), openBound)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
