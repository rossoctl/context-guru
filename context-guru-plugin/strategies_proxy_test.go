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

// strategiesLoadedLimit is the env entry that raises settings.py's 5 s limit on `context-guru-proxy
// strategies` to 15 s. A stub that only prints a file can miss 5 s on a loaded machine, and the read
// then fails (strategy_list_unavailable from install.sh). 15 s is what the other loaded-machine
// strategy tests use. A test that checks the limit itself sets its own value after this one.
const strategiesLoadedLimit = "CONTEXT_GURU_STRATEGIES_TIMEOUT=15"

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
