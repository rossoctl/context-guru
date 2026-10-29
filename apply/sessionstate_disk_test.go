package apply_test

import (
	"path/filepath"
	"testing"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/store"
)

// After a restart (a new store on the same file), the first request of a session restores the
// session's state before the pipeline reads it. A session that does not send a request is not
// restored.
func TestBodyOptsLoadsSessionStateFromDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	open := func() (*store.Memory, *store.Disk) {
		d, err := store.OpenDisk(path, store.DiskOptions{})
		if err != nil {
			t.Fatal(err)
		}
		m := store.NewMemory(store.Options{StateTTLSeconds: 86400})
		m.AttachDisk(d)
		return m, d
	}
	m1, d1 := open()
	apply.RecordBilledInput(m1, "sess-769", 946000)
	m1.Put(store.SumPrefix+"sess-769", []byte("CHECKPOINT"))
	m1.Put(store.SumPrefix+"never-returns", []byte("X"))
	d1.Close()

	m2, d2 := open()
	defer d2.Close()
	if _, ok := m2.Get(store.BilledPrefix + "sess-769"); ok {
		t.Fatal("state in memory before any request")
	}
	res := runTurn(t, apply.Opts{Session: "sess-769"}, m2, compactBody(t, "a", 6))
	if res.Trace.Session != "sess-769" {
		t.Fatalf("session = %q", res.Trace.Session)
	}
	if b, ok := m2.Get(store.BilledPrefix + "sess-769"); !ok || string(b) != "946000" {
		t.Fatalf("billed input not restored by the first request: %q %v", b, ok)
	}
	if b, ok := m2.Get(store.SumPrefix + "sess-769"); !ok || string(b) != "CHECKPOINT" {
		t.Fatalf("checkpoint not restored: %q %v", b, ok)
	}
	if _, ok := m2.Get(store.SumPrefix + "never-returns"); ok {
		t.Fatal("a session that sent no request was loaded")
	}
}
