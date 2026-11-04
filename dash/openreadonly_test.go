package dash

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fileDigest hashes a database file and its -wal, so a test can say "not one byte changed".
func fileDigest(t *testing.T, path string) [sha256.Size]byte {
	t.Helper()
	var all []byte
	for _, p := range []string{path, path + "-wal"} {
		b, err := os.ReadFile(p)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		all = append(all, b...)
	}
	return sha256.Sum256(all)
}

// seededFile is a real dashboard database on disk with one request row, closed again so the
// read-only handle is the only one touching it.
func seededFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cg.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	seedRequest(t, db, "t1", "s1")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// OpenReadOnly READS a live database and writes NOTHING: no migration, no version stamp, no WAL
// checkpoint. Open does all three, which is why an analysis tool beside a running proxy cannot use it.
func TestOpenReadOnlyReadsAndWritesNothing(t *testing.T) {
	path := seededFile(t)
	before := fileDigest(t, path)
	state := dirState(t, path)

	db, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	var n int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("read %d rows (%v), want the 1 seeded row", n, err)
	}
	db.checkpoint() // the one write Open makes on its own; a read-only handle must skip it
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if after := fileDigest(t, path); !bytes.Equal(before[:], after[:]) {
		t.Error("the database or its WAL changed under a read-only handle")
	}
	// SQLite needs the -shm index (and an empty -wal) even to READ a WAL database when no writer
	// is live; those are the only files a read-only open may add, and both must hold no data.
	after := dirState(t, path)
	if after["cg.db"] != state["cg.db"] {
		t.Error("the database file's mtime changed under a read-only handle")
	}
	for name := range after {
		if _, ok := state[name]; !ok && name != "cg.db-wal" && name != "cg.db-shm" {
			t.Errorf("unexpected file %s appeared beside the database", name)
		}
	}
	if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() != 0 {
		t.Errorf("-wal holds %d bytes after a read-only session", fi.Size())
	}
}

// dirState lists every file beside path with its mtime, so a test can say nothing was created
// (-wal, -shm), removed or touched.
func dirState(t *testing.T, path string) map[string]int64 {
	t.Helper()
	ents, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]int64{}
	for _, e := range ents {
		fi, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		m[e.Name()] = fi.ModTime().UnixNano()
	}
	return m
}

// A write through the handle is refused by SQLite itself, not merely avoided by this package.
func TestOpenReadOnlyRefusesWrites(t *testing.T) {
	db, err := OpenReadOnly(seededFile(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.sql.Exec(`DELETE FROM requests`); err == nil {
		t.Fatal("a DELETE succeeded through a read-only handle")
	}
}

// A database at another schema version is REFUSED, not migrated and not renamed aside: a reader
// cannot migrate, and Open's recovery (rename the file, start fresh) would be destructive here.
func TestOpenReadOnlyRefusesAnotherSchemaVersionAndLeavesTheFile(t *testing.T) {
	path := seededFile(t)
	rw, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rw.sql.Exec(`UPDATE meta SET value='999' WHERE key='schema_version'`); err != nil {
		t.Fatal(err)
	}
	rw.Close()
	before := fileDigest(t, path)

	_, err = OpenReadOnly(path)
	var vm *versionMismatch
	if !errors.As(err, &vm) {
		t.Fatalf("err = %v, want a version mismatch", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("the database was moved: %v", statErr)
	}
	if after := fileDigest(t, path); !bytes.Equal(before[:], after[:]) {
		t.Error("a refused read-only open changed the file")
	}
}

// A missing path is an error, never a fresh empty database.
func TestOpenReadOnlyRefusesAMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.db")
	if _, err := OpenReadOnly(path); err == nil {
		t.Fatal("OpenReadOnly created or accepted a missing database")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a file now exists at %s", path)
	}
}

// The analysis entry point works on a read-only handle end to end.
func TestKVCacheSuggestRunsOnAReadOnlyHandle(t *testing.T) {
	db, err := OpenReadOnly(seededFile(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.KVCacheSuggest(Filter{TenantAll: true}, KVCacheOptions{}, nil, KVCacheSimConfig{}); err != nil {
		t.Fatalf("KVCacheSuggest on a read-only handle: %v", err)
	}
}
