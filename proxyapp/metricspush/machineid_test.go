package metricspush

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMachineIDIsStableAcrossRestarts(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "machine-id")
	a, err := MachineID(p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := MachineID(p)
	if err != nil || a != b {
		t.Fatalf("second call = %q, %v; want the same ID %q", b, err, a)
	}
	if !validID(a) {
		t.Fatalf("ID %q is not 32 hex digits", a)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, want 0600", st.Mode().Perm())
	}
}

func TestMachineIDDiffersBetweenInstalls(t *testing.T) {
	a, _ := MachineID(filepath.Join(t.TempDir(), "id"))
	b, _ := MachineID(filepath.Join(t.TempDir(), "id"))
	if a == b {
		t.Fatalf("two installs got the same ID %q", a)
	}
}

func TestMachineIDReplacesAGarbledFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "id")
	os.WriteFile(p, []byte("my-laptop-name\n"), 0o600)
	id, err := MachineID(p)
	if err != nil || !validID(id) || id == "my-laptop-name" {
		t.Fatalf("id=%q err=%v, want a fresh valid ID", id, err)
	}
}

func TestMachineIDWithNoPathStillGivesAnID(t *testing.T) {
	id, err := MachineID("")
	if err == nil || !validID(id) {
		t.Fatalf("id=%q err=%v, want a valid ID and an error that says it is not kept", id, err)
	}
}

func TestMachineIDWhenTheFileCannotBeWritten(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	os.WriteFile(blocker, nil, 0o600)
	id, err := MachineID(filepath.Join(blocker, "id")) // "file" is not a directory
	if err == nil || !validID(id) {
		t.Fatalf("id=%q err=%v, want a valid ID and an error", id, err)
	}
}
