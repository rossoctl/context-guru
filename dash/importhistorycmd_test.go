package dash

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// The import as a command, shared by `context-guru-proxy import-history` (which install.sh runs once) and
// cmd/cg-import-history. It is tested here because its exit code and its one-line summary are the contract
// install.sh reads.

func TestTheImportCommandAppliesOnceAndASecondRunImportsNothing(t *testing.T) {
	home := t.TempDir()
	writeJSONL(t, home, "-Users-v-proj", "s-cmd",
		turnLine("s-cmd", "/Users/v/proj", daysAgo(9), 100, 10),
		turnLine("s-cmd", "/Users/v/proj", daysAgo(8), 100, 10))
	dbPath := filepath.Join(t.TempDir(), "d.db")
	args := []string{"-db", dbPath, "-home", home, "-apply"}

	var out, errb bytes.Buffer
	if rc := RunImportHistory(args, &out, &errb); rc != 0 {
		t.Fatalf("first run rc=%d stderr=%q", rc, errb.String())
	}
	if !strings.Contains(out.String(), "APPLIED — 2 requests imported") {
		t.Fatalf("first run should import 2, got:\n%s", out.String())
	}
	out.Reset()
	if rc := RunImportHistory(args, &out, &errb); rc != 0 {
		t.Fatalf("second run rc=%d stderr=%q", rc, errb.String())
	}
	if !strings.Contains(out.String(), "APPLIED — 0 requests imported") {
		t.Fatalf("second run must import nothing, got:\n%s", out.String())
	}
}

func TestTheImportCommandWithoutADatabaseIsAUsageError(t *testing.T) {
	var out, errb bytes.Buffer
	if rc := RunImportHistory(nil, &out, &errb); rc != 2 {
		t.Fatalf("rc=%d, want 2 (usage)", rc)
	}
	if !strings.Contains(errb.String(), "usage:") {
		t.Fatalf("stderr should carry usage, got %q", errb.String())
	}
}
