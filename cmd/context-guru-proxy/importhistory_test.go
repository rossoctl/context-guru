package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The history import is removed. `context-guru-proxy import-history` stays as a no-op that EXITS 0,
// because an older install.sh still calls it during a plugin/proxy version mismatch: dropping the
// name would let it fall through to the proxy's own parsing and start serving, which hangs the install.
// It must import nothing.
func TestImportHistoryIsANoOpThatExits(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "projects", "-p")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ts := time.Now().AddDate(0, 0, -3).UTC().Format(time.RFC3339Nano)
	line := `{"type":"assistant","sessionId":"s1","cwd":"/p","timestamp":"` + ts +
		`","message":{"id":"m1","model":"claude-sonnet-4-5","usage":{"input_tokens":5,"output_tokens":7}}}`
	if err := os.WriteFile(filepath.Join(dir, "s1.jsonl"), []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(t.TempDir(), "d.db")

	out, code, exited := runMain(t, 20*time.Second,
		"CG_TEST_MAIN_ARGS=import-history -db "+db+" -home "+home+" -apply")

	if !exited {
		t.Fatalf("import-history started serving instead of exiting:\n%s", out)
	}
	if code != 0 || strings.Contains(out, "imported") && strings.Contains(out, "APPLIED") {
		t.Fatalf("code=%d, want 0 and nothing imported; output:\n%s", code, out)
	}
	if !strings.Contains(out, "removed") {
		t.Errorf("import-history does not say it was removed:\n%s", out)
	}
	if _, err := os.Stat(db); err == nil {
		t.Errorf("import-history created a database at %s; a no-op must not", db)
	}
}
