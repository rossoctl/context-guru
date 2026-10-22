package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// `context-guru-proxy import-history` is how install.sh imports pre-install history once: the release
// ships this one binary, so the import has to live in it. It must run and EXIT, never start serving.
func TestImportHistoryIsASubcommandThatRunsAndExits(t *testing.T) {
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
	if code != 0 || !strings.Contains(out, "APPLIED — 1 requests imported") {
		t.Fatalf("code=%d, want 0 and one imported request; output:\n%s", code, out)
	}
}
