// The Ping header is written only by the install. An install made before the header existed never has
// it, and a restart cannot add it (#685). SessionStart adds it to the settings file that holds OUR
// routing, once, and leaves opt-outs, other people's files and the user's own headers alone.
package plugin

import (
	"net/http"
	"net/http/httptest"
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

// recordInstall writes the install-scope row `add` leaves for this file, stamped at `when`. A row from before
// --no-ping-header shipped (2026-10-22 13:58:53 UTC) is the proof that no opt-out can exist.
func (e *healEnv) recordInstall(when string) {
	e.t.Helper()
	real, err := filepath.EvalSymlinks(e.file())
	if err != nil {
		e.t.Fatal(err)
	}
	writeJSON(e.t, filepath.Join(e.state, "install-scope.json"), map[string]any{"version": 1, "projects": map[string]any{
		e.proj: map[string]any{"scope": "project-local", "file": real, "port": 1, "recorded_at": when}}})
}

const (
	beforeFlag = "2026-10-20T08:00:00+00:00"
	afterFlag  = "2026-10-23T08:00:00+00:00"
)

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
	e.recordInstall(beforeFlag)
	out := e.sessionStart()
	if got, _ := e.headers(); got != "x-context-guru-ping: 1" {
		t.Errorf("headers = %q", got)
	}
	for _, want := range []string{healNote, "5 minutes after your last request on a 5-minute cache", "up to 59 minutes on a 1-hour cache", "--no-ping-header"} {
		if !strings.Contains(out, want) {
			t.Errorf("the consent note is missing %q: %q", want, out)
		}
	}
	meta, _ := readJSON(t, e.file())["$context-guru"].(map[string]any)
	if meta["installed_ping_header"] != "x-context-guru-ping: 1" {
		t.Errorf("the line is not recorded, so uninstall could not remove it: %v", meta)
	}
}

func TestSessionStartHealIsIdempotent(t *testing.T) {
	e := newHealEnv(t)
	e.write(map[string]any{}, nil)
	e.recordInstall(beforeFlag)
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
	e.recordInstall(beforeFlag)
	e.sessionStart()
	if got, _ := e.headers(); got != "x-context-guru-token: cg_live_abc\nx-mine: 1\nx-context-guru-ping: 1" {
		t.Errorf("headers = %q", got)
	}
}

func TestSessionStartHealLeavesAUserPingLineAlone(t *testing.T) {
	e := newHealEnv(t)
	e.write(map[string]any{"ANTHROPIC_CUSTOM_HEADERS": "x-context-guru-ping: 0"}, nil)
	e.recordInstall(beforeFlag)
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
	e.recordInstall(beforeFlag)
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
	e.recordInstall(beforeFlag)
	before, _ := os.ReadFile(e.file())
	facts, code := settingsInDir(t, e.state, e.home, e.proj, "ensure-ping-header", "--check", "--file", e.file())
	after, _ := os.ReadFile(e.file())
	if code != 0 || facts["result"] != "missing" || string(before) != string(after) {
		t.Errorf("exit %d facts %v; file changed: %v", code, facts, string(before) != string(after))
	}
}

// An install with no record either way and no proof it is older than --no-ping-header may be an opt-out made
// by the released build (v0.5.4 wrote no record). Nothing is written; one note is shown, once.
func TestSessionStartDoesNotWriteWhenTheInstallCannotBeProvenOlder(t *testing.T) {
	for name, stamp := range map[string]string{"newer row": afterFlag, "no row": ""} {
		t.Run(name, func(t *testing.T) {
			e := newHealEnv(t)
			e.write(map[string]any{"ANTHROPIC_CUSTOM_HEADERS": "x-mine: 1"}, nil)
			if stamp != "" {
				e.recordInstall(stamp)
			}
			before, _ := os.ReadFile(e.file())
			out := e.sessionStart()
			after, _ := os.ReadFile(e.file())
			if string(before) != string(after) {
				t.Errorf("the file was written:\n%s\n---\n%s", before, after)
			}
			const off = "Ping is off for this install. Run /context-guru:install to turn it on."
			if !strings.Contains(out, off) {
				t.Errorf("no note: %q", out)
			}
			if again := e.sessionStart(); strings.Contains(again, off) {
				t.Errorf("the note was shown twice: %q", again)
			}
		})
	}
}

// What the v0.5.4 `add --no-ping-header` wrote: our URL and record, no ping keys. A row stamped after the
// flag shipped proves it is not a pre-flag install.
func TestSessionStartKeepsAV054OptOutOff(t *testing.T) {
	e := newHealEnv(t)
	e.write(map[string]any{}, nil)
	e.recordInstall(afterFlag)
	e.sessionStart()
	if got, has := e.headers(); has {
		t.Errorf("headers = %q: a possible opt-out was turned on", got)
	}
}

// The user took our line out by hand: that is a removal, and it stays removed.
func TestSessionStartDoesNotReAddALineTheUserRemoved(t *testing.T) {
	e := newHealEnv(t)
	e.write(map[string]any{}, nil)
	e.recordInstall(beforeFlag)
	e.sessionStart()
	f := readJSON(t, e.file())
	delete(f["env"].(map[string]any), "ANTHROPIC_CUSTOM_HEADERS")
	writeJSON(t, e.file(), f)
	before, _ := os.ReadFile(e.file())
	out := e.sessionStart()
	after, _ := os.ReadFile(e.file())
	if string(before) != string(after) || strings.Contains(out, healNote) {
		t.Errorf("the removed line came back:\n%s\nout: %q", after, out)
	}
}

// The consent text promises that a re-run with --no-ping-header stops the header. Heal, then opt out.
func TestOptingOutAfterAHealRemovesOurLineOnly(t *testing.T) {
	e := newHealEnv(t)
	e.write(map[string]any{"ANTHROPIC_CUSTOM_HEADERS": "x-mine: 1"}, nil)
	e.recordInstall(beforeFlag)
	e.sessionStart()
	if _, code := settingsInDir(t, e.state, e.home, e.proj, "add", "--file", e.file(), "--url", e.url, "--no-ping-header"); code != 0 {
		t.Fatalf("add exit %d", code)
	}
	if got, _ := e.headers(); got != "x-mine: 1" {
		t.Errorf("headers = %q, want the user's line only", got)
	}
	meta, _ := readJSON(t, e.file())["$context-guru"].(map[string]any)
	if meta["ping_header_opt_out"] != true || meta["installed_ping_header"] != nil {
		t.Errorf("meta = %v", meta)
	}
	// A second opt-out run is a no-op, and the next session does not heal it.
	before, _ := os.ReadFile(e.file())
	e.sessionStart()
	after, _ := os.ReadFile(e.file())
	if string(before) != string(after) {
		t.Errorf("SessionStart changed an opted-out file:\n%s", after)
	}
}

// The opt-out also works from the other write branches of `add` (a repoint to a new port).
func TestOptingOutRemovesOurLineWhenTheInstallMoves(t *testing.T) {
	e := newHealEnv(t)
	if _, code := settingsInDir(t, e.state, e.home, e.proj, "add", "--file", e.file(), "--url", e.url); code != 0 {
		t.Fatal("add failed")
	}
	if got, _ := e.headers(); got != "x-context-guru-ping: 1" {
		t.Fatalf("headers = %q", got)
	}
	moved := "http://127.0.0.1:" + freePort(t) + "/anthropic"
	if _, code := settingsInDir(t, e.state, e.home, e.proj, "add", "--file", e.file(), "--url", moved, "--no-ping-header"); code != 0 {
		t.Fatal("repoint failed")
	}
	if got, has := e.headers(); has {
		t.Errorf("headers = %q: the opt-out left our line", got)
	}
}

// The state names the doctor line and the heal read: a ping line that is not 1 is "user-off", not "present".
func TestDoctorPingCheck(t *testing.T) {
	for state, c := range map[string]struct {
		env  map[string]any
		meta map[string]any
		want string
	}{
		"present":   {map[string]any{"ANTHROPIC_CUSTOM_HEADERS": "x-context-guru-ping: 1"}, nil, "present"},
		"user-off":  {map[string]any{"ANTHROPIC_CUSTOM_HEADERS": "x-context-guru-ping: 0"}, nil, "user-off"},
		"missing":   {map[string]any{}, nil, "missing"},
		"removed":   {map[string]any{}, map[string]any{"installed_ping_header": "x-context-guru-ping: 1"}, "removed"},
		"opted out": {map[string]any{}, map[string]any{"ping_header_opt_out": true}, "opted-out"},
	} {
		t.Run(state, func(t *testing.T) {
			e := newHealEnv(t)
			e.write(c.env, c.meta)
			facts, code := settingsInDir(t, e.state, e.home, e.proj, "ensure-ping-header", "--check", "--file", e.file())
			if code != 0 || facts["result"] != c.want {
				t.Errorf("exit %d facts %v, want %s", code, facts, c.want)
			}
		})
	}
}

// /context-guru:doctor shows the Ping line for a routed install and says how to fix it; elsewhere it is silent.
func TestDoctorShowsThePingHeaderLine(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	t.Cleanup(srv.Close)
	for name, c := range map[string]struct {
		env  map[string]any
		meta map[string]any
		want string
	}{
		"present":   {map[string]any{"ANTHROPIC_CUSTOM_HEADERS": "x-context-guru-ping: 1"}, nil, "✓ Ping header is set"},
		"missing":   {map[string]any{}, nil, "! Ping header is set"},
		"opted out": {map[string]any{}, map[string]any{"ping_header_opt_out": true}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			e := newHealEnv(t)
			e.write(c.env, c.meta)
			out, _ := runCGCode(t, srv.URL, []string{"CLAUDE_PROJECT_DIR=" + e.proj}, "doctor")
			has := strings.Contains(out, "Ping header is set")
			if c.want == "" && has {
				t.Errorf("an opted-out install got a Ping line:\n%s", out)
			}
			if c.want != "" && !strings.Contains(out, c.want) {
				t.Errorf("missing %q:\n%s", c.want, out)
			}
			if name == "missing" && !strings.Contains(out, "Run /context-guru:install to turn Ping on") {
				t.Errorf("no fix text:\n%s", out)
			}
		})
	}
}
