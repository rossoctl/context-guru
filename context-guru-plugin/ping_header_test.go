// The install opts a session in to MANUAL pings (the Ping button, /context-guru:ping) by adding
// `x-context-guru-ping: 1` to env.ANTHROPIC_CUSTOM_HEADERS. Claude Code reads that variable as `Name: Value`
// lines separated by NEWLINES (a comma-joined value arrives as one corrupted header), and the variable may
// already hold the user's headers or a hosted install's x-context-guru-token, so the line is merged in and
// only that line is taken out again.
package plugin

import (
	"path/filepath"
	"testing"
)

func headersOf(t *testing.T, path string) (string, bool) {
	t.Helper()
	env, _ := readJSON(t, path)["env"].(map[string]any)
	v, ok := env["ANTHROPIC_CUSTOM_HEADERS"].(string)
	return v, ok
}

func TestInstallAddsThePingHeader(t *testing.T) {
	state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
	path := filepath.Join(proj, "settings.json")
	if _, code := settingsIn(t, state, home, "add", "--file", path, "--url", ourURL); code != 0 {
		t.Fatalf("add exit %d", code)
	}
	if got, _ := headersOf(t, path); got != "x-context-guru-ping: 1" {
		t.Errorf("headers = %q", got)
	}
}

func TestPingHeaderIsAppendedWithANewlineAndTheUsersHeadersSurvive(t *testing.T) {
	state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
	path := filepath.Join(proj, "settings.json")
	writeJSON(t, path, map[string]any{"env": map[string]any{"ANTHROPIC_CUSTOM_HEADERS": "x-context-guru-token: cg_live_abc\nx-mine: 1"}})
	if _, code := settingsIn(t, state, home, "add", "--file", path, "--url", ourURL); code != 0 {
		t.Fatalf("add exit %d", code)
	}
	want := "x-context-guru-token: cg_live_abc\nx-mine: 1\nx-context-guru-ping: 1"
	if got, _ := headersOf(t, path); got != want {
		t.Errorf("headers = %q, want %q (newline-separated, nothing overwritten)", got, want)
	}
	// Re-running adds nothing twice.
	if _, code := settingsIn(t, state, home, "add", "--file", path, "--url", ourURL); code != 0 {
		t.Fatalf("second add exit %d", code)
	}
	if got, _ := headersOf(t, path); got != want {
		t.Errorf("after a re-run headers = %q, want %q", got, want)
	}
	// Uninstall takes only our line.
	if _, code := settingsIn(t, state, home, "remove", "--file", path); code != 0 {
		t.Fatalf("remove exit %d", code)
	}
	if got, _ := headersOf(t, path); got != "x-context-guru-token: cg_live_abc\nx-mine: 1" {
		t.Errorf("after uninstall headers = %q, want the user's lines only", got)
	}
}

func TestUninstallRemovesTheHeaderVariableWeCreated(t *testing.T) {
	state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
	path := filepath.Join(proj, "settings.json")
	writeJSON(t, path, map[string]any{"theme": "dark"})
	settingsIn(t, state, home, "add", "--file", path, "--url", ourURL)
	if _, code := settingsIn(t, state, home, "remove", "--file", path); code != 0 {
		t.Fatalf("remove exit %d", code)
	}
	if _, ok := headersOf(t, path); ok {
		t.Error("ANTHROPIC_CUSTOM_HEADERS was left behind with nothing of the user's in it")
	}
}

func TestAPingHeaderTheUserSetIsTheirsToKeep(t *testing.T) {
	state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
	path := filepath.Join(proj, "settings.json")
	writeJSON(t, path, map[string]any{"env": map[string]any{"ANTHROPIC_CUSTOM_HEADERS": "x-context-guru-ping: 0"}})
	settingsIn(t, state, home, "add", "--file", path, "--url", ourURL)
	if got, _ := headersOf(t, path); got != "x-context-guru-ping: 0" {
		t.Errorf("headers = %q: the user's own ping line was changed", got)
	}
	settingsIn(t, state, home, "remove", "--file", path)
	if got, _ := headersOf(t, path); got != "x-context-guru-ping: 0" {
		t.Errorf("after uninstall headers = %q: a line we did not add was removed", got)
	}
}

func TestPingHeaderCanBeLeftOut(t *testing.T) {
	state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
	path := filepath.Join(proj, "settings.json")
	settingsIn(t, state, home, "add", "--file", path, "--url", ourURL, "--no-ping-header")
	if _, ok := headersOf(t, path); ok {
		t.Error("--no-ping-header still wrote the header")
	}
}

// A project that installed before the header existed gets it on its next install run.
func TestARoutedInstallGetsThePingHeaderOnARerun(t *testing.T) {
	state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
	path := filepath.Join(proj, "settings.json")
	writeJSON(t, path, map[string]any{"env": map[string]any{"ANTHROPIC_BASE_URL": ourURL}})
	facts, code := settingsIn(t, state, home, "add", "--file", path, "--url", ourURL)
	if code != 0 || facts["result"] != "completed" {
		t.Fatalf("want result=completed, got %v exit %d", facts, code)
	}
	if got, _ := headersOf(t, path); got != "x-context-guru-ping: 1" {
		t.Errorf("headers = %q", got)
	}
}
