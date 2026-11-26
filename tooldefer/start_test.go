package tooldefer

import (
	"path/filepath"
	"testing"
	"time"
)

// ── #871 defect 3: the stored state is loaded before the first request ───

func TestWaitReadyReturnsOnceLoaded(t *testing.T) {
	f, err := OpenFile(filepath.Join(t.TempDir(), FileName), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	m := NewMem(f.DB)
	k := ProbeKey{"u", "claude-sonnet-5-5", "/v1/messages"}
	m.SaveVerdict(k, Pass, time.Now())
	p := NewProber()
	stop := m.Open(time.Hour, p)
	defer stop()
	if !m.WaitReady(5 * time.Second) {
		t.Fatal("WaitReady timed out")
	}
	if p.Verdict(k) != Pass {
		t.Fatal("the stored verdict is not loaded when WaitReady returns")
	}
	mem := NewMem(nil)
	mem.Open(0, nil)
	if !mem.WaitReady(time.Second) {
		t.Fatal("a memory-only history is not ready at once")
	}
	if NewMem(nil).WaitReady(10 * time.Millisecond) {
		t.Fatal("ready before Open")
	}
}
