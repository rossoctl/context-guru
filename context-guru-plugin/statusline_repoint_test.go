// Tests for #551: after a plugin update the status line kept running the OLD version's script.
//
// install.sh writes `statusLine.command` (and the ownership record `installed_statusline`) with the
// absolute, versioned path of the plugin that ran the install. The old version stays in the plugin
// cache, so nothing failed and nothing moved the path. The SessionStart hook now re-points it.
//
// Every test builds a fake plugin cache (`plugins/cache/context-guru/context-guru/<version>/scripts`)
// from a copy of the real scripts, installs from one version, "updates" to another, and runs the
// NEW version's start-proxy.sh. Only temp HOME / CLAUDE_CONFIG_DIR / state directories are used.
package plugin

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repointEnv is one fixture: a home with the user settings file, a project, and a plugin cache.
type repointEnv struct {
	t     *testing.T
	home  string
	state string
	proj  string
	cache string
}

func newRepointEnv(t *testing.T) *repointEnv {
	t.Helper()
	root := t.TempDir()
	e := &repointEnv{t: t, home: filepath.Join(root, "home"), state: filepath.Join(root, "state"),
		proj: filepath.Join(root, "proj"), cache: filepath.Join(root, "home", ".claude", "plugins", "cache", "context-guru", "context-guru")}
	for _, d := range []string{filepath.Join(e.home, ".claude"), e.state, filepath.Join(e.proj, ".claude")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e *repointEnv) userFile() string { return filepath.Join(e.home, ".claude", "settings.json") }

// version copies the real scripts into the cache as `ver` and returns its scripts directory.
func (e *repointEnv) version(ver string) string {
	return e.versionOf("context-guru", "context-guru", ver)
}

// versionOf is version() for any marketplace and plugin name in the cache path.
func (e *repointEnv) versionOf(market, plugin, ver string) string {
	e.t.Helper()
	dir := filepath.Join(e.home, ".claude", "plugins", "cache", market, plugin, ver, "scripts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.t.Fatal(err)
	}
	entries, err := os.ReadDir(scriptsDir(e.t))
	if err != nil {
		e.t.Fatal(err)
	}
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(scriptsDir(e.t), ent.Name()))
		if err != nil {
			e.t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ent.Name()), b, 0o755); err != nil {
			e.t.Fatal(err)
		}
	}
	// Claude Code gives a cached plugin its own realpath; macOS temp dirs are symlinks.
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		e.t.Fatal(err)
	}
	return real
}

func (e *repointEnv) env() []string {
	return append(sandboxEnv(e.t), "HOME="+e.home, "CLAUDE_CONFIG_DIR="+filepath.Join(e.home, ".claude"),
		"CONTEXT_GURU_STATE="+e.state, "CLAUDE_PROJECT_DIR="+e.proj)
}

func (e *repointEnv) run(name string, args ...string) (string, int) {
	e.t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = e.proj
	cmd.Env = e.env()
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		e.t.Fatalf("%s: %v (%s)", name, err, out)
	}
	return string(out), code
}

// install writes the status line the way install.sh does: the running version's own script path.
func (e *repointEnv) install(scripts, file, flags string, userScope bool) {
	e.t.Helper()
	requireTool(e.t, "python3")
	args := []string{filepath.Join(scripts, "settings.py"), "add", "--file", file,
		"--statusline", `python3 "` + filepath.Join(scripts, "statusline.py") + `"` + flags, "--no-backup"}
	if userScope {
		args = append(args, "--user-scope")
	}
	if out, code := e.run("python3", args...); code != 0 {
		e.t.Fatalf("install failed: exit %d: %s", code, out)
	}
}

// sessionStart runs the given version's SessionStart hook. The project is not routed, so the hook
// has nothing else to do and exits at the routing gate.
func (e *repointEnv) sessionStart(scripts string) string {
	e.t.Helper()
	requireTool(e.t, "bash")
	out, code := e.run("bash", filepath.Join(scripts, "start-proxy.sh"))
	if code != 0 {
		e.t.Fatalf("start-proxy.sh must exit 0, got %d: %s", code, out)
	}
	return out
}

func statusLineOf(t *testing.T, file string) (command string, refresh any, recorded string) {
	t.Helper()
	data := readJSON(t, file)
	sl, _ := data["statusLine"].(map[string]any)
	meta, _ := data["$context-guru"].(map[string]any)
	command, _ = sl["command"].(string)
	recorded, _ = meta["installed_statusline"].(string)
	return command, sl["refreshInterval"], recorded
}

func quoted(scripts string) string {
	return `python3 "` + filepath.Join(scripts, "statusline.py") + `"`
}

func TestUpdateRepointsTheStatusLine(t *testing.T) {
	e := newRepointEnv(t)
	a, b := e.version("0.5.0"), e.version("0.5.1")
	e.install(a, e.userFile(), "", true)
	if cmd, _, _ := statusLineOf(t, e.userFile()); cmd != quoted(a) {
		t.Fatalf("setup: install should name version A, got %q", cmd)
	}
	out := e.sessionStart(b)
	cmd, _, rec := statusLineOf(t, e.userFile())
	if cmd != quoted(b) || rec != quoted(b) {
		t.Fatalf("both values must name version B\ncommand:  %s\nrecorded: %s\nwant:     %s", cmd, rec, quoted(b))
	}
	if n := strings.Count(strings.TrimSpace(out), "\n") + 1; strings.TrimSpace(out) == "" || n > 1 {
		t.Errorf("want exactly one note, got %d line(s): %q", n, out)
	}
	// Once current, a second session start says nothing and writes nothing.
	if out := e.sessionStart(b); strings.TrimSpace(out) != "" {
		t.Errorf("an already current status line must be silent, got %q", out)
	}
}

func TestRepointKeepsFlagsRefreshIntervalAndFileMode(t *testing.T) {
	e := newRepointEnv(t)
	a, b := e.version("0.5.0"), e.version("0.5.1")
	e.install(a, e.userFile(), " --layout=wide --flag", true)
	data := readJSON(t, e.userFile())
	data["statusLine"].(map[string]any)["refreshInterval"] = float64(7)
	data["theme"] = "dark"
	writeJSON(t, e.userFile(), data)
	if err := os.Chmod(e.userFile(), 0o600); err != nil {
		t.Fatal(err)
	}
	e.sessionStart(b)
	cmd, refresh, rec := statusLineOf(t, e.userFile())
	want := quoted(b) + " --layout=wide --flag"
	if cmd != want || rec != want {
		t.Errorf("flags must survive\ngot  %s\nwant %s\nrec  %s", cmd, want, rec)
	}
	if refresh != float64(7) {
		t.Errorf("refreshInterval must survive, got %v", refresh)
	}
	if readJSON(t, e.userFile())["theme"] != "dark" {
		t.Error("unrelated keys must survive")
	}
	if st, _ := os.Stat(e.userFile()); st.Mode().Perm() != 0o600 {
		t.Errorf("file mode must be kept, got %v", st.Mode().Perm())
	}
}

func TestRepointFixesEveryScopeWeOwn(t *testing.T) {
	e := newRepointEnv(t)
	a, b := e.version("0.5.0"), e.version("0.5.1")
	files := []string{e.userFile(), filepath.Join(e.proj, ".claude", "settings.json"),
		filepath.Join(e.proj, ".claude", "settings.local.json")}
	for _, f := range files {
		e.install(a, f, "", f == e.userFile())
	}
	e.sessionStart(b)
	for _, f := range files {
		if cmd, _, rec := statusLineOf(t, f); cmd != quoted(b) || rec != quoted(b) {
			t.Errorf("%s not re-pointed: %q / %q", f, cmd, rec)
		}
	}
}

func TestRepointLeavesAlone(t *testing.T) {
	cases := []struct {
		name  string
		setup func(e *repointEnv, a, b string) // leaves the user file in the state under test
	}{
		{"a status line the user wrote", func(e *repointEnv, a, b string) {
			writeJSON(t, e.userFile(), map[string]any{"statusLine": map[string]any{"type": "command",
				"command": quoted(a)}})
		}},
		{"a status line the user changed after the install", func(e *repointEnv, a, b string) {
			e.install(a, e.userFile(), "", true)
			data := readJSON(t, e.userFile())
			data["statusLine"].(map[string]any)["command"] = quoted(a) + " --mine"
			writeJSON(t, e.userFile(), data)
		}},
		{"a status line that is ours and already current", func(e *repointEnv, a, b string) {
			e.install(b, e.userFile(), "", true)
		}},
		{"a status line that names a newer version than the one running", func(e *repointEnv, a, b string) {
			e.install(e.version("0.6.0"), e.userFile(), "", true)
		}},
		{"a settings file with no status line", func(e *repointEnv, a, b string) {
			writeJSON(t, e.userFile(), map[string]any{"theme": "dark"})
		}},
		{"a settings file that is not valid JSON", func(e *repointEnv, a, b string) {
			if err := os.WriteFile(e.userFile(), []byte(`{"statusLine": {`), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"a status line outside the plugin cache", func(e *repointEnv, a, b string) {
			e.install(a, e.userFile(), "", true)
			data := readJSON(t, e.userFile())
			cmd := `python3 "/opt/mine/scripts/statusline.py"`
			data["statusLine"].(map[string]any)["command"] = cmd
			data["$context-guru"].(map[string]any)["installed_statusline"] = cmd
			writeJSON(t, e.userFile(), data)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newRepointEnv(t)
			a, b := e.version("0.5.0"), e.version("0.5.1")
			c.setup(e, a, b)
			before, err := os.ReadFile(e.userFile())
			if err != nil {
				t.Fatal(err)
			}
			if out := e.sessionStart(b); strings.TrimSpace(out) != "" {
				t.Errorf("must print nothing, got %q", out)
			}
			after, _ := os.ReadFile(e.userFile())
			if string(before) != string(after) {
				t.Errorf("file changed\nbefore: %s\nafter:  %s", before, after)
			}
			// The direct entry point must also exit 0 without touching the file.
			if out, code := e.run("python3", filepath.Join(b, "settings.py"), "statusline-repoint"); code != 0 || strings.TrimSpace(out) != "" {
				t.Errorf("statusline-repoint: exit %d, output %q", code, out)
			}
			after, _ = os.ReadFile(e.userFile())
			if string(before) != string(after) {
				t.Errorf("file changed by statusline-repoint")
			}
		})
	}
}

func TestRepointDoesNotRunWhenTheFileIsMissing(t *testing.T) {
	e := newRepointEnv(t)
	b := e.version("0.5.1")
	if out, code := e.run("python3", filepath.Join(b, "settings.py"), "statusline-repoint"); code != 0 || strings.TrimSpace(out) != "" {
		t.Errorf("exit %d, output %q", code, out)
	}
	if _, err := os.Stat(e.userFile()); !os.IsNotExist(err) {
		t.Error("a missing settings file must not be created")
	}
}

// A string compare puts "0.10.0" before "0.9.0". The versions must be compared as numbers.
func TestRepointComparesVersionsAsNumbers(t *testing.T) {
	t.Run("0.9.0 installed, 0.10.0 running: re-point", func(t *testing.T) {
		e := newRepointEnv(t)
		a, b := e.version("0.9.0"), e.version("0.10.0")
		e.install(a, e.userFile(), "", true)
		e.sessionStart(b)
		if cmd, _, rec := statusLineOf(t, e.userFile()); cmd != quoted(b) || rec != quoted(b) {
			t.Errorf("want B in both, got %q / %q", cmd, rec)
		}
	})
	t.Run("0.10.0 installed, 0.9.0 running: leave", func(t *testing.T) {
		e := newRepointEnv(t)
		a, b := e.version("0.10.0"), e.version("0.9.0")
		e.install(a, e.userFile(), "", true)
		before, _ := os.ReadFile(e.userFile())
		e.sessionStart(b)
		after, _ := os.ReadFile(e.userFile())
		if string(before) != string(after) {
			t.Errorf("a newer version must stay\nbefore: %s\nafter:  %s", before, after)
		}
	})
}

// Another plugin's (or another marketplace's) cached statusline.py is not ours to move, even when
// the ownership record names it and its version is older.
func TestRepointLeavesAnotherPluginsScriptAlone(t *testing.T) {
	for _, c := range []struct{ name, market, plugin string }{
		{"another plugin name", "context-guru", "other-plugin"},
		{"another marketplace", "other-market", "context-guru"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newRepointEnv(t)
			a, b := e.versionOf(c.market, c.plugin, "0.5.0"), e.version("0.5.1")
			e.install(a, e.userFile(), "", true)
			before, _ := os.ReadFile(e.userFile())
			e.sessionStart(b)
			if out, code := e.run("python3", filepath.Join(b, "settings.py"), "statusline-repoint"); code != 0 || strings.TrimSpace(out) != "" {
				t.Errorf("exit %d, output %q", code, out)
			}
			after, _ := os.ReadFile(e.userFile())
			if string(before) != string(after) {
				t.Errorf("file changed\nbefore: %s\nafter:  %s", before, after)
			}
		})
	}
}
