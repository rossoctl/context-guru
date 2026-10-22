package dash

import (
	"path/filepath"
	"testing"
	"time"
)

// SHUTDOWN MUST NOT LOSE INVENTORY. Recorder.Close closes the database after waiting for its goroutines; the
// inventory writer was started outside that wait, so a Close right after RecordInventory could close the DB
// under the writer's final flush and the session's inventory never reached disk. Reproduced in 22 of 50 runs
// before the fix (and caught by the e2e harness's shutdown probe). Run 50 times because it is a race.
func TestCloseRightAfterRecordingInventoryStillWritesIt(t *testing.T) {
	lost := 0
	const runs = 50
	for i := 0; i < runs; i++ {
		path := filepath.Join(t.TempDir(), "d.db")
		rec, err := NewRecorder(Options{DBPath: path})
		if err != nil {
			t.Fatal(err)
		}
		inv := ScanInventory("anthropic", ccBody(t, []string{tool("Bash", "b")}, skillsReminder,
			[]map[string]any{{"name": "Bash", "input": map[string]any{}}}))
		rec.RecordInventory("t1", "s1", 1000, inv, true, "/p")
		if err := rec.Close(); err != nil {
			t.Fatal(err)
		}
		db, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		d, _, err := db.countInventoryRows()
		db.Close()
		if err != nil {
			t.Fatal(err)
		}
		if d == 0 {
			lost++
		}
	}
	t.Logf("inventory lost on Close in %d of %d runs", lost, runs)
	if lost > 0 {
		t.Errorf("%d of %d runs wrote no inventory rows when Close followed RecordInventory", lost, runs)
	}
}

// Recording after Close starts no writer: nothing would drain it, and Close has already waited, so a goroutine
// started now would be added to a WaitGroup whose Wait has returned. The guard is that startInBackground
// refuses once the recorder is closed; this pins it, and that a late record neither panics nor blocks.
func TestRecordingInventoryAfterCloseStartsNothingAndDoesNotBlock(t *testing.T) {
	rec, err := NewRecorder(Options{DBPath: filepath.Join(t.TempDir(), "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	if rec.startInBackground(func() { t.Error("started a goroutine on a closed recorder") }) {
		t.Error("startInBackground accepted work after Close")
	}
	inv := ScanInventory("anthropic", ccBody(t, []string{tool("Bash", "b")}, skillsReminder, nil))
	done := make(chan struct{})
	go func() { rec.RecordInventory("t1", "s1", 1000, inv, true, "/p"); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RecordInventory after Close blocked")
	}
	if _, ok := invWriters.Load(rec); ok {
		t.Error("a writer was left registered for a closed recorder")
	}
}
