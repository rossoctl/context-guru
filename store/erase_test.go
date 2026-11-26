package store

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/rossoctl/context-guru/internal/testutil"
)

// An erased session is gone from memory and from disk: a new store on the same file (a restart)
// loads nothing. Its other threads go too, and another session stays.
func TestEraseSessionSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	m1, d1 := newDiskStore(t, path, DiskOptions{}, stateOpts())
	m1.Put(SumPrefix+"s1", []byte("SUMMARY"))
	m1.Put(BilledPrefix+"s1\x00t2", []byte("946000"))
	m1.Put(SumPrefix+"s1x", []byte("OTHER SESSION, SAME STEM")) // not s1: no boundary after "s1"
	m1.Put(SumPrefix+"s2", []byte("OTHER"))
	d1.Flush()

	n, err := m1.EraseSession("s1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("EraseSession deleted %d rows, want 2", n)
	}
	if _, ok := m1.Get(SumPrefix + "s1"); ok {
		t.Error("the erased summary is still in memory")
	}
	keys := rowKeys(t, d1)
	if keys[SumPrefix+"s1"] || keys[BilledPrefix+"s1\x00t2"] {
		t.Errorf("erased rows are still on disk: %v", keys)
	}
	if !keys[SumPrefix+"s1x"] || !keys[SumPrefix+"s2"] {
		t.Errorf("another session's rows went: %v", keys)
	}
	if err := d1.Close(); err != nil {
		t.Fatal(err)
	}

	m2, d2 := newDiskStore(t, path, DiskOptions{}, stateOpts())
	defer d2.Close()
	m2.LoadSession("s1")
	if got, ok := m2.Get(SumPrefix + "s1"); ok {
		t.Errorf("a new store loads the erased summary back: %q", got)
	}
	m2.LoadSession("s2")
	if got, ok := m2.Get(SumPrefix + "s2"); !ok || string(got) != "OTHER" {
		t.Errorf("the other session after a restart = %q, %v; want OTHER", got, ok)
	}
}

// A write still waiting for the disk when the session is erased never reaches it.
func TestEraseSessionDropsPendingWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	m, d := newDiskStore(t, path, DiskOptions{}, stateOpts())
	defer d.Close()
	m.Put(SumPrefix+"s1", []byte("QUEUED")) // pending: no flush yet
	if _, err := m.EraseSession("s1"); err != nil {
		t.Fatal(err)
	}
	if keys := rowKeys(t, d); keys[SumPrefix+"s1"] { // rowKeys flushes first
		t.Error("a write queued before the erase reached the disk after it")
	}
}

// A flush that took its batch before the erase, and writes it during the erase, must not leave
// the rows behind: EraseSession waits for it, then deletes. The test holds flushMu as such a flush
// does, and writes the row only after the erase has cleared memory.
func TestEraseSessionWaitsForARunningFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	m, d := newDiskStore(t, path, DiskOptions{}, stateOpts())
	defer d.Close()
	m.Put(SumPrefix+"s1", []byte("IN FLIGHT"))

	d.flushMu.Lock() // a flush is running
	d.mu.Lock()
	batch := d.pending // and it took its batch
	d.pending = map[string]pendingRow{}
	d.mu.Unlock()

	done := make(chan error, 1)
	go func() { _, err := m.EraseSession("s1"); done <- err }()
	testutil.WaitFor(t, "the erase clears memory", func() bool {
		_, ok := m.Get(SumPrefix + "s1")
		return !ok
	})
	for k, r := range batch { // the running flush writes its old batch
		if _, err := d.db.Exec(`INSERT INTO session_state(key,val,updated) VALUES(?,?,?)`,
			[]byte(k), r.val, r.updated); err != nil {
			t.Fatal(err)
		}
	}
	d.flushMu.Unlock() // the flush ends
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if keys := rowKeys(t, d); keys[SumPrefix+"s1"] {
		t.Error("a flush running during the erase wrote the row back")
	}
}

// A LoadSession that read the rows before an erase, and puts them in memory after it, keeps none:
// the rows are the erased ones. The next load reads the disk again.
func TestEraseSessionDuringALoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	m1, d1 := newDiskStore(t, path, DiskOptions{}, stateOpts())
	m1.Put(SumPrefix+"s1", []byte("SUMMARY"))
	if err := d1.Close(); err != nil {
		t.Fatal(err)
	}

	m, d := newDiskStore(t, path, DiskOptions{}, stateOpts())
	defer d.Close()
	m.afterLoadRead = func() {
		m.afterLoadRead = nil
		if _, err := m.EraseSession("s1"); err != nil {
			t.Error(err)
		}
	}
	m.LoadSession("s1")
	if got, ok := m.Get(SumPrefix + "s1"); ok {
		t.Errorf("a load that raced the erase put the erased summary in memory: %q", got)
	}
	m.Put(SumPrefix+"s1", []byte("NEW"))
	d.Flush()
	m.mu.Lock()
	_, marked := m.loadedAt["s1"]
	m.mu.Unlock()
	if marked {
		t.Error("the raced load left its mark, so the session's next request would not read the disk")
	}
}

// EraseAll removes every state row, the global namespaces included, and every state key in memory.
func TestEraseAll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	m, d := newDiskStore(t, path, DiskOptions{}, stateOpts())
	defer d.Close()
	m.Put(SumPrefix+"s1", []byte("A"))
	m.Put(SumPrefix+"s2", []byte("B"))
	m.Put(XResultPrefix+"hash", []byte("REWRITE"))
	d.Flush()
	m.Put(BilledPrefix+"s3", []byte("1")) // pending

	n, err := m.EraseAll()
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("EraseAll deleted %d rows, want 3", n)
	}
	if keys := rowKeys(t, d); len(keys) != 0 {
		t.Errorf("rows after EraseAll: %v", keys)
	}
	for _, k := range []string{SumPrefix + "s1", XResultPrefix + "hash", BilledPrefix + "s3"} {
		if _, ok := m.Get(k); ok {
			t.Errorf("%q is still in memory after EraseAll", k)
		}
	}
}

// A hosted table is shared by every tenant: EraseAll clears memory and leaves the table.
func TestEraseAllRefusedOnAHostedTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	m, d := newDiskStore(t, path, DiskOptions{Hosted: true}, stateOpts())
	defer d.Close()
	m.Put(BilledPrefix+"t1:s1", []byte("1"))
	d.Flush()
	if _, err := m.EraseAll(); !errors.Is(err, ErrHostedEraseAll) {
		t.Fatalf("EraseAll on a hosted table: err = %v, want ErrHostedEraseAll", err)
	}
	if keys := rowKeys(t, d); !keys[BilledPrefix+"t1:s1"] {
		t.Error("EraseAll deleted a hosted table's rows")
	}
}

// With no disk, an erase is a memory erase.
func TestEraseWithoutADisk(t *testing.T) {
	m := NewMemory(Options{})
	m.Put(SumPrefix+"s1", []byte("A"))
	if n, err := m.EraseSession("s1"); err != nil || n != 0 {
		t.Fatalf("EraseSession = %d, %v; want 0, nil", n, err)
	}
	if _, ok := m.Get(SumPrefix + "s1"); ok {
		t.Error("the summary is still in memory")
	}
	var nilMem *Memory
	if _, err := nilMem.EraseAll(); err != nil {
		t.Error(err)
	}
}
