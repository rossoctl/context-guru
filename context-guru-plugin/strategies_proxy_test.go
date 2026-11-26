package plugin

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rossoctl/context-guru/config"
)

// The plugin keeps no strategy table: it asks the proxy binary (`context-guru-proxy strategies`).
// These helpers give a test a binary that answers with the REAL table of this checkout, printed by
// config.StrategiesJSON, so a test and the shipped proxy cannot disagree about it.

// strategiesStubSnippet is the shell line a fake proxy script adds to answer `strategies`.
func strategiesStubSnippet(t *testing.T) string {
	t.Helper()
	b, err := config.StrategiesJSON()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "strategies.json")
	if err := os.WriteFile(file, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return "if [ \"$1\" = strategies ]; then cat '" + file + "'; exit 0; fi\n"
}

// fakeStrategiesBin is a proxy binary that only answers `strategies` (and --version).
func fakeStrategiesBin(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "context-guru-proxy")
	script := "#!/usr/bin/env bash\n" +
		"if [ \"$1\" = --version ]; then echo 'context-guru-proxy v0.7.1 (commit none)'; exit 0; fi\n" +
		strategiesStubSnippet(t) + "exit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
