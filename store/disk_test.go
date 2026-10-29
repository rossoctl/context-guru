package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newDiskStore(t *testing.T, path string, o DiskOptions, opt Options) (*Memory, *Disk) {
	t.Helper()
	d, err := OpenDisk(path, o)
	if err != nil {
		t.Fatalf("OpenDisk: %v", err)
	}
	m := NewMemory(opt)
	m.AttachDisk(d)
	return m, d
}

func stateOpts() Options { return Options{StateTTLSeconds: int(DefaultStateTTL / time.Second)} }

func rowKeys(t *testing.T, d *Disk) map[string]bool {
	t.Helper()
	d.Flush()
	rows, err := d.db.Query(`SELECT key FROM session_state`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var k []byte
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		out[string(k)] = true
	}
	return out
}

// A checkpoint written before a restart (new store, same file) is served by the first request after.
func TestDiskCheckpointSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	m1, d1 := newDiskStore(t, path, DiskOptions{}, stateOpts())
	m1.Put(SumPrefix+"s1", []byte("SUMMARY BYTES"))
	m1.Put(BilledPrefix+"s1", []byte("946000"))
	if err := d1.Close(); err != nil {
		t.Fatal(err)
	}

	m2, d2 := newDiskStore(t, path, DiskOptions{}, stateOpts())
	defer d2.Close()
	if _, ok := m2.Get(SumPrefix + "s1"); ok {
		t.Fatal("state must not be in memory before the session's first request")
	}
	m2.LoadSession("s1")
	got, ok := m2.Get(SumPrefix + "s1")
	if !ok || string(got) != "SUMMARY BYTES" {
		t.Fatalf("checkpoint after restart = %q, %v", got, ok)
	}
	if b, ok := m2.Get(BilledPrefix + "s1"); !ok || string(b) != "946000" {
		t.Fatalf("billed input after restart = %q, %v", b, ok)
	}
}

// A session idle past the memory lifetime loads from disk on its next request.
func TestDiskLoadsAfterMemoryTTL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	now := time.Now()
	m, d := newDiskStore(t, path, DiskOptions{}, stateOpts())
	defer d.Close()
	m.SetClock(func() time.Time { return now })
	m.LoadSession("s1")
	m.Put(SumPrefix+"s1", []byte("CP"))
	d.Flush()                     // the real flusher has run long before a 24 h pause
	now = now.Add(25 * time.Hour) // past the 24 h memory lifetime
	if _, ok := m.Get(SumPrefix + "s1"); ok {
		t.Fatal("entry must be gone from memory after the state lifetime")
	}
	m.LoadSession("s1")
	if got, ok := m.Get(SumPrefix + "s1"); !ok || string(got) != "CP" {
		t.Fatalf("after idle: %q, %v", got, ok)
	}
}

// The memory lifetime of a state key is the state lifetime; other keys keep the base one.
func TestStateTTLAppliesOnlyToStateKeys(t *testing.T) {
	now := time.Now()
	m := NewMemory(Options{StateTTLSeconds: 24 * 3600})
	m.SetClock(func() time.Time { return now })
	m.Put(SumPrefix+"s", []byte("x"))
	m.Put("cg:keep:s:abc", []byte("x"))
	now = now.Add(5 * time.Hour) // past DefaultTTL (2.8 h), inside 24 h
	if _, ok := m.Get(SumPrefix + "s"); !ok {
		t.Fatal("state key must live 24 h")
	}
	if _, ok := m.Get("cg:keep:s:abc"); ok {
		t.Fatal("a non-state key keeps the base lifetime")
	}
}

// With no state lifetime set, today's 2.8 h holds and no disk is used.
func TestMemoryOnlyDefaultsUnchanged(t *testing.T) {
	now := time.Now()
	m := NewMemory(Options{})
	m.SetClock(func() time.Time { return now })
	m.Put(SumPrefix+"s", []byte("x"))
	m.LoadSession("s") // no disk: no-op
	now = now.Add(DefaultTTL + time.Second)
	if _, ok := m.Get(SumPrefix + "s"); ok {
		t.Fatal("default state lifetime must stay DefaultTTL")
	}
}

// A session that never returns is never loaded: opening the store reads nothing.
func TestNoLoadAtStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	m1, d1 := newDiskStore(t, path, DiskOptions{}, stateOpts())
	m1.Put(SumPrefix+"dead", []byte("x"))
	d1.Close()
	m2, d2 := newDiskStore(t, path, DiskOptions{}, stateOpts())
	defer d2.Close()
	m2.mu.Lock()
	n := len(m2.items)
	m2.mu.Unlock()
	if n != 0 {
		t.Fatalf("startup loaded %d entries", n)
	}
	m2.LoadSession("other") // another session returns; "dead" stays on disk
	m2.mu.Lock()
	n = len(m2.items)
	m2.mu.Unlock()
	if n != 0 {
		t.Fatalf("loading another session pulled %d entries", n)
	}
}

// One disk read per session per lifetime.
func TestLoadSessionReadsOncePerLifetime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	m1, d1 := newDiskStore(t, path, DiskOptions{}, stateOpts())
	m1.Put(SumPrefix+"s", []byte("v1"))
	d1.Close()
	m2, d2 := newDiskStore(t, path, DiskOptions{}, stateOpts())
	defer d2.Close()
	m2.LoadSession("s")
	// Change the row behind the store's back: a second load must not read it.
	if _, err := d2.db.Exec(`UPDATE session_state SET val = ?`, []byte("v2")); err != nil {
		t.Fatal(err)
	}
	m2.mu.Lock()
	delete(m2.items, SumPrefix+"s")
	m2.ll.Init()
	m2.mu.Unlock()
	m2.LoadSession("s")
	if _, ok := m2.Get(SumPrefix + "s"); ok {
		t.Fatal("second LoadSession inside the lifetime must not read the disk again")
	}
}

// At "metadata only", no content key reaches the file.
func TestMetadataOnlyWritesNoContentKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	m, d := newDiskStore(t, path, DiskOptions{MetadataOnly: true}, stateOpts())
	defer d.Close()
	for _, p := range contentPrefixes {
		m.Put(p+"s", []byte("SECRET TEXT"))
	}
	for _, p := range metadataPrefixes {
		m.Put(p+"s", []byte("1"))
	}
	keys := rowKeys(t, d)
	for _, p := range contentPrefixes {
		if keys[p+"s"] {
			t.Errorf("content key %s reached the file", p)
		}
	}
	for _, p := range metadataPrefixes {
		if !keys[p+"s"] {
			t.Errorf("metadata key %s missing from the file", p)
		}
	}
	// And a load must not bring one back either.
	m2 := NewMemory(stateOpts())
	m2.AttachDisk(d)
	m2.LoadSession("s")
	if _, ok := m2.Get(SumPrefix + "s"); ok {
		t.Fatal("content key restored at metadata-only")
	}
}

// At "everything", the content keys are written too.
func TestEverythingWritesContentKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	m, d := newDiskStore(t, path, DiskOptions{}, stateOpts())
	defer d.Close()
	for _, p := range contentPrefixes {
		m.Put(p+"s", []byte("x"))
	}
	keys := rowKeys(t, d)
	for _, p := range contentPrefixes {
		if !keys[p+"s"] {
			t.Errorf("content key %s missing at everything", p)
		}
	}
}

// At "off" (no path), no file is created.
func TestOffCreatesNoFile(t *testing.T) {
	dir := t.TempDir()
	m := NewMemory(stateOpts())
	m.Put(SumPrefix+"s", []byte("x"))
	m.LoadSession("s")
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("files created with the disk store off: %v", ents)
	}
	if _, err := OpenDisk("", DiskOptions{}); err == nil {
		t.Fatal("an empty path must not open a disk store")
	}
}

// The rewind stash and other keys never reach the file.
func TestStashNeverWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	m, d := newDiskStore(t, path, DiskOptions{}, stateOpts())
	defer d.Close()
	if !m.PutStash("deadbeefdeadbeef", []byte("ORIGINAL TOOL OUTPUT")) {
		t.Fatal("stash refused")
	}
	m.Put("cg:keep:s:abc", []byte("x"))
	m.Put("cg:own:s:abc", []byte("x"))
	m.Put("cafe", []byte("plain payload via Put"))
	m.Put(SumPrefix+"s", []byte("x"))
	keys := rowKeys(t, d)
	if len(keys) != 1 || !keys[SumPrefix+"s"] {
		t.Fatalf("file holds %v, want only the checkpoint", keys)
	}
}

// A global key (no session id) is read from disk on a miss.
func TestGlobalKeyFallsBackToDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	m1, d1 := newDiskStore(t, path, DiskOptions{}, stateOpts())
	m1.Put(XResultPrefix+"abc123", []byte("paid rewrite"))
	d1.Close()
	m2, d2 := newDiskStore(t, path, DiskOptions{}, stateOpts())
	defer d2.Close()
	if got, ok := m2.Get(XResultPrefix + "abc123"); !ok || string(got) != "paid rewrite" {
		t.Fatalf("global key after restart = %q, %v", got, ok)
	}
	if _, ok := m2.Get(XResultPrefix + "nope"); ok {
		t.Fatal("a missing global key must stay missing")
	}
}

// A session id that is a prefix of another does not pull the other's rows.
func TestLoadSessionBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	m1, d1 := newDiskStore(t, path, DiskOptions{}, stateOpts())
	m1.Put(SumPrefix+"abc", []byte("A"))
	m1.Put(SumPrefix+"abcdef", []byte("B"))
	m1.Put(SumPrefix+"abc\x00sub", []byte("T"))
	d1.Close()
	m2, d2 := newDiskStore(t, path, DiskOptions{}, stateOpts())
	defer d2.Close()
	m2.LoadSession("abc")
	if _, ok := m2.Get(SumPrefix + "abcdef"); ok {
		t.Fatal("loaded a different session whose id starts with this one")
	}
	if _, ok := m2.Get(SumPrefix + "abc\x00sub"); !ok {
		t.Fatal("the thread key of the session must load")
	}
}

// Fail open: a corrupt file, an unwritable path, and a closed database never fail or panic.
func TestDiskFailsOpen(t *testing.T) {
	dir := t.TempDir()
	// Not a database.
	bad := filepath.Join(dir, "bad.db")
	if err := os.WriteFile(bad, []byte("this is not sqlite, it is a long enough text to fill a header....................................."), 0o600); err != nil {
		t.Fatal(err)
	}
	if d, err := OpenDisk(bad, DiskOptions{}); err == nil {
		// The driver may open lazily; then every operation must fail soft.
		m := NewMemory(stateOpts())
		m.AttachDisk(d)
		m.Put(SumPrefix+"s", []byte("x"))
		m.LoadSession("s")
		d.Flush()
		if got, ok := m.Get(SumPrefix + "s"); !ok || string(got) != "x" {
			t.Fatal("memory must keep working over a corrupt file")
		}
		d.Close()
	}
	// A directory that does not exist.
	if _, err := OpenDisk(filepath.Join(dir, "no", "such", "dir", "s.db"), DiskOptions{}); err == nil {
		t.Fatal("expected an error for an unwritable path")
	}
	// A database closed under the store.
	m, d := newDiskStore(t, filepath.Join(dir, "ok.db"), DiskOptions{}, stateOpts())
	m.Put(SumPrefix+"s", []byte("x"))
	d.db.Close()
	m.Put(SumPrefix+"s", []byte("y"))
	d.Flush()
	m.LoadSession("other")
	m.Get(XResultPrefix + "g")
	if got, ok := m.Get(SumPrefix + "s"); !ok || string(got) != "y" {
		t.Fatal("memory must keep working when the disk fails")
	}
	if d.Errors() == 0 {
		t.Fatal("the failure must be counted")
	}
	d.Close()
}

// A read-only file: writes fail, reads of what is there still work, nothing panics.
func TestDiskReadOnlyFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	path := filepath.Join(t.TempDir(), "state.db")
	m1, d1 := newDiskStore(t, path, DiskOptions{}, stateOpts())
	m1.Put(SumPrefix+"s", []byte("x"))
	d1.Close()
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	if d, err := OpenDisk(path, DiskOptions{}); err == nil {
		m := NewMemory(stateOpts())
		m.AttachDisk(d)
		m.Put(SumPrefix+"t", []byte("y"))
		d.Flush()
		m.LoadSession("t")
		if got, ok := m.Get(SumPrefix + "t"); !ok || string(got) != "y" {
			t.Fatal("memory must keep working over a read-only file")
		}
		d.Close()
	}
}

// The file is created with mode 0600.
func TestDiskFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	_, d := newDiskStore(t, path, DiskOptions{}, stateOpts())
	defer d.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

// Rows older than the retention are deleted.
func TestDiskRetentionSweep(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	m, d := newDiskStore(t, path, DiskOptions{Retention: time.Hour}, stateOpts())
	defer d.Close()
	m.Put(SumPrefix+"s", []byte("x"))
	d.Flush()
	d.sweepAt(time.Now().Add(2 * time.Hour))
	if keys := rowKeys(t, d); len(keys) != 0 {
		t.Fatalf("rows past the retention remain: %v", keys)
	}
}

// A write does not wait for the disk: Put only queues.
func TestPutDoesNotWriteSynchronously(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	m, d := newDiskStore(t, path, DiskOptions{FlushEvery: time.Hour}, stateOpts())
	defer d.Close()
	m.Put(SumPrefix+"s", []byte("x"))
	var n int
	if err := d.db.QueryRow(`SELECT count(*) FROM session_state`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("Put wrote to the file inside the call")
	}
	d.mu.Lock()
	q := len(d.pending)
	d.mu.Unlock()
	if q != 1 {
		t.Fatalf("pending = %d, want 1", q)
	}
}

// The loaded-session record has a hard cap and drops the oldest id on insert.
func TestLoadedRecordIsCapped(t *testing.T) {
	m, d := newDiskStore(t, filepath.Join(t.TempDir(), "state.db"), DiskOptions{}, stateOpts())
	defer d.Close()
	for i := 0; i < maxLoaded+100; i++ {
		m.LoadSession(fmt.Sprintf("s%d", i))
	}
	m.mu.Lock()
	n, o := len(m.loadedAt), len(m.loadedOrder)
	_, oldest := m.loadedAt["s0"]
	_, newest := m.loadedAt[fmt.Sprintf("s%d", maxLoaded+99)]
	m.mu.Unlock()
	if n > maxLoaded || o > maxLoaded {
		t.Fatalf("record grew to %d / %d, cap %d", n, o, maxLoaded)
	}
	if oldest || !newest {
		t.Fatalf("oldest kept = %v, newest kept = %v", oldest, newest)
	}
}

// A failed read clears the mark, so the next request retries.
func TestFailedLoadAllowsRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	m1, d1 := newDiskStore(t, path, DiskOptions{}, stateOpts())
	m1.Put(SumPrefix+"s", []byte("CP"))
	d1.Close()
	m2, d2 := newDiskStore(t, path, DiskOptions{}, stateOpts())
	// Break the read: the table is renamed away, then restored.
	if _, err := d2.db.Exec(`ALTER TABLE session_state RENAME TO session_state_x`); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	m2.SetClock(func() time.Time { return now })
	m2.LoadSession("s")
	if _, err := d2.db.Exec(`ALTER TABLE session_state_x RENAME TO session_state`); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * loadRetryAfter)
	m2.LoadSession("s")
	defer d2.Close()
	if got, ok := m2.Get(SumPrefix + "s"); !ok || string(got) != "CP" {
		t.Fatalf("retry after a failed load: %q %v", got, ok)
	}
}

// At the pending cap, an update to a key already waiting is kept; a new key is dropped.
func TestPendingCapKeepsUpdates(t *testing.T) {
	_, d := newDiskStore(t, filepath.Join(t.TempDir(), "state.db"), DiskOptions{FlushEvery: time.Hour}, stateOpts())
	defer d.Close()
	now := time.Now()
	for i := 0; i < maxPending; i++ {
		d.enqueue(fmt.Sprintf("%ss%d", SumPrefix, i), []byte("old"), now)
	}
	d.enqueue(SumPrefix+"s0", []byte("new"), now)
	d.enqueue(SumPrefix+"extra", []byte("x"), now)
	d.mu.Lock()
	defer d.mu.Unlock()
	if string(d.pending[SumPrefix+"s0"].val) != "new" {
		t.Fatal("update to a waiting key was dropped at the cap")
	}
	if _, ok := d.pending[SumPrefix+"extra"]; ok || len(d.pending) != maxPending {
		t.Fatal("a new key must be dropped at the cap")
	}
}

// Each distinct failing op is logged once, and every failure is counted.
func TestEachOpLoggedOnce(t *testing.T) {
	_, d := newDiskStore(t, filepath.Join(t.TempDir(), "state.db"), DiskOptions{}, stateOpts())
	defer d.Close()
	d.fail("a", errors.New("x"))
	d.fail("a", errors.New("x"))
	d.fail("b", errors.New("y"))
	n := 0
	d.logged.Range(func(_, _ any) bool { n++; return true })
	if n != 2 || d.Errors() != 3 {
		t.Fatalf("ops logged %d, errors %d", n, d.Errors())
	}
}

func breakTable(t *testing.T, d *Disk) {
	t.Helper()
	if _, err := d.db.Exec(`ALTER TABLE session_state RENAME TO session_state_x`); err != nil {
		t.Fatal(err)
	}
}

// A disk that keeps failing must not grow the order list: N failed loads of one session leave at
// most one entry, and many failing sessions leave none.
func TestFailedLoadsDoNotGrowTheOrderList(t *testing.T) {
	m, d := newDiskStore(t, filepath.Join(t.TempDir(), "state.db"), DiskOptions{}, stateOpts())
	defer d.Close()
	breakTable(t, d)
	now := time.Now()
	m.SetClock(func() time.Time { return now })
	for i := 0; i < 50; i++ {
		now = now.Add(2 * loadRetryAfter) // past the backoff, so every call really reads
		m.LoadSession("same")
	}
	for i := 0; i < 200; i++ {
		m.LoadSession(fmt.Sprintf("s%d", i))
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.loadedOrder) != 0 || len(m.loadedAt) != 0 {
		t.Fatalf("after failures: order %d, marks %d, want 0", len(m.loadedOrder), len(m.loadedAt))
	}
}

// After a failed read, the next requests inside the backoff do not read again; after it, they do.
func TestFailedLoadBacksOff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	m1, d1 := newDiskStore(t, path, DiskOptions{}, stateOpts())
	m1.Put(SumPrefix+"s", []byte("CP"))
	d1.Close()
	m, d := newDiskStore(t, path, DiskOptions{}, stateOpts())
	defer d.Close()
	now := time.Now()
	m.SetClock(func() time.Time { return now })
	breakTable(t, d)
	m.LoadSession("s")
	e1 := d.Errors()
	if _, err := d.db.Exec(`ALTER TABLE session_state_x RENAME TO session_state`); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Second)
	m.LoadSession("s")
	if _, ok := m.Get(SumPrefix + "s"); ok || d.Errors() != e1 {
		t.Fatal("a retry inside the backoff must not read the disk")
	}
	now = now.Add(2 * loadRetryAfter)
	m.LoadSession("s")
	if got, ok := m.Get(SumPrefix + "s"); !ok || string(got) != "CP" {
		t.Fatalf("retry after the backoff: %q %v", got, ok)
	}
}
