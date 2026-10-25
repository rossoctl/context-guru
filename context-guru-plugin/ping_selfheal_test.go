// The Ping header is written only by the install. An install made before the header existed never has
// it, and a restart cannot add it (#685). SessionStart adds it to the settings file that holds OUR
// routing, once, and leaves opt-outs, other people's files and the user's own headers alone.
package plugin

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type healEnv struct {
	t     *testing.T
	home  string
	proj  string
	state string
	port  string
	url   string
}

func newHealEnv(t *testing.T) *healEnv {
	e := &healEnv{t: t, home: t.TempDir(), proj: t.TempDir(), state: t.TempDir(), port: freePort(t)}
	if err := os.MkdirAll(filepath.Join(e.proj, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.url = "http://127.0.0.1:" + e.port + "/anthropic"
	return e
}

func (e *healEnv) file() string { return filepath.Join(e.proj, ".claude", "settings.local.json") }

// write puts an old-shaped install in place: our base URL, our record, and whatever else the case needs.
func (e *healEnv) write(env map[string]any, meta map[string]any) {
	e.t.Helper()
	env["ANTHROPIC_BASE_URL"] = e.url
	if meta == nil {
		meta = map[string]any{}
	}
	meta["installed_base_url"] = e.url
	writeJSON(e.t, e.file(), map[string]any{"env": env, "$context-guru": meta})
}

// sessionStart runs the real start-proxy.sh. The proxy binary is a stand-in that exits at once.
func (e *healEnv) sessionStart() string {
	e.t.Helper()
	requireTool(e.t, "bash")
	fake := filepath.Join(e.t.TempDir(), "fake-proxy")
	if err := os.WriteFile(fake, []byte("#!/usr/bin/env bash\nexit 0\n"), 0o755); err != nil {
		e.t.Fatal(err)
	}
	cmd := exec.Command("bash", filepath.Join(scriptsDir(e.t), "start-proxy.sh"))
	cmd.Dir = e.proj
	cmd.Env = append(sandboxEnv(e.t), "HOME="+e.home, "CLAUDE_CONFIG_DIR="+filepath.Join(e.home, ".claude"),
		"CLAUDE_PROJECT_DIR="+e.proj, "CONTEXT_GURU_STATE="+e.state, "CONTEXT_GURU_BIN="+fake,
		"CONTEXT_GURU_HEALTH_BUDGET=1", "CLAUDE_PLUGIN_OPTION_PORT="+e.port, "ANTHROPIC_BASE_URL="+e.url,
		"TMPDIR="+e.t.TempDir())
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() != 0 {
		e.t.Fatalf("start-proxy.sh must exit 0, got %d: %s", ee.ExitCode(), out)
	}
	return string(out)
}

func (e *healEnv) headers() (string, bool) { return headersOf(e.t, e.file()) }

const healNote = "Ping enabled for this install; it works from your next session"

func TestSessionStartHealsAnInstallFromBeforeThePingHeader(t *testing.T) {
	e := newHealEnv(t)
	e.write(map[string]any{}, nil) // no installed_ping_header, no opt-out: predates both
	out := e.sessionStart()
	if got, _ := e.headers(); got != "x-context-guru-ping: 1" {
		t.Errorf("headers = %q", got)
	}
	if !strings.Contains(out, healNote) {
		t.Errorf("the hook did not say it enabled Ping: %q", out)
	}
	meta, _ := readJSON(t, e.file())["$context-guru"].(map[string]any)
	if meta["installed_ping_header"] != "x-context-guru-ping: 1" {
		t.Errorf("the line is not recorded, so uninstall could not remove it: %v", meta)
	}
}

func TestSessionStartHealIsIdempotent(t *testing.T) {
	e := newHealEnv(t)
	e.write(map[string]any{}, nil)
	e.sessionStart()
	before, _ := os.ReadFile(e.file())
	out := e.sessionStart()
	after, _ := os.ReadFile(e.file())
	if string(before) != string(after) {
		t.Errorf("a second run changed the file:\n%s\n---\n%s", before, after)
	}
	if strings.Contains(out, healNote) {
		t.Errorf("a second run said it enabled Ping again: %q", out)
	}
}

func TestSessionStartDoesNotHealAnInstallMadeWithNoPingHeader(t *testing.T) {
	e := newHealEnv(t)
	home, state := t.TempDir(), t.TempDir()
	// A real opted-out install, not a hand-made file: the choice has to be recorded by `add`.
	if _, code := settingsInDir(t, state, home, e.proj, "add", "--file", e.file(), "--url", e.url, "--no-ping-header"); code != 0 {
		t.Fatalf("add exit %d", code)
	}
	if meta, _ := readJSON(t, e.file())["$context-guru"].(map[string]any); meta["ping_header_opt_out"] != true {
		t.Fatalf("the opt-out was not recorded: %v", meta)
	}
	before, _ := os.ReadFile(e.file())
	out := e.sessionStart()
	after, _ := os.ReadFile(e.file())
	if string(before) != string(after) {
		t.Errorf("an opted-out install was changed:\n%s\n---\n%s", before, after)
	}
	if strings.Contains(out, healNote) {
		t.Errorf("hook enabled Ping on an opted-out install: %q", out)
	}
}

func TestSessionStartHealKeepsTheUsersOtherHeaders(t *testing.T) {
	e := newHealEnv(t)
	e.write(map[string]any{"ANTHROPIC_CUSTOM_HEADERS": "x-context-guru-token: cg_live_abc\nx-mine: 1"}, nil)
	e.sessionStart()
	if got, _ := e.headers(); got != "x-context-guru-token: cg_live_abc\nx-mine: 1\nx-context-guru-ping: 1" {
		t.Errorf("headers = %q", got)
	}
}

func TestSessionStartHealLeavesAUserPingLineAlone(t *testing.T) {
	e := newHealEnv(t)
	e.write(map[string]any{"ANTHROPIC_CUSTOM_HEADERS": "x-context-guru-ping: 0"}, nil)
	e.sessionStart()
	if got, _ := e.headers(); got != "x-context-guru-ping: 0" {
		t.Errorf("headers = %q: the user's own ping line was changed", got)
	}
}

func TestSessionStartDoesNotTouchSettingsThatAreNotOurs(t *testing.T) {
	e := newHealEnv(t)
	// Routed to our port, but the record says somebody else wrote the URL; and a file with no record.
	writeJSON(t, e.file(), map[string]any{"env": map[string]any{"ANTHROPIC_BASE_URL": e.url},
		"$context-guru": map[string]any{"installed_base_url": "http://other.example/anthropic"}})
	other := filepath.Join(e.proj, ".claude", "settings.json")
	writeJSON(t, other, map[string]any{"env": map[string]any{"ANTHROPIC_BASE_URL": e.url, "ANTHROPIC_CUSTOM_HEADERS": "x-mine: 1"}})
	b1, _ := os.ReadFile(e.file())
	b2, _ := os.ReadFile(other)
	e.sessionStart()
	a1, _ := os.ReadFile(e.file())
	a2, _ := os.ReadFile(other)
	if string(b1) != string(a1) || string(b2) != string(a2) {
		t.Errorf("a settings file that is not ours was changed:\n%s\n%s", a1, a2)
	}
}

func TestUninstallAfterAHealRemovesExactlyOurLine(t *testing.T) {
	e := newHealEnv(t)
	e.write(map[string]any{"ANTHROPIC_CUSTOM_HEADERS": "x-mine: 1"}, nil)
	e.sessionStart()
	if got, _ := e.headers(); got != "x-mine: 1\nx-context-guru-ping: 1" {
		t.Fatalf("headers = %q", got)
	}
	if _, code := settingsInDir(t, e.state, e.home, e.proj, "remove", "--file", e.file()); code != 0 {
		t.Fatalf("remove exit %d", code)
	}
	if got, _ := e.headers(); got != "x-mine: 1" {
		t.Errorf("after uninstall headers = %q, want the user's line only", got)
	}
}

func TestEnsurePingHeaderCheckWritesNothing(t *testing.T) {
	e := newHealEnv(t)
	e.write(map[string]any{}, nil)
	before, _ := os.ReadFile(e.file())
	facts, code := settingsInDir(t, e.state, e.home, e.proj, "ensure-ping-header", "--check", "--file", e.file())
	after, _ := os.ReadFile(e.file())
	if code != 0 || facts["result"] != "missing" || string(before) != string(after) {
		t.Errorf("exit %d facts %v; file changed: %v", code, facts, string(before) != string(after))
	}
}
