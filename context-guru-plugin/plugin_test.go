// Package plugin holds tests for the Claude Code plugin's shell/Python helpers.
//
// The scripts are not Go, but their failure modes are the most expensive in this repo: they
// edit the user's real settings.json, and the SessionStart hook runs in EVERY project the user
// has. A regression here does not degrade compaction — it breaks Claude Code on a stranger's
// machine, in projects that have nothing to do with context-guru. So they are tested from Go,
// where `go test ./...` and CI already look.
package plugin

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func scriptsDir(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("scripts")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("plugin scripts missing: %v", err)
	}
	return abs
}

// requireTool fails rather than skips for the two interpreters these tests are built on.
//
// A skip and a pass are indistinguishable in CI output, and this whole package exists because
// these scripts warrant coverage their blast radius demands — so on a runner without python3 or
// bash, EVERY test in this file used to skip and `go test` was green. ubuntu-latest has both, so
// an absence means the image changed, and that is something to hear about rather than sail past.
// Genuinely optional tools keep using t.Skipf via requireOptionalTool.
func requireTool(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		switch name {
		case "python3", "bash":
			t.Fatalf("%s is required to test the plugin scripts and is not on PATH: %v", name, err)
		default:
			t.Skipf("%s not available: %v", name, err)
		}
	}
	return p
}

// settings runs settings.py and returns its key=value output as a map, plus the exit code.
func settings(t *testing.T, args ...string) (map[string]string, int) {
	t.Helper()
	return settingsIn(t, t.TempDir(), t.TempDir(), args...)
}

// settingsIn is settings() with the two directories settings.py now writes OUTSIDE the target file
// made explicit: the state directory holding the escape hatch and its record, and HOME (the hatch
// is also copied to ~/.local/bin when that exists).
//
// Every test goes through here, including the ones that predate the hatch, because otherwise
// `go test` writes into the DEVELOPER'S real ~/.local/state/context-guru — recording temp paths in
// the manifest their own hatch would later read, and rewriting the copy on their PATH. A test suite
// for a recovery tool must not be able to disturb the developer's own recovery tool.
func settingsIn(t *testing.T, state, home string, args ...string) (map[string]string, int) {
	t.Helper()
	return settingsInDir(t, state, home, "", args...)
}

// settingsInDir is settingsIn with the working directory also made explicit — needed by anything
// that depends on `os.getcwd()` inside settings.py (record_install_scope/resolve_install_scope
// key their state by the caller's cwd, the same way install.sh's `route_scope_file()` builds
// `$PWD/.claude/settings.local.json`). `dir` empty keeps the test binary's own cwd, matching
// settingsIn's prior behavior for every test that does not care.
func settingsInDir(t *testing.T, state, home, dir string, args ...string) (map[string]string, int) {
	t.Helper()
	py := requireTool(t, "python3")
	cmd := exec.Command(py, append([]string{filepath.Join(scriptsDir(t), "settings.py")}, args...)...)
	cmd.Env = append(sandboxEnv(t), "CONTEXT_GURU_STATE="+state, "HOME="+home)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running settings.py: %v (%s)", err, out)
	}
	facts := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			facts[k] = v
		}
	}
	t.Logf("settings.py %v (dir=%s) -> exit %d, %v", args, dir, code, facts)
	return facts, code
}

// sandboxEnv is the base environment for EVERY subprocess this suite starts.
//
// S5 in review, reproduced live on a reviewer's machine: `go test ./context-guru-plugin/...` spawned
// start-proxy.sh, which resolved the developer's REAL ~/.local/state/context-guru and left four
// pidfiles in it. The PR's notes claimed this was fixed; it was fixed for the settings() helper only,
// while ~28 other call sites still did `append(sandboxEnv(t), …)` and inherited the real HOME.
//
// A test suite for a recovery tool must not be able to disturb the developer's recovery tool, so the
// isolation belongs in one place that every site goes through rather than in a habit each new test has
// to remember. Everything that could redirect a write into real dotfiles — or make the suite's result
// depend on the machine it runs on — is dropped and re-pinned to per-test temp directories.
//
// The ANTHROPIC_* variables are dropped for the second reason: the reviewer's box has CLAUDE_CONFIG_DIR
// set and the suite FAILED there, and a developer machine running this plugin has ANTHROPIC_BASE_URL and
// ANTHROPIC_CUSTOM_HEADERS exported, which would leak into assertions about an unrouted project.
//
// `extra` is appended last, so a test that deliberately sets one of these still wins.
func sandboxEnv(t *testing.T, extra ...string) []string {
	t.Helper()
	sandboxed := map[string]bool{
		"HOME": true, "XDG_STATE_HOME": true, "CONTEXT_GURU_STATE": true, "CLAUDE_CONFIG_DIR": true,
		"CONTEXT_GURU_BIN": true, "ANTHROPIC_BASE_URL": true, "ANTHROPIC_UPSTREAM": true,
		"ANTHROPIC_API_KEY": true, "ANTHROPIC_AUTH_TOKEN": true, "ANTHROPIC_CUSTOM_HEADERS": true,
	}
	var env []string
	for _, kv := range os.Environ() {
		if k, _, ok := strings.Cut(kv, "="); !ok || !sandboxed[k] {
			env = append(env, kv)
		}
	}
	// HOME and XDG_STATE_HOME are PINNED; CONTEXT_GURU_STATE is only DROPPED, not set.
	//
	// Setting it broke four pre-existing tests, and the reason is worth keeping: since S10,
	// start-proxy.sh resolves CONTEXT_GURU_STATE ahead of XDG_STATE_HOME — so pinning it here outranked
	// the XDG_STATE_HOME those tests deliberately pass, and the pidfile and keep-alive config landed
	// somewhere they were not looking. Dropping it from the inherited environment is all the isolation
	// needs: every state path then resolves through XDG_STATE_HOME or HOME, both of which are pinned
	// below. A test that wants to pin CONTEXT_GURU_STATE itself passes it in `extra`, which wins.
	root := t.TempDir()
	env = append(env,
		"HOME="+root,
		"XDG_STATE_HOME="+filepath.Join(root, "xdg-state"),
	)
	return append(env, extra...)
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("settings file is not valid JSON after the edit: %v\n%s", err, b)
	}
	return m
}

const ourURL = "http://127.0.0.1:8787/anthropic"

// TestSettingsAddPreservesEverythingElse is the whole reason a script does this rather than a
// one-line `jq`: the target is a file the user depends on, holding their theme, model,
// permission rules and their own env vars. Exactly one key may appear, and nothing may be lost.
func TestSettingsAddPreservesEverythingElse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	writeJSON(t, path, map[string]any{
		"theme": "dark",
		"model": "opus",
		"env": map[string]any{
			"SOME_OTHER_VAR":       "keep me",
			"ANTHROPIC_SMALL_FAST": "also keep me",
		},
		"permissions": map[string]any{"allow": []string{"Bash(ls:*)"}},
	})

	facts, code := settings(t, "add", "--file", path, "--url", ourURL)
	if code != 0 || facts["result"] != "added" {
		t.Fatalf("add failed: exit %d, %v", code, facts)
	}
	if facts["backup"] == "" || facts["backup"] == "(new file)" {
		t.Errorf("no backup was taken of an existing settings file: %v", facts)
	} else if _, err := os.Stat(facts["backup"]); err != nil {
		t.Errorf("reported backup %q does not exist: %v", facts["backup"], err)
	}

	got := readJSON(t, path)
	if got["theme"] != "dark" || got["model"] != "opus" {
		t.Errorf("top-level settings were lost: %v", got)
	}
	if got["permissions"] == nil {
		t.Error("permissions block was lost")
	}
	env, _ := got["env"].(map[string]any)
	if env["ANTHROPIC_BASE_URL"] != ourURL {
		t.Errorf("env.ANTHROPIC_BASE_URL = %v, want %q", env["ANTHROPIC_BASE_URL"], ourURL)
	}
	if env["SOME_OTHER_VAR"] != "keep me" || env["ANTHROPIC_SMALL_FAST"] != "also keep me" {
		t.Errorf("the user's own env vars were lost: %v", env)
	}
	if len(env) != 3 {
		t.Errorf("env has %d keys, want the 2 originals plus ours: %v", len(env), env)
	}
}

// TestSettingsAddRefusesToStealAnExistingBaseURL covers the one conflict the install has to
// reason about. A base URL already in the file may be the user's company gateway or a benchmark
// endpoint; taking it over would break their setup while reporting success.
func TestSettingsAddRefusesToStealAnExistingBaseURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	theirs := "https://gateway.corp.example/anthropic"
	writeJSON(t, path, map[string]any{"env": map[string]any{"ANTHROPIC_BASE_URL": theirs}})

	facts, code := settings(t, "add", "--file", path, "--url", ourURL)
	if code != 2 || facts["result"] != "conflict" {
		t.Fatalf("expected a conflict (exit 2), got exit %d, %v", code, facts)
	}
	if facts["existing"] != theirs {
		t.Errorf("conflict did not report the existing value: %v", facts)
	}
	if env := readJSON(t, path)["env"].(map[string]any); env["ANTHROPIC_BASE_URL"] != theirs {
		t.Fatalf("the file was modified despite the conflict: %v", env)
	}

	// --force is the user's explicit decision, and it must report what it replaced so the old
	// value is recoverable from the transcript as well as the backup.
	facts, code = settings(t, "add", "--file", path, "--url", ourURL, "--force")
	if code != 0 || facts["result"] != "added" || facts["replaced"] != theirs {
		t.Fatalf("--force did not replace and report: exit %d, %v", code, facts)
	}

	// Re-adding the same URL is a no-op, so a re-run of the install skill is free.
	facts, code = settings(t, "add", "--file", path, "--url", ourURL)
	if code != 0 || facts["result"] != "unchanged" {
		t.Fatalf("re-adding the same URL should be unchanged: exit %d, %v", code, facts)
	}
}

// TestSettingsRemoveTakesOnlyOurKey: uninstall must be exact. It removes our base URL and
// nothing else, refuses to remove one that is not ours, and leaves no empty `env: {}` behind.
func TestSettingsRemoveTakesOnlyOurKey(t *testing.T) {
	dir := t.TempDir()

	// (a) our key alongside the user's own env vars.
	path := filepath.Join(dir, "a.json")
	writeJSON(t, path, map[string]any{"theme": "dark", "env": map[string]any{
		"ANTHROPIC_BASE_URL": ourURL, "KEEP": "yes"}})
	facts, code := settings(t, "remove", "--file", path, "--url", ourURL)
	if code != 0 || facts["result"] != "removed" {
		t.Fatalf("remove failed: exit %d, %v", code, facts)
	}
	got := readJSON(t, path)
	env, _ := got["env"].(map[string]any)
	if _, still := env["ANTHROPIC_BASE_URL"]; still {
		t.Error("the key survived removal")
	}
	if env["KEEP"] != "yes" || got["theme"] != "dark" {
		t.Errorf("removal took more than its own key: %v", got)
	}

	// (b) our key alone: the env block we created goes with it, leaving no litter.
	path = filepath.Join(dir, "b.json")
	writeJSON(t, path, map[string]any{"theme": "dark", "env": map[string]any{
		"ANTHROPIC_BASE_URL": ourURL}})
	if _, code := settings(t, "remove", "--file", path, "--url", ourURL); code != 0 {
		t.Fatalf("remove exit %d", code)
	}
	if got := readJSON(t, path); got["env"] != nil {
		t.Errorf("an empty env block was left behind: %v", got)
	}

	// (c) a base URL that is NOT ours must survive an uninstall untouched.
	path = filepath.Join(dir, "c.json")
	theirs := "http://127.0.0.1:4000/anthropic" // e.g. litellm
	writeJSON(t, path, map[string]any{"env": map[string]any{"ANTHROPIC_BASE_URL": theirs}})
	facts, code = settings(t, "remove", "--file", path, "--url", ourURL)
	if code != 2 || facts["result"] != "conflict" {
		t.Fatalf("uninstall must not remove a base URL it did not install: exit %d, %v", code, facts)
	}
	if env := readJSON(t, path)["env"].(map[string]any); env["ANTHROPIC_BASE_URL"] != theirs {
		t.Fatalf("someone else's base URL was removed: %v", env)
	}
}

// TestSettingsRefusesToRewriteABrokenFile: if the file will not parse, the only safe move is to
// stop. Treating it as empty and writing a fresh one would discard every setting in it.
func TestSettingsRefusesToRewriteABrokenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	broken := "{\n  \"theme\": \"dark\",,,\n}\n"
	if err := os.WriteFile(path, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	facts, code := settings(t, "add", "--file", path, "--url", ourURL)
	if code != 3 || facts["reason"] != "unparseable_json" {
		t.Fatalf("expected a refusal on unparseable JSON, got exit %d, %v", code, facts)
	}
	b, _ := os.ReadFile(path)
	if string(b) != broken {
		t.Fatalf("the broken file was modified:\n%s", b)
	}
}

// --- the SessionStart hook -----------------------------------------------------------------

// runStart runs start-proxy.sh with a controlled environment and returns its output.
//
// CONTEXT_GURU_BIN points at a sentinel script: if the hook decides to launch a proxy, the
// sentinel file appears. That is how "did it start something?" is asserted, rather than by
// looking for a process.
func runStart(t *testing.T, env map[string]string) (out string, code int, startedSentinel string) {
	t.Helper()
	requireTool(t, "bash")
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "started")
	fake := filepath.Join(dir, "fake-proxy")
	if err := os.WriteFile(fake, []byte("#!/usr/bin/env bash\ntouch \""+sentinel+"\"\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "start-proxy.sh"))
	cmd.Env = append(sandboxEnv(t), "CONTEXT_GURU_BIN="+fake, "TMPDIR="+dir)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	b, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running start-proxy.sh: %v (%s)", err, b)
	}
	t.Logf("start-proxy.sh env=%v -> exit %d, output:\n%s", env, code, b)
	return string(b), code, sentinel
}

// TestHookIsSilentAndInertWhereRoutingIsNotConfigured is the property that makes a user-scope
// plugin acceptable at all.
//
// The plugin installs globally, so this hook runs on EVERY session in EVERY project — including
// all the ones the user never routed. In those it must do nothing and say nothing: starting a
// proxy would be waste, and printing anything would put context-guru noise in sessions that have
// nothing to do with it. It also must not hijack a user who routes to a different local proxy on
// another port, which is why the gate matches the port and not merely "localhost".
func TestHookIsSilentAndInertWhereRoutingIsNotConfigured(t *testing.T) {
	// The last row is a POSITIVE CONTROL and it is not decoration.
	//
	// Every other assertion here is an absence, so without it this test cannot distinguish "the
	// gate declined" from "the script exited before reaching the gate" — gut start-proxy.sh to a
	// bare `exit 0` and every silent row still passes. The control did exist, in
	// TestHookStartsTheProxyAndWaitsForHealthz, but a control in a different test is one that a
	// later change can narrow or skip without anything here failing. Keep it local to the test
	// whose meaning depends on it.
	for _, c := range []struct {
		name, baseURL string
		routed        bool
	}{
		{name: "unset"},
		{name: "another local proxy on a different port (e.g. litellm)", baseURL: "http://localhost:4000/anthropic"},
		{name: "a remote gateway", baseURL: "https://gateway.corp.example/anthropic"},
		{name: "our port number appearing in a REMOTE host", baseURL: "https://8787.example.com/anthropic"},
		// The gate matched on the port as a PREFIX, so 8787 also matched 87871 — and this hook
		// would start our proxy on 8787 under a user routed to a different local proxy there.
		{name: "our port as a PREFIX of a longer port", baseURL: "http://127.0.0.1:87871/anthropic"},
		{name: "POSITIVE CONTROL: a routed project, where it must act", routed: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			port := "8787"
			baseURL := c.baseURL
			if c.routed {
				// A port of our own, so this never probes or starts anything on a developer's
				// real 8787, and a short budget because the stand-in never answers /healthz.
				port = freePort(t)
				baseURL = "http://127.0.0.1:" + port + "/anthropic"
			}
			env := map[string]string{
				"CLAUDE_PLUGIN_OPTION_PORT":  port,
				"ANTHROPIC_BASE_URL":         baseURL,
				"CONTEXT_GURU_HEALTH_BUDGET": "1",
				"XDG_STATE_HOME":             t.TempDir(),
			}
			out, code, sentinel := runStart(t, env)
			if code != 0 {
				t.Errorf("exit %d; the hook must never fail a session", code)
			}
			_, started := os.Stat(sentinel)
			if c.routed {
				if started != nil {
					t.Errorf("the hook did NOT start a proxy in a routed project — so every "+
						"silent row above proves nothing: %v\noutput:\n%s", started, out)
				}
				if strings.TrimSpace(out) == "" {
					t.Errorf("the hook said nothing in a routed project where the proxy never " +
						"came up; the failure report is the only diagnostic that path has")
				}
				return
			}
			if strings.TrimSpace(out) != "" {
				t.Errorf("the hook printed output in an unrouted project: %q", out)
			}
			if started == nil {
				t.Error("the hook started a proxy in a project that is not routed to it")
			}
		})
	}
}

// TestHookIsIdempotentWhenTheProxyIsAlreadyUp: SessionStart also fires on clear, compact,
// resume and fork, so a long session re-runs this repeatedly. A second proxy must never be
// launched — it would fail to bind, or worse, bind a different port and split the state.
func TestHookIsIdempotentWhenTheProxyIsAlreadyUp(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln) //nolint:errcheck // returns ErrServerClosed on Close
	defer srv.Close()
	port := fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)

	out, code, sentinel := runStart(t, map[string]string{
		"CLAUDE_PLUGIN_OPTION_PORT": port,
		"ANTHROPIC_BASE_URL":        "http://127.0.0.1:" + port + "/anthropic",
	})
	if code != 0 {
		t.Errorf("exit %d, output %q", code, out)
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Error("a second proxy was started even though /healthz already answered")
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("nothing to do, but the hook printed %q", out)
	}
}

// TestHookNeverFailsTheSessionWhenTheBinaryIsMissing: routed, but the binary is gone (the user
// deleted it, or PATH differs under the hook). The session must still start, with an
// explanation — a hook that exits non-zero here is a plugin that can brick every session on the
// machine, which is the biggest risk in this whole feature.
func TestHookNeverFailsTheSessionWhenTheBinaryIsMissing(t *testing.T) {
	requireTool(t, "bash")
	dir := t.TempDir()
	cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "start-proxy.sh"))
	// An unused high port: nothing answers /healthz, and the binary does not exist.
	cmd.Env = append(sandboxEnv(t),
		"CLAUDE_PLUGIN_OPTION_PORT=8799",
		"ANTHROPIC_BASE_URL=http://127.0.0.1:8799/anthropic",
		"CONTEXT_GURU_BIN="+filepath.Join(dir, "does-not-exist"),
		"TMPDIR="+dir)
	b, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit %d — the hook must never fail a session: %s", code, b)
	}
	out := string(b)
	for _, want := range []string{"not on PATH", "/context-guru:install"} {
		if !strings.Contains(out, want) {
			t.Errorf("the explanation omits %q:\n%s", want, out)
		}
	}
}

// TestHookStartsTheProxyAndWaitsForHealthz is the positive path, and specifically the WAIT: the
// hook is synchronous on purpose, so the session's first API request cannot beat the proxy up.
// A hook that returned before /healthz answered would leave that race in place.
func TestHookStartsTheProxyAndWaitsForHealthz(t *testing.T) {
	requireTool(t, "bash")
	py := requireTool(t, "python3")
	if runtime.GOOS == "windows" {
		t.Skip("shell hook is POSIX-only")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
	ln.Close() // free it for the fake proxy to bind

	dir := t.TempDir()
	// A stand-in proxy: takes ~1s to come up, then answers /healthz. The delay is the point —
	// it is what a hook that does not wait would skip past.
	fake := filepath.Join(dir, "fake-proxy")
	script := "#!/usr/bin/env bash\nsleep 1\nexec " + py + " -c '\n" +
		"import http.server\n" +
		"class H(http.server.BaseHTTPRequestHandler):\n" +
		"    def do_GET(self):\n" +
		"        self.send_response(200); self.end_headers(); self.wfile.write(b\"ok\")\n" +
		"    def log_message(self, *a): pass\n" +
		"http.server.HTTPServer((\"127.0.0.1\", " + port + "), H).serve_forever()\n'\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "start-proxy.sh"))
	cmd.Env = append(sandboxEnv(t),
		"CLAUDE_PLUGIN_OPTION_PORT="+port,
		"ANTHROPIC_BASE_URL=http://127.0.0.1:"+port+"/anthropic",
		"CONTEXT_GURU_BIN="+fake,
		"TMPDIR="+dir)
	b, err := cmd.CombinedOutput()
	t.Cleanup(func() { exec.Command("pkill", "-f", "127.0.0.1\", "+port).Run() }) //nolint:errcheck
	if err != nil {
		t.Fatalf("start-proxy.sh failed: %v\n%s", err, b)
	}
	if !strings.Contains(string(b), "proxy up on 127.0.0.1:"+port) {
		t.Fatalf("the hook returned without reporting a healthy proxy:\n%s", b)
	}
	// The claim is that it returned only AFTER /healthz answered, so it must answer now.
	resp, err := http.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		t.Fatalf("the hook reported the proxy up, but /healthz does not answer: %v", err)
	}
	resp.Body.Close()
}

// --- fixes from the review of #141 ---------------------------------------------------------

// TestBackupsDoNotClobberEachOther is the defect that destroyed the user's undo.
//
// The stamp was second-granularity with a plain copy2, so an install-then-uninstall round trip —
// well inside one second — wrote both backups to the SAME filename. The survivor held the
// POST-install state, and the install skill tells the user to keep that path as their undo. The
// value it was supposed to protect was gone from both the file and the backup.
func TestBackupsDoNotClobberEachOther(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	theirs := "https://gateway.corp.example/anthropic"
	writeJSON(t, path, map[string]any{"env": map[string]any{"ANTHROPIC_BASE_URL": theirs}})

	// Back to back, deliberately: the bug needed only that both land in the same second. Two
	// `add` calls rather than an add/remove pair — a clean `remove` now deletes every backup for
	// the file outright (see forget_backups()), which would hide the exact clobbering this test
	// exists to catch rather than exercise it.
	first, code := settings(t, "add", "--file", path, "--url", ourURL, "--force")
	if code != 0 {
		t.Fatalf("first add: exit %d, %v", code, first)
	}
	// The first backup must still hold what was there BEFORE we touched it — checked now, before
	// the second call below has any chance to matter.
	b, err := os.ReadFile(first["backup"])
	if err != nil {
		t.Fatalf("the first backup is gone: %v", err)
	}
	if !strings.Contains(string(b), theirs) {
		t.Errorf("the first backup does not contain the value it was meant to preserve:\n%s", b)
	}

	// Our own URL on a different port takes the "repointed" branch, which backs up
	// unconditionally — a second real backup of the same file, back to back with the first.
	second, code := settings(t, "add", "--file", path, "--url", "http://127.0.0.1:19999/anthropic")
	if code != 0 {
		t.Fatalf("second add: exit %d, %v", code, second)
	}
	if first["backup"] == second["backup"] {
		t.Fatalf("both operations reported the same backup path %q, so one overwrote the other",
			first["backup"])
	}
}

// TestUninstallRestoresTheBaseURLItReplaced: after a --force install over somebody's own gateway,
// uninstall must hand it back. Deleting the key left them with NO base URL at all — a worse state
// than before they installed, and (with the backup defect above) unrecoverable from anything the
// tool produced.
func TestUninstallRestoresTheBaseURLItReplaced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	theirs := "https://gateway.corp.example/anthropic"
	writeJSON(t, path, map[string]any{"env": map[string]any{
		"ANTHROPIC_BASE_URL": theirs, "ANTHROPIC_AUTH_TOKEN": "keep"}})

	if _, code := settings(t, "add", "--file", path, "--url", ourURL, "--force"); code != 0 {
		t.Fatal("add --force failed")
	}
	facts, code := settings(t, "remove", "--file", path, "--url", ourURL)
	if code != 0 {
		t.Fatalf("remove: exit %d, %v", code, facts)
	}
	if facts["restored"] != theirs {
		t.Errorf("remove reported restored=%q, want %q", facts["restored"], theirs)
	}
	env, _ := readJSON(t, path)["env"].(map[string]any)
	if env["ANTHROPIC_BASE_URL"] != theirs {
		t.Fatalf("the user's own base URL was not restored: %v", env)
	}
	if env["ANTHROPIC_AUTH_TOKEN"] != "keep" {
		t.Errorf("an unrelated env var was lost: %v", env)
	}
	// And no bookkeeping left behind.
	if _, ok := readJSON(t, path)["$context-guru"]; ok {
		t.Errorf("uninstall left its own bookkeeping key in the user's settings")
	}
}

// TestSettingsPreservesFileMode: the file holds a credential often enough that widening its mode
// is a real leak. The temp file is created fresh, so os.replace took the UMASK mode rather than
// the replaced file's — a 600 settings file came back 644 under the common default.
//
// The fixture mode is 0640 and MUST NOT be 0600. At 0600 this test passed with the copymode/chmod
// block deleted entirely — because 0600 is exactly what `tempfile.mkstemp()` creates on its own, so
// the assertion held whether or not the mode was preserved. Confirmed by a reviewer who deleted that
// block and watched this test stay green while a real 0640 file was silently narrowed to 0600.
//
// 0640 is neither the mkstemp default nor a umask-derived mode, so passing it requires copymode to
// have run. The umask case the test was written for is still covered: 0640 is not 0644 either.
func TestSettingsPreservesFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes only")
	}
	const fixtureMode = 0o640 // deliberately not 0600 — see above
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	writeJSON(t, path, map[string]any{"env": map[string]any{"ANTHROPIC_AUTH_TOKEN": "secret"}})
	if err := os.Chmod(path, fixtureMode); err != nil {
		t.Fatal(err)
	}
	if _, code := settings(t, "add", "--file", path, "--url", ourURL); code != 0 {
		t.Fatal("add failed")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != fixtureMode {
		t.Errorf("mode after add = %o, want %o: this file holds a credential, and a mode that "+
			"merely happens to match mkstemp's default would prove nothing", got, fixtureMode)
	}
}

// TestSettingsFollowsASymlink: a dotfile-managed settings.json is commonly a symlink into a
// repository. os.replace onto the link path replaces the LINK with a regular file, so the edit
// never reaches the file the user manages and their dotfiles still hold the old content — while
// the tool reports success.
func TestSettingsFollowsASymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ")
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles", "settings.json")
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, real, map[string]any{"theme": "dark"})
	link := filepath.Join(dir, "settings.json")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot symlink here: %v", err)
	}

	if _, code := settings(t, "add", "--file", link, "--url", ourURL); code != 0 {
		t.Fatal("add failed")
	}
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced by a regular file, so the user's dotfiles repo never saw the edit")
	}
	env, _ := readJSON(t, real)["env"].(map[string]any)
	if env["ANTHROPIC_BASE_URL"] != ourURL {
		t.Errorf("the edit did not reach the real file: %v", readJSON(t, real))
	}
}

// TestSettingsRecognisesItsOwnURLOnAnotherPort: changing the configured port and re-running install
// used to report a conflict against context-guru itself, telling the user something else owned
// their routing.
func TestSettingsRecognisesItsOwnURLOnAnotherPort(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	// The state a previous install leaves: the URL it wrote, recorded.
	writeJSON(t, path, map[string]any{
		"env":           map[string]any{"ANTHROPIC_BASE_URL": "http://localhost:9999/anthropic"},
		"$context-guru": map[string]any{"installed_base_url": "http://localhost:9999/anthropic"},
	})

	facts, code := settings(t, "add", "--file", path, "--url", ourURL)
	if code != 0 || facts["result"] != "repointed" {
		t.Fatalf("expected a clean repoint, got exit %d, %v", code, facts)
	}
	if env, _ := readJSON(t, path)["env"].(map[string]any); env["ANTHROPIC_BASE_URL"] != ourURL {
		t.Errorf("not repointed: %v", env)
	}
	// Anything we did NOT record stays a conflict — including another LOCAL proxy, which is the
	// case a URL-shape rule got wrong: litellm's default is http://127.0.0.1:4000/anthropic, and
	// treating that as ours would have let uninstall delete somebody else's routing.
	for _, theirs := range []string{
		"https://8787.example.com/anthropic", // remote host that merely contains our port
		"http://127.0.0.1:4000/anthropic",    // another local proxy (litellm's default)
	} {
		p2 := filepath.Join(dir, "conflict.json")
		writeJSON(t, p2, map[string]any{"env": map[string]any{"ANTHROPIC_BASE_URL": theirs}})
		facts, code = settings(t, "add", "--file", p2, "--url", ourURL)
		if code != 2 || facts["result"] != "conflict" {
			t.Errorf("%s must be a conflict, not ours: exit %d, %v", theirs, code, facts)
		}
		if _, code := settings(t, "remove", "--file", p2, "--url", ourURL); code != 2 {
			t.Errorf("uninstall must refuse to remove %s", theirs)
		}
	}
}

// TestInstallRefusesAnUnverifiedDownload is the security fix, and it is the one to keep.
//
// A checksum MISMATCH was fatal, but an absent or unfetchable checksums.txt printed one advisory
// line and fell through to `install -m 755`. An unverified binary landed on a PATH directory and
// ran — a binary that handles all of the user's LLM traffic and holds their API key. The script's
// own comment said "a failure here is fatal, never a warning" while the code did the opposite.
func TestInstallRefusesAnUnverifiedDownload(t *testing.T) {
	requireTool(t, "bash")
	dir := t.TempDir()

	// A stub `curl` that serves a tarball and 404s the checksum file — exactly the shape of a
	// release whose checksums.txt is missing.
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(dir, "context-guru-proxy")
	if err := os.WriteFile(payload, []byte("#!/bin/sh\necho THIS BINARY WAS NEVER VERIFIED\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tarball := filepath.Join(dir, "payload.tar.gz")
	if out, err := exec.Command("tar", "czf", tarball, "-C", dir, "context-guru-proxy").CombinedOutput(); err != nil {
		t.Fatalf("tar: %v (%s)", err, out)
	}
	stub := "#!/usr/bin/env bash\n" +
		"# args end with the URL; -o <file> gives the destination\n" +
		"dest=\"\"; url=\"\"\n" +
		"while [ $# -gt 0 ]; do case \"$1\" in -o) dest=$2; shift 2;; -*) shift;; *) url=$1; shift;; esac; done\n" +
		"case \"$url\" in\n" +
		"  *checksums.txt) exit 22;;\n" +
		"  *api.github.com*) printf '{\"tag_name\": \"v9.9.9\"}' ${dest:+> \"$dest\"}; exit 0;;\n" +
		"  *.tar.gz) cp " + tarball + " \"$dest\"; exit 0;;\n" +
		"esac\nexit 22\n"
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(dir, "dest")
	cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "install.sh"))
	cmd.Env = append(sandboxEnv(t),
		"PATH="+bin+":"+os.Getenv("PATH"),
		"CONTEXT_GURU_DEST="+dest,
		"HOME="+dir,
	)
	out, err := cmd.CombinedOutput()
	t.Logf("install.sh output:\n%s", out)
	if err == nil {
		t.Error("install.sh succeeded without verifying the download")
	}
	if !strings.Contains(string(out), "checksum_unavailable") {
		t.Errorf("the refusal does not name the reason: %s", out)
	}
	if _, err := os.Stat(filepath.Join(dest, "context-guru-proxy")); err == nil {
		t.Fatal("an unverified binary was installed onto a PATH directory")
	}
}

// TestHookMakesTheProxyIdentifiable is the other half of the uninstall fix.
//
// The uninstall skill used to stop the proxy with `pkill -f "context-guru-proxy.*$PORT"`, which
// could not work: the starter passed the port through LISTEN_ADDR in the environment, so it
// appeared nowhere in the proxy's command line. The pattern matched no proxy — and did match the
// shell running it, i.e. the Bash tool of the user's own session, killing it mid-command while the
// proxy kept the port.
//
// So the starter now has to leave two handles behind, and this asserts both:
//
//  1. the port in `argv`, so `ps` and a human can tell instances apart;
//  2. a pidfile, which is what uninstall actually uses — no pattern matching at all.
func TestHookMakesTheProxyIdentifiable(t *testing.T) {
	requireTool(t, "bash")
	dir := t.TempDir()

	// A stand-in proxy that records its own argv and then holds the port, so the starter's
	// health probe succeeds and the script runs to completion.
	argvFile := filepath.Join(dir, "argv")
	py := requireTool(t, "python3")
	port := freePort(t)
	fake := filepath.Join(dir, "fake-proxy")
	script := "#!/usr/bin/env bash\n" +
		"printf '%s\\n' \"$*\" > " + argvFile + "\n" +
		"exec " + py + " -c '\n" +
		"import http.server\n" +
		"class H(http.server.BaseHTTPRequestHandler):\n" +
		"    def do_GET(self):\n" +
		"        self.send_response(200); self.end_headers(); self.wfile.write(b\"ok\")\n" +
		"    def log_message(self, *a): pass\n" +
		"http.server.HTTPServer((\"127.0.0.1\", " + port + "), H).serve_forever()\n'\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	state := filepath.Join(dir, "state")
	cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "start-proxy.sh"))
	cmd.Env = append(sandboxEnv(t),
		"CLAUDE_PLUGIN_OPTION_PORT="+port,
		"ANTHROPIC_BASE_URL=http://127.0.0.1:"+port+"/anthropic",
		"CONTEXT_GURU_BIN="+fake,
		"XDG_STATE_HOME="+state,
		"TMPDIR="+dir)
	out, err := cmd.CombinedOutput()
	t.Logf("start-proxy.sh:\n%s", out)
	t.Cleanup(func() {
		if b, e := os.ReadFile(filepath.Join(state, "context-guru", "proxy-"+port+".pid")); e == nil {
			exec.Command("kill", strings.TrimSpace(string(b))).Run() //nolint:errcheck
		}
	})
	if err != nil {
		t.Fatalf("start-proxy.sh failed: %v", err)
	}

	// (1) the port is on the command line.
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("the fake proxy never ran: %v", err)
	}
	if !strings.Contains(string(argv), "--listen") || !strings.Contains(string(argv), port) {
		t.Errorf("the proxy's argv does not carry its port (%q); nothing can identify this "+
			"instance among others, which is what made the old pkill pattern match the caller's "+
			"own shell instead", strings.TrimSpace(string(argv)))
	}
	// The dashboard must not be written into whatever directory the proxy started in — that is
	// the user's repository.
	if !strings.Contains(string(argv), "--dashboard-db") {
		t.Errorf("no explicit --dashboard-db, so the database lands in the current directory: %q", argv)
	}

	// (2) the pidfile exists, names a live process, and that process is ours.
	pidfile := filepath.Join(state, "context-guru", "proxy-"+port+".pid")
	b, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatalf("no pidfile at %s: uninstall has no handle but a pattern match: %v", pidfile, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		t.Fatalf("pidfile does not contain a pid: %q", b)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Errorf("pidfile names pid %d, which is not running: %v", pid, err)
	}
}

// freePort asks the kernel for a port and gives it back, so the fake proxy can bind it.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
	ln.Close()
	return p
}

// --- fixes from the review of #160 ---------------------------------------------------------

// hookTimeout reads a hook's timeout out of hooks.json rather than hardcoding it here.
//
// Read from the config on purpose: the defect below was a mismatch BETWEEN this file's budget and
// that file's timeout, so a test with the number retyped into it could pass while the pair drifted
// apart again. This way, lowering the timeout in hooks.json fails the test that depends on it.
func hookTimeout(t *testing.T, event string) time.Duration {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("hooks", "hooks.json"))
	if err != nil {
		t.Fatalf("reading hooks.json: %v", err)
	}
	var cfg struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("hooks.json does not parse: %v", err)
	}
	group, ok := cfg.Hooks[event]
	if !ok || len(group) == 0 || len(group[0].Hooks) == 0 {
		t.Fatalf("hooks.json has no %s hook", event)
	}
	secs := group[0].Hooks[0].Timeout
	if secs <= 0 {
		t.Fatalf("%s hook has no timeout in hooks.json", event)
	}
	return time.Duration(secs) * time.Second
}

// stallingPort returns a port with a listener that ACCEPTS connections and never answers.
//
// This is the shape that made the old iteration-counted health loop pathological: curl cannot
// return early, so every probe burns its full --max-time instead of failing instantly the way a
// refused port does. A hung proxy, a half-open socket, and an unrelated service holding the port
// all look like this, and none of them are exotic.
func stallingPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// Closing the listener is registered from the TEST goroutine; that is what unblocks Accept
	// below and lets the goroutine tear down its own connections.
	t.Cleanup(func() { ln.Close() })
	go func() {
		// `held` is touched by this goroutine only — appended here, closed by this deferred func.
		// An earlier version registered that cleanup with t.Cleanup from inside here, which reads
		// the slice from the test goroutine while this one appends to it: a data race that -race
		// caught in CI and a non-race run cannot see.
		var held []net.Conn
		defer func() {
			for _, c := range held {
				c.Close()
			}
		}()
		for {
			c, err := ln.Accept()
			if err != nil { // the listener was closed by the cleanup above
				return
			}
			held = append(held, c) // hold it open, answer nothing
		}
	}()
	return fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
}

// runCheck runs check-proxy.sh the way the UserPromptSubmit hook does, with a stand-in binary.
//
// `bin` is the script body of the fake proxy; the caller decides whether it ever binds the port.
func runCheck(t *testing.T, port, binBody string) (out string, code int, elapsed time.Duration) {
	t.Helper()
	requireTool(t, "bash")
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-proxy")
	if err := os.WriteFile(fake, []byte(binBody), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "check-proxy.sh"))
	cmd.Env = append(sandboxEnv(t),
		"CLAUDE_PLUGIN_ROOT="+root,
		"CLAUDE_PLUGIN_OPTION_PORT="+port,
		"ANTHROPIC_BASE_URL=http://127.0.0.1:"+port+"/anthropic",
		"CONTEXT_GURU_BIN="+fake,
		"XDG_STATE_HOME="+filepath.Join(dir, "state"),
		"TMPDIR="+dir)
	start := time.Now()
	b, err := cmd.CombinedOutput()
	elapsed = time.Since(start)
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running check-proxy.sh: %v (%s)", err, b)
	}
	t.Logf("check-proxy.sh -> exit %d in %v, output:\n%s", code, elapsed.Round(time.Millisecond), b)
	return string(b), code, elapsed
}

// TestCheckHookFinishesInsideItsOwnTimeout is the test whose absence hid a defect that defeated
// the hook's entire purpose.
//
// check-proxy.sh exists for one case: routing configured, nothing listening, and therefore a
// prompt that produces NOTHING — no error, no timeout the user can read. The hook replaces that
// silence with an explanation. But the explanation is the LAST thing the script prints, after it
// has tried to recover, and recovery called start-proxy.sh with its default 15s health wait. The
// measured total was 19s against a 10s hook timeout, so on exactly the path the hook was written
// for, Claude Code killed it first and the user saw nothing at all — the same symptom, now with a
// hook that was supposed to have fixed it.
//
// Asserted with real margin rather than "under the timeout": a check that only just fits is a
// check that fails on a loaded CI runner, and a flaky test here would get muted, which is how the
// property would be lost a second time.
func TestCheckHookFinishesInsideItsOwnTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hook is POSIX-only")
	}
	limit := hookTimeout(t, "UserPromptSubmit")
	// The WORST shape, deliberately: a port that accepts and never answers, so each of the three
	// probes costs its full --max-time on top of the health budget. Measuring the cheap shape (a
	// refused port, where curl returns instantly) would have let this assertion pass at almost any
	// timeout, which is the opposite of what it is for.
	port := stallingPort(t)
	out, code, elapsed := runCheck(t, port, "#!/usr/bin/env bash\nsleep 60\n")

	if code != 0 {
		t.Errorf("the hook exited %d; it must never fail a prompt", code)
	}
	if margin := limit / 2; elapsed > margin {
		t.Errorf("check-proxy.sh took %v on the dead-proxy path; hooks.json allows %v for "+
			"UserPromptSubmit, and this must finish inside half of that so a loaded machine "+
			"still gets the diagnostic (it is printed last, so a kill means the user sees nothing)",
			elapsed.Round(time.Millisecond), limit)
	}
	// The whole point of surviving is what it says. Compare on collapsed whitespace: the note is
	// hard-wrapped for a terminal, so a plain substring match depends on where the wrap lands.
	flat := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{
		"nothing is answering there",
		"Your request will hang with no error message",
		// This used to assert the literal "--dashboard", because the note restated the proxy's whole
		// command line and a copy of it that forgot those flags left the user with a 404 dashboard.
		// The note no longer restates anything — it points at start-proxy.sh, which always passes them
		// — so the string is gone by design rather than by regression.
		//
		// The property itself did not move to prose: TestCheckProxyRecoveryCommandIsTheRealLaunchPath
		// RUNS the printed command and asserts --dashboard in the argv the proxy is actually launched
		// with, which is strictly stronger than finding the word in a paragraph. What is left to check
		// here is that this path still hands the user something runnable at all.
		"start-proxy.sh",
		"--unrouted",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("the diagnostic is missing %q; output was:\n%s", want, out)
		}
	}
}

// TestCheckHookIsSilentWhereRoutingIsNotConfigured mirrors the SessionStart property, and matters
// more here: this hook runs on EVERY PROMPT in every project on the machine, not once per session.
// Anything it prints lands in the model's context for that turn, so a regression is noise on every
// turn the user takes, in projects that have nothing to do with context-guru.
func TestCheckHookIsSilentWhereRoutingIsNotConfigured(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hook is POSIX-only")
	}
	requireTool(t, "bash")
	// Last row is a POSITIVE CONTROL: without it every assertion here is an absence, and a
	// check-proxy.sh gutted to `exit 0` passes all of them. See the note in the SessionStart
	// equivalent — a control living in another test is one this test cannot rely on.
	for _, c := range []struct {
		name, baseURL string
		routed        bool
	}{
		{name: "unset"},
		{name: "another local proxy on a different port (e.g. litellm)", baseURL: "http://localhost:4000/anthropic"},
		{name: "a remote gateway", baseURL: "https://gateway.corp.example/anthropic"},
		// The gate was a PREFIX match, so port 8787 also matched 87871 — and this hook would then
		// probe and start our proxy underneath a user routed elsewhere on that port.
		{name: "our port as a PREFIX of a longer port", baseURL: "http://127.0.0.1:87871/anthropic"},
		{name: "POSITIVE CONTROL: a routed project with a dead proxy, where it must speak", routed: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			port := "8787"
			baseURL := c.baseURL
			if c.routed {
				port = freePort(t) // our own port, so a developer's real 8787 is never touched
				baseURL = "http://127.0.0.1:" + port + "/anthropic"
			}
			cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "check-proxy.sh"))
			cmd.Env = append(sandboxEnv(t),
				"CLAUDE_PLUGIN_OPTION_PORT="+port,
				"ANTHROPIC_BASE_URL="+baseURL,
				// No recovery attempt: the binary is absent, so this exercises the gate and the
				// diagnostic without waiting on a health budget.
				"CONTEXT_GURU_BIN=/nonexistent/never-run-me",
				"XDG_STATE_HOME="+dir,
				"TMPDIR="+dir)
			b, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("the hook must exit 0 everywhere: %v\n%s", err, b)
			}
			spoke := len(strings.TrimSpace(string(b))) != 0
			if c.routed {
				if !spoke {
					t.Error("the hook said nothing about a dead proxy in a ROUTED project — so " +
						"every silent row above is consistent with the script doing nothing at all")
				}
				return
			}
			if spoke {
				t.Errorf("the hook spoke in an unrouted project (base URL %q):\n%s", c.baseURL, b)
			}
		})
	}
}

// TestCheckHookRecoversSilently pins the deliberate choice to keep the diagnostic LAST.
//
// The common case for this hook is an --idle-exit between two prompts: the proxy is gone, it comes
// back, and the user should never know. Printing the note up front would guarantee it is seen when
// the hook is killed, but it would also put a paragraph about a dead proxy into the context of
// every successful recovery. Silence on success is what makes the ordering worth defending — and
// it is only safe because the recovery is now budgeted, which the timeout test above enforces.
func TestCheckHookRecoversSilently(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hook is POSIX-only")
	}
	py := requireTool(t, "python3")
	port := freePort(t)
	fake := "#!/usr/bin/env bash\nexec " + py + " -c '\n" +
		"import http.server\n" +
		"class H(http.server.BaseHTTPRequestHandler):\n" +
		"    def do_GET(self):\n" +
		"        self.send_response(200); self.end_headers(); self.wfile.write(b\"ok\")\n" +
		"    def log_message(self, *a): pass\n" +
		"http.server.HTTPServer((\"127.0.0.1\", " + port + "), H).serve_forever()\n'\n"

	t.Cleanup(func() { exec.Command("pkill", "-f", "127.0.0.1\", "+port).Run() }) //nolint:errcheck
	out, code, _ := runCheck(t, port, fake)
	if code != 0 {
		t.Errorf("exit %d; the hook must never fail a prompt", code)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("the hook recovered the proxy but still spoke — this output goes into the "+
			"model's context on a turn where nothing is wrong:\n%s", out)
	}
	resp, err := http.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		t.Fatalf("the hook was silent but the proxy is not up — silence must mean success: %v", err)
	}
	resp.Body.Close()
}

// TestStartHookBudgetsOnWallClockNotIterations covers the shape that made the failure report
// unreachable.
//
// The health wait was `for _ in $(seq 1 60)` with `--max-time 2`, commented "up to ~15s". That is
// only true when the port is REFUSED, where curl returns instantly. Against a socket that accepts
// and never answers — a hung proxy, or an unrelated service on the port — each probe burned its
// full timeout: measured 2046ms, so ~122s against a 60s SessionStart timeout. The hook was killed,
// so the block that prints the log path and the /context-guru:status pointer never ran. A hung
// port is one of the likeliest reasons to need that block, and it was the one case that never
// produced it.
func TestStartHookBudgetsOnWallClockNotIterations(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hook is POSIX-only")
	}
	requireTool(t, "bash")
	port := stallingPort(t)
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-proxy")
	if err := os.WriteFile(fake, []byte("#!/usr/bin/env bash\nsleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	limit := hookTimeout(t, "SessionStart")
	cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "start-proxy.sh"))
	cmd.Env = append(sandboxEnv(t),
		"CLAUDE_PLUGIN_OPTION_PORT="+port,
		"ANTHROPIC_BASE_URL=http://127.0.0.1:"+port+"/anthropic",
		"CONTEXT_GURU_BIN="+fake,
		"CONTEXT_GURU_HEALTH_BUDGET=3",
		"XDG_STATE_HOME="+filepath.Join(dir, "state"),
		"TMPDIR="+dir)
	start := time.Now()
	b, err := cmd.CombinedOutput()
	elapsed := time.Since(start)
	t.Logf("start-proxy.sh against an accept-and-stall port -> %v in %v, output:\n%s",
		err, elapsed.Round(time.Millisecond), b)
	if err != nil {
		t.Fatalf("the hook must exit 0 even here: %v", err)
	}
	if elapsed > limit/2 {
		t.Errorf("took %v against a stalling port; SessionStart allows %v, and the budget was 3s "+
			"— an iteration-counted loop reaches ~122s here and gets killed", elapsed, limit)
	}
	// The point of finishing early is that this actually gets said.
	if !strings.Contains(string(b), "did not come up") {
		t.Errorf("the failure report never ran — that is the whole defect:\n%s", b)
	}
}

// TestUninstallRefusesAForeignBaseURLEvenWithNoURLGiven is the regression test for the worst
// defect this plugin has had.
//
// `remove` guarded its conflict check with `if args.url and ...`, so the documented invocation
// with no --url skipped the check entirely and deleted whatever base URL was configured. Measured
// before the fix: a corporate gateway with no context-guru record came out `result=removed`,
// `restored=` empty, exit 0 — the user's gateway silently gone, reported as success.
//
// The file must come back BYTE-IDENTICAL, not merely "still have a base URL": a refusal that
// rewrites the file has already done the thing it refused.
func TestUninstallRefusesAForeignBaseURLEvenWithNoURLGiven(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	const foreign = `{
  "env": {
    "ANTHROPIC_BASE_URL": "https://gateway.corp.example.com",
    "ANTHROPIC_AUTH_TOKEN": "sk-corp-secret"
  },
  "permissions": {"allow": ["Bash(ls:*)"]}
}
`
	if err := os.WriteFile(path, []byte(foreign), 0o600); err != nil {
		t.Fatal(err)
	}

	facts, code := settings(t, "remove", "--file", path)
	if code != 2 {
		t.Errorf("exit %d for a base URL we never installed; want 2 (conflict). facts=%v", code, facts)
	}
	if facts["result"] != "conflict" {
		t.Errorf("result=%q, want conflict", facts["result"])
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != foreign {
		t.Errorf("the file was rewritten while refusing to change it:\n--- got ---\n%s\n--- want ---\n%s",
			got, foreign)
	}
	// And with no --url, a URL we DID record must still be removable, or uninstall is broken.
	facts, code = settings(t, "add", "--file", path, "--url", "http://127.0.0.1:8787/anthropic", "--force")
	if code != 0 {
		t.Fatalf("add --force failed: %v", facts)
	}
	facts, code = settings(t, "remove", "--file", path)
	if code != 0 || facts["result"] != "removed" {
		t.Errorf("a recorded URL must be removable with no --url: exit %d, facts=%v", code, facts)
	}
}

// TestInstallReportsPATHFromTheSourceFallbackToo covers a silence, which is why it went unnoticed.
//
// try_source_build returned success and the script exited right there, bypassing the shared tail —
// so a user who landed on the `go install` fallback got `result=installed` and NO `on_path` line.
// ~/.local/bin frequently is not on PATH, and install/SKILL.md only warns when it reads on_path=false,
// so nothing said anything. The failure surfaced later, in a different session, as the SessionStart
// hook reporting "the proxy binary is not on PATH", with nothing tying it back to the install.
func TestInstallReportsPATHFromTheSourceFallbackToo(t *testing.T) {
	requireTool(t, "bash")
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}

	// curl: resolve a release, then 404 the tarball — a published tag with no asset for this
	// platform, which is exactly what sends the script to the source fallback.
	stub := "#!/usr/bin/env bash\n" +
		"dest=\"\"; url=\"\"\n" +
		"while [ $# -gt 0 ]; do case \"$1\" in -o) dest=$2; shift 2;; -*) shift;; *) url=$1; shift;; esac; done\n" +
		"case \"$url\" in\n" +
		"  *api.github.com*) printf '{\"tag_name\": \"v9.9.9\"}' ${dest:+> \"$dest\"}; exit 0;;\n" +
		"esac\nexit 22\n"
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	// A stand-in `go` that installs into GOBIN the way the real one does, without a toolchain.
	goStub := "#!/usr/bin/env bash\n" +
		"[ \"$1\" = install ] || exit 1\n" +
		"mkdir -p \"$GOBIN\" && printf '#!/bin/sh\\ntrue\\n' > \"$GOBIN/context-guru-proxy\"\n" +
		"chmod 755 \"$GOBIN/context-guru-proxy\"\n"
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(goStub), 0o755); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(dir, "dest")
	run := func(pathHasDest bool) map[string]string {
		path := bin + ":" + os.Getenv("PATH")
		if pathHasDest {
			path = dest + ":" + path
		}
		cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "install.sh"))
		cmd.Env = append(sandboxEnv(t), "PATH="+path, "CONTEXT_GURU_DEST="+dest, "HOME="+dir)
		out, err := cmd.CombinedOutput()
		t.Logf("install.sh (dest on PATH=%v) -> %v, output:\n%s", pathHasDest, err, out)
		if err != nil {
			t.Fatalf("the source fallback should have succeeded: %v", err)
		}
		facts := map[string]string{}
		for _, line := range strings.Split(string(out), "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
				facts[k] = v
			}
		}
		return facts
	}

	facts := run(false)
	if facts["result"] != "installed" || facts["built_from"] != "source" {
		t.Fatalf("the fallback did not report a source install: %v", facts)
	}
	if facts["on_path"] != "false" {
		t.Errorf("on_path=%q with $DEST off PATH; the fallback must report this or the user only "+
			"finds out from a hook in a later session: %v", facts["on_path"], facts)
	}
	if facts["note"] == "" {
		t.Errorf("no actionable note accompanied on_path=false: %v", facts)
	}
	if facts["fallback"] != "go_install_attempted" {
		t.Errorf("fallback=%q — the line is printed before the build, so it must not read as "+
			"proof the build worked: %v", facts["fallback"], facts)
	}

	if facts := run(true); facts["on_path"] != "true" {
		t.Errorf("on_path=%q with $DEST on PATH: %v", facts["on_path"], facts)
	}
}

// TestBackupPruningSurvivesAGlobbyPath: `glob.glob` reads `[`, `?` and `*` in the PATH as pattern
// syntax, so for a settings file under a directory like `foo[1]` the prune matched nothing and
// silently did nothing — forever. Invisible by construction, because pruning is best-effort, and
// the backups KEEP_BACKUPS exists to bound then grow without limit.
//
// Repeated `add` calls, not add/remove pairs: a clean `remove` now deletes every backup for the
// file outright (see forget_backups()), so an add/remove loop would exercise that instead of the
// rolling KEEP_BACKUPS window this test is actually about. Repointing to a new port each time hits
// the dedicated "own URL on another port" branch, which takes a real backup on every call without
// ever uninstalling.
func TestBackupPruningSurvivesAGlobbyPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proj[1]", ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "settings.json")
	writeJSON(t, path, map[string]any{"env": map[string]any{"KEEP": "yes"}})

	// 16 calls comfortably exceeds KEEP_BACKUPS (10): the first is a fresh add, the rest each
	// repoint to a new port, and every one of those takes its own backup unconditionally.
	for i := 0; i < 16; i++ {
		url := fmt.Sprintf("http://127.0.0.1:%d/anthropic", 8787+i)
		if _, code := settings(t, "add", "--file", path, "--url", url); code != 0 {
			t.Fatalf("add %d failed", i)
		}
	}
	entries, err := os.ReadDir(filepath.Join(dir, "context-guru-settings-json"))
	if err != nil {
		t.Fatal(err)
	}
	backups := 0
	for _, e := range entries {
		if strings.Contains(e.Name(), ".context-guru-backup-") {
			backups++
		}
	}
	t.Logf("%d backups left under a path containing [ ]", backups)
	if backups > 11 { // KEEP_BACKUPS plus the one just written
		t.Errorf("%d backups accumulated under a globby path — pruning never matched anything", backups)
	}
}

// skillBlock returns the fenced ```bash block in a skill file that contains `needle`.
//
// The skills are prompts, but the destructive steps in them are shell that gets run verbatim. So
// the ones that can hurt somebody are extracted and EXECUTED here rather than reviewed by reading:
// reading is exactly what missed the ordering defect this covers, twice.
func skillBlock(t *testing.T, skill, needle string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("skills", skill, "SKILL.md"))
	if err != nil {
		t.Fatalf("reading skill: %v", err)
	}
	var blocks []string
	var cur []string
	in := false
	for _, line := range strings.Split(string(b), "\n") {
		switch {
		case !in && strings.HasPrefix(line, "```bash"):
			in, cur = true, nil
		case in && strings.HasPrefix(line, "```"):
			in = false
			blocks = append(blocks, strings.Join(cur, "\n"))
		case in:
			cur = append(cur, line)
		}
	}
	found := ""
	for _, blk := range blocks {
		if strings.Contains(blk, needle) {
			if found != "" {
				t.Fatalf("%s/SKILL.md has more than one bash block containing %q; the test cannot "+
					"tell which one is the destructive path", skill, needle)
			}
			found = blk
		}
	}
	if found == "" {
		t.Fatalf("no bash block in %s/SKILL.md contains %q", skill, needle)
	}
	return found
}

// TestUninstallDoesNotSignalAProcessThatIsNotOurs executes the uninstall skill's stop-the-proxy
// block against a PID that is NOT a context-guru proxy.
//
// The block used to send the signal first and document the ownership check as a SEPARATE snippet
// below it — so executed the way it reads, top to bottom, `kill "$pid"` had already run by the time
// the guard was reached. That matters most on the lsof/ss fallback, which exists precisely for a
// stale pidfile or a hand-started proxy: the cases where the PID may belong to something else. A
// recycled PID satisfies `kill -0` perfectly well.
//
// This is the same shape as the defect that had uninstall killing the user's own Claude Code
// session, which is the reason it gets an executing test rather than careful prose.
func TestUninstallDoesNotSignalAProcessThatIsNotOurs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell snippet is POSIX-only")
	}
	requireTool(t, "bash")
	block := skillBlock(t, "uninstall", `kill "$pid"`)
	// The block is a TEMPLATE: the skill tells the model to obtain the configured port (from
	// `settings.py config`, because CLAUDE_PLUGIN_OPTION_* is invisible to a Bash tool call) and put it
	// in. So fill the placeholder exactly as the model is instructed to, and fail loudly if it is
	// missing — an unsubstituted `PORT="<port>"` would make every path below look inert for the wrong
	// reason, which is what happened when the placeholder was introduced.
	const placeholder = `PORT="<port>"`
	if !strings.Contains(block, placeholder) {
		t.Fatalf("the uninstall block no longer carries %s; if the port is obtained differently now, "+
			"this test needs to follow suit rather than execute a stale template:\n%s", placeholder, block)
	}
	block = strings.Replace(block, placeholder, `PORT="8787"`, 1)

	for _, c := range []struct {
		name       string
		psReports  string // what `ps -p <pid> -o command=` prints
		wantKilled bool
	}{
		{"a stranger's process on our port", "/usr/bin/postgres -D /var/lib/postgres", false},
		{"a proxy of ours", "context-guru-proxy --listen 127.0.0.1:8787 --preset cache", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			stubs := filepath.Join(dir, "bin")
			if err := os.MkdirAll(stubs, 0o755); err != nil {
				t.Fatal(err)
			}
			killLog := filepath.Join(dir, "kill.log")
			write := func(name, body string) {
				if err := os.WriteFile(filepath.Join(stubs, name), []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			write("ps", "#!/usr/bin/env bash\nprintf '%s\\n' "+strconv.Quote(c.psReports)+"\n")
			// No socket-owner lookup: the pidfile below is what supplies the PID.
			write("lsof", "#!/usr/bin/env bash\nexit 1\n")
			write("ss", "#!/usr/bin/env bash\nexit 1\n")

			state := filepath.Join(dir, "state", "context-guru")
			if err := os.MkdirAll(state, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(state, "proxy-8787.pid"), []byte("424242\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			// `kill` must be intercepted by a FUNCTION, not a file on PATH: it is a bash builtin,
			// so a PATH stub is never consulted — the first version of this test stubbed a file,
			// the block's `kill -0` liveness probe therefore failed for a pid that does not exist,
			// it fell through to the socket-owner lookup and the run exercised none of the branch
			// under test. It records the signal rather than sending one, and answers `kill -0` as
			// "alive" so the pidfile path is the one taken.
			preamble := "kill() {\n" +
				"  if [ \"$1\" = -0 ]; then return 0; fi\n" +
				"  printf '%s\\n' \"$*\" >> " + strconv.Quote(killLog) + "\n" +
				"}\n"
			cmd := exec.Command("bash", "-c", preamble+block)
			cmd.Env = append(sandboxEnv(t),
				"PATH="+stubs+":"+os.Getenv("PATH"),
				"CLAUDE_PLUGIN_OPTION_PORT=8787",
				"XDG_STATE_HOME="+filepath.Join(dir, "state"))
			out, err := cmd.CombinedOutput()
			t.Logf("uninstall stop-block -> %v, output:\n%s", err, out)

			logged, _ := os.ReadFile(killLog)
			killed := strings.Contains(string(logged), "424242")
			if killed != c.wantKilled {
				t.Errorf("kill invoked = %v, want %v (ps reported %q). kill log: %q\nblock output:\n%s",
					killed, c.wantKilled, c.psReports, logged, out)
			}
			if !c.wantKilled && !strings.Contains(string(out), "NOT OURS") {
				t.Errorf("the block signalled nothing but also said nothing about why:\n%s", out)
			}
			// A pidfile belonging to someone else's process must survive: removing it would strand
			// a proxy of ours that is still running under a different pid.
			_, statErr := os.Stat(filepath.Join(state, "proxy-8787.pid"))
			if !c.wantKilled && statErr != nil {
				t.Errorf("the pidfile was removed for a process we refused to touch: %v", statErr)
			}
		})
	}
}

// TestChainingUpstreamSurvivesIntoLaterSessions covers the gap the first hosted-agent install hit.
//
// On a platform whose own gateway holds the credential and rewrites model names, the proxy has to
// chain behind it — and the SessionStart hook reads its configuration from the settings env block.
// With ANTHROPIC_UPSTREAM set only in the installing shell's environment, chaining worked until the
// proxy idled out; the next session's hook then started one aimed at api.anthropic.com, where every
// request fails. So `add --upstream` writes both keys in one atomic save.
//
// The removal half matters as much: uninstall must take back only an upstream it recorded writing.
// Deleting one the user set themselves is the same overreach as deleting a base URL we never
// installed.
func TestChainingUpstreamSurvivesIntoLaterSessions(t *testing.T) {
	const theirGateway = "http://127.0.0.1:24180"

	t.Run("written and removed as a pair", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "settings.json")
		writeJSON(t, path, map[string]any{"env": map[string]any{"MINE": "keep"}})

		if facts, code := settings(t, "add", "--file", path, "--url", ourURL,
			"--upstream", theirGateway); code != 0 || facts["result"] != "added" {
			t.Fatalf("add --upstream failed: %v", facts)
		}
		env := readJSON(t, path)["env"].(map[string]any)
		if env["ANTHROPIC_UPSTREAM"] != theirGateway {
			t.Errorf("ANTHROPIC_UPSTREAM = %v, want %q — without it the hook starts an unchained "+
				"proxy in every later session", env["ANTHROPIC_UPSTREAM"], theirGateway)
		}
		if env["MINE"] != "keep" {
			t.Error("the user's own env var was lost")
		}

		if facts, code := settings(t, "remove", "--file", path); code != 0 {
			t.Fatalf("remove failed: %v", facts)
		}
		env = readJSON(t, path)["env"].(map[string]any)
		if _, still := env["ANTHROPIC_UPSTREAM"]; still {
			t.Error("uninstall left our upstream key behind")
		}
		if env["MINE"] != "keep" {
			t.Error("removal took the user's own env var with it")
		}
	})

	t.Run("an upstream we did NOT write is left alone", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "settings.json")
		writeJSON(t, path, map[string]any{"env": map[string]any{"MINE": "keep"}})

		// Routed by us, but the upstream is the user's own — no --upstream on the add.
		if _, code := settings(t, "add", "--file", path, "--url", ourURL); code != 0 {
			t.Fatal("add failed")
		}
		data := readJSON(t, path)
		env := data["env"].(map[string]any)
		env["ANTHROPIC_UPSTREAM"] = "https://their-own-choice.example"
		writeJSON(t, path, data)

		if _, code := settings(t, "remove", "--file", path); code != 0 {
			t.Fatal("remove failed")
		}
		env = readJSON(t, path)["env"].(map[string]any)
		if env["ANTHROPIC_UPSTREAM"] != "https://their-own-choice.example" {
			t.Errorf("uninstall deleted an upstream it never wrote: %v", env)
		}
	})
}

// TestBinPathSurvivesAMachineWhereItIsNotOnPATH is the hosted-agent case, and it is about a
// SILENT failure rather than a visible one.
//
// The SessionStart hook resolves the proxy by NAME. On a machine where the install directory is not
// on PATH — `~/.local/bin` very often is not, and on an agent pod the writable dirs reset on restart
// so "edit your shell profile" does not survive — the install succeeds, routing works for the session
// that set it up, and the auto-restart hook then never finds the binary again. The failure mode it
// exists to catch is a hang with no error, so nothing announces that the safety net is gone.
//
// `--bin` writes the absolute path into the env block the hook inherits. As with the upstream,
// uninstall must take back only a path it recorded writing.
func TestBinPathSurvivesAMachineWhereItIsNotOnPATH(t *testing.T) {
	const absPath = "/home/agent/.local/bin/context-guru-proxy"

	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	writeJSON(t, path, map[string]any{"env": map[string]any{"MINE": "keep"}})

	if facts, code := settings(t, "add", "--file", path, "--url", ourURL, "--bin", absPath); code != 0 {
		t.Fatalf("add --bin failed: %v", facts)
	}
	env := readJSON(t, path)["env"].(map[string]any)
	if env["CONTEXT_GURU_BIN"] != absPath {
		t.Errorf("CONTEXT_GURU_BIN = %v, want %q — without it the hook cannot find the proxy on a "+
			"machine where its directory is not on PATH, and nothing says so", env["CONTEXT_GURU_BIN"], absPath)
	}

	if _, code := settings(t, "remove", "--file", path); code != 0 {
		t.Fatal("remove failed")
	}
	env = readJSON(t, path)["env"].(map[string]any)
	if _, still := env["CONTEXT_GURU_BIN"]; still {
		t.Error("uninstall left our binary path behind")
	}
	if env["MINE"] != "keep" {
		t.Error("removal took the user's own env var with it")
	}

	// A CONTEXT_GURU_BIN the user set themselves is theirs to keep.
	writeJSON(t, path, map[string]any{"env": map[string]any{"MINE": "keep"}})
	if _, code := settings(t, "add", "--file", path, "--url", ourURL); code != 0 {
		t.Fatal("add failed")
	}
	data := readJSON(t, path)
	env = data["env"].(map[string]any)
	env["CONTEXT_GURU_BIN"] = "/opt/their/own/build"
	writeJSON(t, path, data)
	if _, code := settings(t, "remove", "--file", path); code != 0 {
		t.Fatal("remove failed")
	}
	if got := readJSON(t, path)["env"].(map[string]any)["CONTEXT_GURU_BIN"]; got != "/opt/their/own/build" {
		t.Errorf("uninstall deleted a binary path it never wrote: %v", got)
	}
}

// --- fixes from the second review of #160 -----------------------------------------------------

// TestReRunningTheInstallFillsInMissingKeys is the defect that defeated the remedy for every other
// failure in this flow.
//
// `unchanged` used to mean "the base URL matches", and the two early returns in cmd_add wrote nothing
// else — so `add --url <same> --upstream … --bin …` reported success and wrote NEITHER new key. Since
// re-running the install is the obvious thing to do after an attempt dies partway (which is how every
// hosted-agent attempt ended), the repair was a silent no-op. The repointed path had the same hole, so
// changing the configured port un-chained the proxy without a word.
func TestReRunningTheInstallFillsInMissingKeys(t *testing.T) {
	const gw = "http://gw.example:4000"
	const bin = "/opt/cg/context-guru-proxy"

	ourKeys := func(path string) map[string]any {
		t.Helper()
		env, _ := readJSON(t, path)["env"].(map[string]any)
		out := map[string]any{}
		for _, k := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_UPSTREAM", "CONTEXT_GURU_BIN"} {
			if v, ok := env[k]; ok {
				out[k] = v
			}
		}
		return out
	}

	t.Run("re-run adds the keys the first run did not have", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "settings.json")
		writeJSON(t, path, map[string]any{"env": map[string]any{}})

		if _, code := settings(t, "add", "--file", path, "--url", ourURL); code != 0 {
			t.Fatal("first add failed")
		}
		facts, code := settings(t, "add", "--file", path, "--url", ourURL, "--upstream", gw, "--bin", bin)
		if code != 0 {
			t.Fatalf("re-run failed: %v", facts)
		}
		if facts["result"] == "unchanged" {
			t.Errorf("result=unchanged on a re-run that had two keys to add — this is the no-op that "+
				"made re-running the install useless as a repair: %v", facts)
		}
		got := ourKeys(path)
		if got["ANTHROPIC_UPSTREAM"] != gw || got["CONTEXT_GURU_BIN"] != bin {
			t.Errorf("re-run did not write the missing keys: %v", got)
		}
	})

	t.Run("unchanged only when there is genuinely nothing to add", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "settings.json")
		writeJSON(t, path, map[string]any{"env": map[string]any{}})
		if _, code := settings(t, "add", "--file", path, "--url", ourURL,
			"--upstream", gw, "--bin", bin); code != 0 {
			t.Fatal("add failed")
		}
		facts, code := settings(t, "add", "--file", path, "--url", ourURL, "--upstream", gw, "--bin", bin)
		if code != 0 || facts["result"] != "unchanged" {
			t.Errorf("a complete install re-run should be unchanged: %v", facts)
		}
	})

	t.Run("a port change keeps the chaining keys", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "settings.json")
		writeJSON(t, path, map[string]any{"env": map[string]any{}})
		if _, code := settings(t, "add", "--file", path, "--url", ourURL,
			"--upstream", gw, "--bin", bin); code != 0 {
			t.Fatal("add failed")
		}
		const moved = "http://127.0.0.1:9999/anthropic"
		facts, code := settings(t, "add", "--file", path, "--url", moved, "--upstream", gw, "--bin", bin)
		if code != 0 || facts["result"] != "repointed" {
			t.Fatalf("expected repointed: %v", facts)
		}
		got := ourKeys(path)
		if got["ANTHROPIC_BASE_URL"] != moved {
			t.Errorf("port change did not move the base URL: %v", got)
		}
		if got["ANTHROPIC_UPSTREAM"] != gw || got["CONTEXT_GURU_BIN"] != bin {
			t.Errorf("a port change silently un-chained the proxy: %v", got)
		}
	})

	t.Run("other_env_keys counts the user's keys, not ours", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "settings.json")
		writeJSON(t, path, map[string]any{"env": map[string]any{"MINE": "one"}})
		facts, _ := settings(t, "add", "--file", path, "--url", ourURL, "--upstream", gw, "--bin", bin)
		if facts["other_env_keys"] != "1" {
			t.Errorf("other_env_keys=%q; the user owns exactly one env var, and reporting our own "+
				"keys as theirs made /context-guru:status repeat the wrong number back to them",
				facts["other_env_keys"])
		}
	})
}

// TestStartProxyReportsArgumentsItCannotUse: three malformed invocations launched a proxy and reported
// success while doing the wrong thing, because the argument loop had no default branch and took the
// next word as a value on faith.
//
//	--upsteam <url>        one transposed letter -> no upstream at all
//	--upstream             missing value         -> no upstream at all
//	--upstream --bin <p>   swallowed the flag    -> upstream="--bin", and --bin lost too
//
// The first two leave a proxy forwarding to api.anthropic.com, which on a platform whose gateway
// rewrites model names makes every request fail. Exiting non-zero is not the fix — this script must
// never fail a session — so it says what it discarded, for the same reason the declined gate leaves a
// breadcrumb: silence is indistinguishable from "never ran".
func TestStartProxyReportsArgumentsItCannotUse(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hook is POSIX-only")
	}
	requireTool(t, "bash")

	for _, c := range []struct {
		name     string
		args     []string
		wantSaid string
		wantUp   string // expected --anthropic-upstream value in argv, "" for none
	}{
		{"good", []string{"--upstream", "http://gw:4000"}, "", "http://gw:4000"},
		{"typo", []string{"--upsteam", "http://gw:4000"}, "unrecognised argument '--upsteam'", ""},
		{"no value", []string{"--upstream"}, "needs a value", ""},
		{"nonsense", []string{"--nonsense"}, "unrecognised argument '--nonsense'", ""},
		// The empty `=` forms, which are how a caller interpolating an unset variable arrives here.
		{"empty =value", []string{"--upstream="}, "needs a value", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			argv := filepath.Join(dir, "argv.log")
			fake := filepath.Join(dir, "fake")
			body := "#!/usr/bin/env bash\nprintf '%s\\n' \"$*\" >> " + argv + "\nsleep 30\n"
			if err := os.WriteFile(fake, []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
			port := freePort(t)
			cmd := exec.Command("bash", append([]string{
				filepath.Join(scriptsDir(t), "start-proxy.sh"), "--unrouted", "--bin", fake,
			}, c.args...)...)
			cmd.Env = append(sandboxEnv(t),
				"CLAUDE_PLUGIN_OPTION_PORT="+port,
				"ANTHROPIC_BASE_URL=",
				"ANTHROPIC_UPSTREAM=",
				"CLAUDE_PLUGIN_OPTION_UPSTREAM=",
				"CONTEXT_GURU_HEALTH_BUDGET=1",
				"XDG_STATE_HOME="+filepath.Join(dir, "state"),
				"TMPDIR="+dir)
			out, err := cmd.CombinedOutput()
			t.Cleanup(func() { exec.Command("pkill", "-f", fake).Run() }) //nolint:errcheck
			if err != nil {
				t.Fatalf("must never fail a session: %v\n%s", err, out)
			}
			said := string(out)
			if c.wantSaid == "" {
				if strings.Contains(said, "ignoring") {
					t.Errorf("complained about a valid invocation:\n%s", said)
				}
			} else if !strings.Contains(said, c.wantSaid) {
				t.Errorf("did not report the discarded argument (want %q):\n%s", c.wantSaid, said)
			}
			launched, _ := os.ReadFile(argv)
			if c.wantUp == "" {
				if strings.Contains(string(launched), "--anthropic-upstream") {
					t.Errorf("an upstream reached argv from a malformed argument: %s", launched)
				}
			} else if !strings.Contains(string(launched), "--anthropic-upstream "+c.wantUp) {
				t.Errorf("argv missing the upstream: %s", launched)
			}
		})
	}
}

// TestRejectingAValueDoesNotEatTheNextFlag is the row that used to prove nothing.
//
// `--upstream --bin <path>` was in the table above with assertions "it says 'needs a value'" and "no
// upstream reaches argv" — and the BUGGY parser satisfies both. Reconstructed and measured by the
// reviewer: with the shift outside the accepted branch, rejecting the value still consumed `--bin`, so
// the message appeared, no upstream reached argv, and the row was green either way.
//
// What actually separates the two versions is `--bin`: the buggy parser eats it (falling back to
// resolving the binary by name, which fails on the machines --bin exists for), the fixed one rejects
// `--upstream` alone and then parses `--bin <path>` normally.
//
// So this asserts the POSITIVE — the proxy was launched via the binary that --bin named. A test whose
// only assertions are absences cannot distinguish "handled correctly" from "handled wrongly in a way
// that happens to be quiet", which is the lesson three of this week's defects share.
func TestRejectingAValueDoesNotEatTheNextFlag(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hook is POSIX-only")
	}
	requireTool(t, "bash")
	dir := t.TempDir()
	argv := filepath.Join(dir, "argv.log")
	fake := filepath.Join(dir, "fake-proxy")
	body := "#!/usr/bin/env bash\nprintf '%s\\n' \"$*\" >> " + argv + "\nsleep 30\n"
	if err := os.WriteFile(fake, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)

	// --upstream has no value; --bin follows it and must survive.
	cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "start-proxy.sh"),
		"--unrouted", "--upstream", "--bin", fake)
	cmd.Env = append(sandboxEnv(t),
		"CLAUDE_PLUGIN_OPTION_PORT="+port,
		"ANTHROPIC_BASE_URL=",
		"ANTHROPIC_UPSTREAM=",
		"CLAUDE_PLUGIN_OPTION_UPSTREAM=",
		"CONTEXT_GURU_BIN=",
		"CONTEXT_GURU_HEALTH_BUDGET=1",
		"XDG_STATE_HOME="+filepath.Join(dir, "state"),
		"TMPDIR="+dir)
	out, err := cmd.CombinedOutput()
	t.Cleanup(func() { exec.Command("pkill", "-f", fake).Run() }) //nolint:errcheck
	if err != nil {
		t.Fatalf("must never fail a session: %v\n%s", err, out)
	}
	t.Logf("output:\n%s", out)

	// The positive: --bin was honoured, so the proxy actually started from that path.
	launched, _ := os.ReadFile(argv)
	if len(launched) == 0 {
		t.Errorf("--bin was eaten as --upstream's rejected value, so the binary fell back to name "+
			"resolution and nothing started. Output was:\n%s", out)
	}
	// And the rejection still had to be reported.
	if !strings.Contains(string(out), "needs a value") {
		t.Errorf("the rejected --upstream was not reported:\n%s", out)
	}
	// The buggy parser reports the path as a stray argument; the fixed one consumes it as --bin's value.
	if strings.Contains(string(out), "unrecognised argument '"+fake+"'") {
		t.Errorf("the path after --bin was treated as a stray argument, which means --bin was "+
			"consumed by the rejected --upstream:\n%s", out)
	}
	if strings.Contains(string(launched), "--anthropic-upstream") {
		t.Errorf("an upstream reached argv from a rejected value: %s", launched)
	}
}

// --- settings.py's statusLine key ---------------------------------------------------------

// TestSettingsStatuslineRoundTrips: add --statusline writes the top-level key alongside the
// existing env changes in ONE atomic save, and remove takes back only what it recorded writing —
// the exact shape already proven for env.CONTEXT_GURU_BIN and env.ANTHROPIC_UPSTREAM above,
// applied to a key that lives outside `env` entirely.
func TestSettingsStatuslineRoundTrips(t *testing.T) {
	const cmdPath = "/opt/cg/statusline.py"
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	writeJSON(t, path, map[string]any{"theme": "dark"})

	facts, code := settings(t, "add", "--file", path, "--url", ourURL, "--statusline", cmdPath)
	if code != 0 || facts["result"] != "added" {
		t.Fatalf("add --statusline failed: exit %d, %v", code, facts)
	}
	got := readJSON(t, path)
	sl, _ := got["statusLine"].(map[string]any)
	if sl["type"] != "command" || sl["command"] != cmdPath {
		t.Fatalf("statusLine = %v, want {type: command, command: %q}", got["statusLine"], cmdPath)
	}
	if got["theme"] != "dark" {
		t.Error("an unrelated top-level setting was lost")
	}

	// Re-adding the identical install is a no-op, same as the base_url side.
	facts, code = settings(t, "add", "--file", path, "--url", ourURL, "--statusline", cmdPath)
	if code != 0 || facts["result"] != "unchanged" {
		t.Fatalf("re-adding the same statusline should be unchanged: exit %d, %v", code, facts)
	}

	facts, code = settings(t, "remove", "--file", path, "--url", ourURL)
	if code != 0 || facts["result"] != "removed" {
		t.Fatalf("remove failed: exit %d, %v", code, facts)
	}
	if _, still := readJSON(t, path)["statusLine"]; still {
		t.Error("the statusLine key survived removal")
	}
	if readJSON(t, path)["theme"] != "dark" {
		t.Error("removal took more than its own keys")
	}
}

// TestSettingsStatuslineRefusesToStealAnExisting mirrors
// TestSettingsAddRefusesToStealAnExistingBaseURL for the statusLine key: a statusLine already in
// the file may be the user's own, or another plugin's, and taking it over silently would break
// whatever it drives while reporting success.
func TestSettingsStatuslineRefusesToStealAnExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	theirs := map[string]any{"type": "command", "command": "~/.claude/my-own-statusline.sh"}
	writeJSON(t, path, map[string]any{"statusLine": theirs})

	facts, code := settings(t, "add", "--file", path, "--url", ourURL, "--statusline", "/opt/cg/statusline.py")
	if code != 2 || facts["result"] != "conflict" {
		t.Fatalf("expected a conflict (exit 2), got exit %d, %v", code, facts)
	}
	got := readJSON(t, path)
	if sl, _ := got["statusLine"].(map[string]any); sl["command"] != theirs["command"] {
		t.Fatalf("the file was modified despite the conflict: %v", got["statusLine"])
	}
	// The base_url side must not have been written either — one conflict stops the WHOLE call.
	if env, _ := got["env"].(map[string]any); env != nil {
		t.Errorf("env was written even though the statusline conflict should have stopped everything: %v", env)
	}

	// --force is the explicit decision, same as the base_url side, and it must be recoverable.
	facts, code = settings(t, "add", "--file", path, "--url", ourURL, "--statusline", "/opt/cg/statusline.py", "--force")
	if code != 0 || facts["result"] != "added" {
		t.Fatalf("--force did not proceed: exit %d, %v", code, facts)
	}
	facts, code = settings(t, "remove", "--file", path, "--url", ourURL)
	if code != 0 {
		t.Fatalf("remove failed: %v", facts)
	}
	got = readJSON(t, path)
	if sl, _ := got["statusLine"].(map[string]any); sl["command"] != theirs["command"] {
		t.Errorf("uninstall did not restore the statusLine it replaced: %v", got["statusLine"])
	}
}

// TestSettingsStatuslineOmittedWhenFlagAbsent proves the feature is fully additive: every
// existing call site that never passes --statusline (which is most of them, including every
// pre-existing test in this file) must behave exactly as it did before this key existed.
func TestSettingsStatuslineOmittedWhenFlagAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	writeJSON(t, path, map[string]any{"env": map[string]any{}})
	if _, code := settings(t, "add", "--file", path, "--url", ourURL); code != 0 {
		t.Fatal("add failed")
	}
	if _, ok := readJSON(t, path)["statusLine"]; ok {
		t.Error("a statusLine key appeared without --statusline ever being passed")
	}
}

// TestSettingsStatuslineOnlyInstallTouchesNoRouting covers the /context-guru:statusline skill's
// own call shape: --statusline with NO --url, so a user who wants the status line (typically at
// USER scope, since it renders regardless of which project is open) does not accidentally start
// routing that scope's sessions through the proxy as a side effect.
func TestSettingsStatuslineOnlyInstallTouchesNoRouting(t *testing.T) {
	const cmdPath = "/opt/cg/statusline.py"
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	writeJSON(t, path, map[string]any{"theme": "dark"})

	facts, code := settings(t, "add", "--file", path, "--statusline", cmdPath)
	if code != 0 || facts["result"] != "added" {
		t.Fatalf("statusline-only add failed: exit %d, %v", code, facts)
	}
	got := readJSON(t, path)
	if _, hasEnv := got["env"]; hasEnv {
		t.Errorf("env appeared from a call that never passed --url: %v", got)
	}
	sl, _ := got["statusLine"].(map[string]any)
	if sl["command"] != cmdPath {
		t.Fatalf("statusLine = %v, want command %q", got["statusLine"], cmdPath)
	}

	// Re-running is a no-op.
	if _, code := settings(t, "add", "--file", path, "--statusline", cmdPath); code != 0 {
		t.Fatal("re-add failed")
	}

	// Removing it with no --url must find it anyway — there is no env.ANTHROPIC_BASE_URL to key
	// off, which is exactly the shape the base_url removal path's early return would otherwise
	// mistake for "nothing installed here".
	facts, code = settings(t, "remove", "--file", path)
	if code != 0 || facts["result"] != "removed" {
		t.Fatalf("statusline-only remove failed: exit %d, %v", code, facts)
	}
	if _, still := readJSON(t, path)["statusLine"]; still {
		t.Error("the statusLine key survived a statusline-only removal")
	}
	if readJSON(t, path)["theme"] != "dark" {
		t.Error("removal took more than its own key")
	}
}

// TestSettingsOffLeavesRoutingUntouched: `off` is the one-word undo for a statusline installed
// alongside routing (the shape `install.sh --route` now always leaves behind) — the case `remove`
// cannot serve, since `remove` takes back routing too. Covers the case that actually matters:
// both keys present, `off` must take only its own.
func TestSettingsOffLeavesRoutingUntouched(t *testing.T) {
	const cmdPath = "/opt/cg/statusline.py"
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	writeJSON(t, path, map[string]any{"theme": "dark"})

	if _, code := settings(t, "add", "--file", path, "--url", "http://127.0.0.1:8787/anthropic",
		"--statusline", cmdPath); code != 0 {
		t.Fatal("combined routing+statusline add failed")
	}

	facts, code := settings(t, "off", "--file", path)
	if code != 0 || facts["result"] != "removed" {
		t.Fatalf("off failed: exit %d, %v", code, facts)
	}
	got := readJSON(t, path)
	if _, still := got["statusLine"]; still {
		t.Error("the statusLine key survived `off`")
	}
	env, _ := got["env"].(map[string]any)
	if env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:8787/anthropic" {
		t.Errorf("off touched routing, want it untouched: env = %v", got["env"])
	}
	if got["theme"] != "dark" {
		t.Error("off took more than its own key")
	}

	// Re-running once nothing is installed is a no-op, not an error.
	if facts, code := settings(t, "off", "--file", path); code != 0 || facts["result"] != "unchanged" {
		t.Fatalf("off on an already-off statusline: exit %d, %v", code, facts)
	}

	// A file that has never existed is also a no-op, not an error.
	missing := filepath.Join(dir, "does-not-exist.json")
	if facts, code := settings(t, "off", "--file", missing); code != 0 || facts["result"] != "unchanged" {
		t.Fatalf("off on a missing file: exit %d, %v", code, facts)
	}
}

// TestSettingsAddRequiresUrlOrStatusline: with neither flag, `add` has nothing to do and must
// say so rather than silently writing an empty ANTHROPIC_BASE_URL.
func TestSettingsAddRequiresUrlOrStatusline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	py := requireTool(t, "python3")
	cmd := exec.Command(py, filepath.Join(scriptsDir(t), "settings.py"), "add", "--file", path)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("add with neither --url nor --statusline should fail, got: %s", out)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("a file was created by a call that should have refused before writing anything")
	}
}

// --- the statusLine command --------------------------------------------------------------

// runStatusline runs statusline.py with a controlled environment and stdin, and returns its
// output, exit code and wall-clock time. Trailing args (e.g. "--cache", "--keepalive") are
// passed straight through to the script, exactly as settings.py would install them.
//
// TMPDIR is pinned to a fresh t.TempDir() so the /api/stats response cache the script keeps
// there (keyed by port AND session_id) cannot leak between tests that happen to reuse either.
func runStatusline(t *testing.T, env map[string]string, stdin string, args ...string) (out string, code int, elapsed time.Duration) {
	t.Helper()
	py := requireTool(t, "python3")
	cmd := exec.Command(py, append([]string{filepath.Join(scriptsDir(t), "statusline.py")}, args...)...)
	cmd.Env = append(sandboxEnv(t), "TMPDIR="+t.TempDir())
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdin = strings.NewReader(stdin)
	start := time.Now()
	b, err := cmd.CombinedOutput()
	elapsed = time.Since(start)
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running statusline.py: %v (%s)", err, b)
	}
	t.Logf("statusline.py args=%v env=%v stdin=%q -> exit %d in %v, output %q",
		args, env, stdin, code, elapsed.Round(time.Millisecond), b)
	return string(b), code, elapsed
}

// statsStub is a stand-in /api/stats that always answers the same JSON body, so tests can drive
// statusline.py against a controlled savings/keepalive shape without a real proxy.
func statsStub(t *testing.T, body string) (port string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	return fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
}

// routedEnv is the one env var that makes statusline.py act at all: it self-gates on
// ANTHROPIC_BASE_URL naming our own port, exactly like the two hooks above.
func routedEnv(port string) map[string]string {
	return map[string]string{
		"ANTHROPIC_BASE_URL":        "http://127.0.0.1:" + port + "/anthropic",
		"CLAUDE_PLUGIN_OPTION_PORT": port,
	}
}

// statuslinePayload builds a stdin payload carrying real session totals — the shape confirmed
// against the installed Claude Code CLI binary and a live capture (see report-pr217.md):
// session_id, cost.total_cost_usd, and context_window.total_input_tokens/total_output_tokens.
// This is what makes _default_segment able to compute anything at all; the bare "{}" used by the
// tests above this one is deliberately what a malformed/pre-first-response payload looks like.
func statuslinePayload(sessionID string, totalUSD float64, inputTokens, outputTokens int) string {
	return fmt.Sprintf(`{"session_id":%q,"cost":{"total_cost_usd":%v},`+
		`"context_window":{"total_input_tokens":%d,"total_output_tokens":%d}}`,
		sessionID, totalUSD, inputTokens, outputTokens)
}

// statsStubCapturingQuery is statsStub plus a hook that hands the request's raw query string to
// the caller — used to prove /api/stats is actually called with ?session=<id> rather than
// unscoped, which a passing statsStub test alone cannot show (it ignores the query entirely).
func statsStubCapturingQuery(t *testing.T, body string, gotQuery *string) (port string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		*gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	return fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
}

// TestStatuslineIsSilentWhereRoutingIsNotConfigured is the same property the two hooks have,
// and for the same reason: this plugin installs a statusLine at USER scope (see
// skills/statusline/SKILL.md), so it renders in every project on the machine, including every one
// that never routed through context-guru. Printing anything there — even "not routed" — would be
// permanent noise in somebody's terminal for a plugin they are not using in that project.
func TestStatuslineIsSilentWhereRoutingIsNotConfigured(t *testing.T) {
	for _, c := range []struct {
		name, baseURL string
	}{
		{"unset", ""},
		{"another local proxy on a different port (e.g. litellm)", "http://localhost:4000/anthropic"},
		{"a remote gateway", "https://gateway.corp.example/anthropic"},
		{"our port number appearing in a REMOTE host", "https://8787.example.com/anthropic"},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, code, _ := runStatusline(t, map[string]string{"ANTHROPIC_BASE_URL": c.baseURL}, "{}")
			if code != 0 {
				t.Errorf("exit %d; the status line command must never fail a render", code)
			}
			if strings.TrimSpace(out) != "" {
				t.Errorf("printed %q for a project that is not routed through us", out)
			}
		})
	}
}

// TestStatuslineReportsADownProxyInThreeCharacters covers the ACCEPT-AND-STALL shape, the same
// one stallingPort exists for above: a hung proxy, a half-open socket, or an unrelated service on
// the port all look like this to a client, and none of them are exotic. The render path must
// notice inside its own short timeout rather than hang the status line.
func TestStatuslineReportsADownProxyInThreeCharacters(t *testing.T) {
	port := stallingPort(t)
	out, code, elapsed := runStatusline(t, routedEnv(port), "{}")
	if code != 0 {
		t.Errorf("exit %d; must never fail a render", code)
	}
	if strings.TrimSpace(out) != "cg!" {
		t.Errorf("got %q, want the down indicator %q", strings.TrimSpace(out), "cg!")
	}
	if elapsed > 2*time.Second {
		t.Errorf("took %v against a stalling port; the HTTP fetch carries its own ~0.6s timeout, "+
			"so this should return in well under the script's own 2s backstop", elapsed)
	}
}

// TestStatuslineNeverFailsOnAConnectionRefusal is the cheap, common shape of "down" — nothing
// listening at all — kept distinct from the stalling-port test above because a refused
// connection returns instantly and must not be confused with a slow one in the code path.
func TestStatuslineNeverFailsOnAConnectionRefusal(t *testing.T) {
	port := freePort(t) // freed immediately: nothing is listening
	out, code, _ := runStatusline(t, routedEnv(port), "{}")
	if code != 0 {
		t.Errorf("exit %d; must never fail a render", code)
	}
	if strings.TrimSpace(out) != "cg!" {
		t.Errorf("got %q, want %q", strings.TrimSpace(out), "cg!")
	}
}

// TestStatuslineSurvivesMalformedStdin: Claude Code's own payload shape is trusted, but this
// script must not crash if it is ever handed something else — a truncated pipe, a future
// incompatible change, a manual invocation while testing. Passes --cache so the fallback cache
// segment this asserts on is actually turned on; without it every one of these malformed
// payloads also carries no session totals, so the default segment stays silent too and the
// script would (correctly) print nothing at all — which this test is not the one checking.
func TestStatuslineSurvivesMalformedStdin(t *testing.T) {
	port := statsStub(t, `{"total_saved_usd": 0, "saved_unique": 0}`)
	for _, stdin := range []string{"", "not json{{{", "null", "[1,2,3]", `{"prompt_cache": "not an object"}`} {
		out, code, _ := runStatusline(t, routedEnv(port), stdin, "--cache")
		if code != 0 {
			t.Errorf("stdin %q: exit %d; must never fail a render", stdin, code)
		}
		if !strings.Contains(out, "cache") {
			t.Errorf("stdin %q: expected a graceful fallback cache segment, got %q", stdin, out)
		}
	}
}

// TestStatuslineSurvivesAMalformedStatsResponse: the proxy answered, but not with anything this
// script can parse (a future field-shape change, a body truncated by an intermediary). The cache
// stopper comes from stdin and owes the network nothing, so it must still render once turned on.
func TestStatuslineSurvivesAMalformedStatsResponse(t *testing.T) {
	port := statsStub(t, `not valid json at all`)
	stdin := `{"prompt_cache": {"warm": true, "expires_at": ` +
		fmt.Sprint(time.Now().Add(90*time.Second).Unix()) + `}}`
	out, code, _ := runStatusline(t, routedEnv(port), stdin, "--cache")
	if code != 0 {
		t.Errorf("exit %d; must never fail a render", code)
	}
	if !strings.HasPrefix(strings.TrimSpace(out), "cache 1:") {
		t.Errorf("got %q; the cache stopper must still render off a malformed /api/stats body", out)
	}
	if strings.Contains(out, "saved") {
		t.Errorf("a savings segment appeared from an unparseable stats body: %q", out)
	}
}

// parseCacheSeconds reads back "cache M:SS" as a second count, for boundary assertions that have
// to tolerate the real (small, sub-second) delay between this test computing `expires_at` and the
// subprocess evaluating `time.time()` against it — a fixed-string comparison at a tight margin is
// flaky by construction, not a property of the script.
func parseCacheSeconds(t *testing.T, seg string) int {
	t.Helper()
	m, s := 0, 0
	if _, err := fmt.Sscanf(seg, "cache %d:%d", &m, &s); err != nil {
		t.Fatalf("%q does not parse as a cache countdown: %v", seg, err)
	}
	return m*60 + s
}

// TestStatuslineCacheCountdownMath is the boundary table for the TTL "stopper" — the single most
// valuable element of this whole feature, per its own design brief. Every row is a way the
// countdown could be wrong, including the one a naive `remaining < 0` check gets backwards: a
// prefix that expires AT this exact instant (remaining == 0) has already gone cold, not "still
// has zero seconds left".
func TestStatuslineCacheCountdownMath(t *testing.T) {
	port := statsStub(t, `{}`)
	for _, c := range []struct {
		name         string
		remainingSec float64
		wantCold     bool
	}{
		{"fresh: about 4m50s left", 290, false},
		{"a few seconds left", 3, false},
		{"boundary: expires exactly now", 0, true},
		{"just expired", -5, true},
		{"clock skew: far in the past", -100000, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			expiresAt := time.Now().Add(time.Duration(c.remainingSec * float64(time.Second))).Unix()
			stdin := fmt.Sprintf(`{"prompt_cache": {"warm": true, "expires_at": %d}}`, expiresAt)
			out, code, _ := runStatusline(t, routedEnv(port), stdin, "--cache")
			if code != 0 {
				t.Fatalf("exit %d", code)
			}
			got := strings.SplitN(strings.TrimSpace(out), " | ", 2)[0]
			if c.wantCold {
				if got != "cache cold" {
					t.Errorf("remaining=%.0fs: got %q, want %q", c.remainingSec, got, "cache cold")
				}
				return
			}
			if got == "cache cold" {
				t.Fatalf("remaining=%.0fs: reported cold while still positive", c.remainingSec)
			}
			gotSec := parseCacheSeconds(t, got)
			// Truncation only ever rounds DOWN (never up, never negative here), so the tolerance
			// is asymmetric: at most the subprocess's own wall-clock overhead behind the expected
			// value, never ahead of it.
			if gotSec > int(c.remainingSec) || gotSec < int(c.remainingSec)-2 {
				t.Errorf("remaining=%.0fs: got %q (%ds), want within 2s below that", c.remainingSec, got, gotSec)
			}
		})
	}
}

// TestStatuslineOmitsPromptCacheWhenAbsent covers the normal, expected "no data yet" state —
// before a session's first response has usage to track — as distinct from an error. It must
// read calmly, never as a hang or a missing feature.
// TestStatuslineCacheCountdownExactBoundary isolates remaining==0 EXACTLY, which the table test
// above cannot reliably do: a subprocess always burns some real wall-clock time between this test
// computing `expires_at` and the script evaluating `time.time()` against it, so "expires right
// now" always lands very slightly negative in practice by the time it's checked — which happens
// to satisfy BOTH `remaining <= 0` and the off-by-one `remaining < 0`, so that table row cannot
// catch the boundary bug on its own. This freezes time.time() inside the interpreter instead, so
// remaining is exactly 0.0, not "usually a hair negative".
func TestStatuslineCacheCountdownExactBoundary(t *testing.T) {
	py := requireTool(t, "python3")
	script := `
import importlib.util, sys
spec = importlib.util.spec_from_file_location("statusline", sys.argv[1])
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
m.time.time = lambda: 1_700_000_000.0
print(m._cache_stopper({"prompt_cache": {"expires_at": 1_700_000_000.0}}))        # remaining == 0
print(m._cache_stopper({"prompt_cache": {"expires_at": 1_700_000_000.5}}))        # remaining +0.5s
print(m._cache_stopper({"prompt_cache": {"expires_at": 1_700_000_000.0 - 0.5}}))  # remaining -0.5s
`
	out, err := exec.Command(py, "-c", script, filepath.Join(scriptsDir(t), "statusline.py")).CombinedOutput()
	if err != nil {
		t.Fatalf("running the boundary check: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	want := []string{"cache cold", "cache 0:00", "cache cold"}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), len(want), out)
	}
	for i, w := range want {
		if lines[i] != w {
			t.Errorf("line %d: got %q, want %q (remaining==0 must already read cold, not "+
				"\"0:00\" — a prefix that expires AT this instant has no time left)", i, lines[i], w)
		}
	}
}

func TestStatuslineOmitsPromptCacheWhenAbsent(t *testing.T) {
	port := statsStub(t, `{}`)
	out, code, _ := runStatusline(t, routedEnv(port), `{}`, "--cache")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.TrimSpace(out) != "cache –" {
		t.Errorf("got %q, want the neutral placeholder %q", strings.TrimSpace(out), "cache –")
	}
}

// TestStatuslineOmitsZeroSavings covers a payload with NO session totals at all (the same "{}"
// shape used above) — before the first response of a session, there is nothing to show and
// nothing to divide by. Distinct from TestStatuslineDefaultSegmentZeroTotal below, which drives
// a REAL zero total (cost and tokens both explicitly 0, as Claude Code's own payload reads
// before any usage exists) through the same "nothing to divide by" path.
func TestStatuslineOmitsZeroSavings(t *testing.T) {
	port := statsStub(t, `{"total_saved_usd": 0, "saved_unique": 0, "keepalive_pings": 0}`)
	out, code, _ := runStatusline(t, routedEnv(port), `{}`, "--cache", "--keepalive")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.TrimSpace(out) != "cache –" {
		t.Errorf("got %q, want only the neutral cache placeholder (no session totals to show, no "+
			"keep-alive pings to report)", out)
	}
}

// TestStatuslineDefaultSegmentZeroTotal is the "a brand-new session divides by nothing" case
// called out by name: cost.total_cost_usd and context_window's token pair are all explicitly 0,
// exactly like Claude Code's own real payload before the first response has anything to track
// (confirmed by a live capture — see report-pr217.md). Must render nothing, never "$0.00/0 saved
// of $0.00/0", a crash, or a NaN/Inf from a division this must never even attempt.
func TestStatuslineDefaultSegmentZeroTotal(t *testing.T) {
	port := statsStub(t, `{"total_saved_usd": 0, "saved_unique": 0}`)
	stdin := `{"session_id":"sess-zero","cost":{"total_cost_usd":0},` +
		`"context_window":{"total_input_tokens":0,"total_output_tokens":0}}`
	out, code, _ := runStatusline(t, routedEnv(port), stdin)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("got %q, want nothing — a zero session total is a fresh session, not a fault", out)
	}
	if strings.Contains(strings.ToLower(out), "nan") || strings.Contains(strings.ToLower(out), "inf") {
		t.Fatalf("a zero-total render produced %q — this must never divide by the total at all", out)
	}
}

// TestStatuslineDefaultShowsOnlySavings is the shape the feature is FOR: real, non-trivial
// session totals (matching the live capture) paired with a real savings figure, and — with
// neither --cache nor --keepalive passed — nothing else in the line at all. This is also the
// positive control for TestStatuslineOmitsZeroSavings above: without a working default segment,
// that test's "" assertion (once --cache/--keepalive are added there) would pass for the wrong
// reason — a statusline.py that rendered NO segment ever would look identical.
func TestStatuslineDefaultShowsOnlySavings(t *testing.T) {
	port := statsStub(t, `{"total_saved_usd": 0.03, "saved_unique": 12000, "keepalive_pings": 4}`)
	stdin := statuslinePayload("sess-real", 0.41, 180000, 7000)
	out, code, _ := runStatusline(t, routedEnv(port), stdin)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	got := strings.TrimSpace(out)
	if got != "$0.03/12.0k saved of $0.41/187.0k" {
		t.Errorf("got %q, want the savings-vs-session-total segment exactly", got)
	}
	if strings.Contains(got, "cache") || strings.Contains(got, "ka ") {
		t.Errorf("got %q — an extra segment appeared without its flag", got)
	}
}

// TestStatuslineExtrasHiddenByDefault is the OTHER half of the toggle: the same conditions that
// would make each extra segment render (a real prompt_cache in stdin, real keepalive_pings in
// stats) must produce NEITHER of them without the flag that turns them on — proven alongside
// TestStatuslineExtrasShownWhenEnabled below so that "hidden by default" cannot pass merely
// because the segment is broken.
func TestStatuslineExtrasHiddenByDefault(t *testing.T) {
	port := statsStub(t, `{"total_saved_usd": 0.03, "saved_unique": 12000, "keepalive_pings": 4,
		"keepalive_net_usd": 0.07, "keepalive_misses_avoided": 4}`)
	stdin := `{"session_id":"sess-real","cost":{"total_cost_usd":0.41},` +
		`"context_window":{"total_input_tokens":180000,"total_output_tokens":7000},` +
		`"prompt_cache":{"expires_at":` + fmt.Sprint(time.Now().Add(90*time.Second).Unix()) + `}}`
	out, code, _ := runStatusline(t, routedEnv(port), stdin)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(out, "cache") {
		t.Errorf("got %q — the cache segment showed without --cache", out)
	}
	if strings.Contains(out, "ka ") {
		t.Errorf("got %q — the keep-alive segment showed without --keepalive", out)
	}
}

// TestStatuslineExtrasShownWhenEnabled is the positive control for the test above: the SAME
// conditions, with both flags passed, must show both extras — otherwise "hidden by default"
// would be indistinguishable from "permanently broken".
func TestStatuslineExtrasShownWhenEnabled(t *testing.T) {
	port := statsStub(t, `{"total_saved_usd": 0.03, "saved_unique": 12000, "keepalive_pings": 4,
		"keepalive_net_usd": 0.07, "keepalive_misses_avoided": 4}`)
	stdin := `{"session_id":"sess-real","cost":{"total_cost_usd":0.41},` +
		`"context_window":{"total_input_tokens":180000,"total_output_tokens":7000},` +
		`"prompt_cache":{"expires_at":` + fmt.Sprint(time.Now().Add(90*time.Second).Unix()) + `}}`
	out, code, _ := runStatusline(t, routedEnv(port), stdin, "--cache", "--keepalive")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "cache 1:") {
		t.Errorf("got %q, want a rendered cache countdown with --cache passed", out)
	}
	// The keep-alive segment now shows the NET dollar figure and the misses it prevented, not the
	// raw ping count — see statusline.py's _keepalive_segment.
	if !strings.Contains(out, "ka ≤4miss $0.07") {
		t.Errorf("got %q, want %q with --keepalive passed", out, "ka ≤4miss $0.07")
	}
	if !strings.Contains(out, "$0.03/12.0k saved of $0.41/187.0k") {
		t.Errorf("got %q, want the default segment to keep rendering alongside the extras", out)
	}
}

// TestStatuslineShowsANegativeNetHonestly: total_saved_usd can go genuinely negative — an idle
// keep-alive spending more than it recovers nets the whole figure negative (dash/overview.go's
// own waterfall calls this "a real outcome the dashboard will not hide"). The default segment
// must not fold that into the same omission as a fresh session with nothing to report yet: a
// nonzero SESSION TOTAL with a negative saving is a real fact, not "nothing happened".
func TestStatuslineShowsANegativeNetHonestly(t *testing.T) {
	port := statsStub(t, `{"total_saved_usd": -0.05, "saved_unique": 0}`)
	stdin := statuslinePayload("sess-neg", 1.00, 40000, 1000)
	out, code, _ := runStatusline(t, routedEnv(port), stdin)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "$-0.05/0 saved of $1.00/41.0k") {
		t.Errorf("got %q, want a segment showing the negative net, not an omission", out)
	}
}

// TestStatuslineScopesStatsToItsOwnSession proves /api/stats is actually called with
// ?session=<this session's id> — the fix for mixing a process-wide savings figure into a
// segment that claims to be THIS session's own. A statsStub alone (used by every test above)
// cannot show this: it ignores the query string entirely, so a regression to the old unscoped
// URL would still pass every one of them.
func TestStatuslineScopesStatsToItsOwnSession(t *testing.T) {
	var gotQuery string
	port := statsStubCapturingQuery(t, `{"total_saved_usd": 0.01, "saved_unique": 1}`, &gotQuery)
	stdin := statuslinePayload("abc-123-session", 0.10, 1000, 100)
	_, code, _ := runStatusline(t, routedEnv(port), stdin)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if gotQuery != "session=abc-123-session" {
		t.Errorf("got query %q, want %q", gotQuery, "session=abc-123-session")
	}
}

// TestStatuslineDefaultSegmentRequiresStats: a real, nonzero session total with NO savings
// figure to pair it with (the proxy answered, but /api/stats has nothing — e.g. --dashboard is
// off) must render nothing, not a session total with a fabricated or missing "saved" half.
func TestStatuslineDefaultSegmentRequiresStats(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "dashboard disabled", http.StatusNotFound)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	port := fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)

	stdin := statuslinePayload("sess-nodash", 0.41, 180000, 7000)
	out, code, _ := runStatusline(t, routedEnv(port), stdin)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("got %q, want nothing — a real session total with no savings figure to pair "+
			"it with must not render half a claim", out)
	}
}

// TestStatuslineRejectsAMalformedSessionId: session_id is a string this script did not generate,
// carried straight from stdin into a URL query and a tempfile path. One that does not look like
// the UUID Claude Code actually sends must be treated as absent rather than passed through
// verbatim — proven here by a session_id containing characters that would otherwise inject a
// second query parameter.
func TestStatuslineRejectsAMalformedSessionId(t *testing.T) {
	var gotQuery string
	port := statsStubCapturingQuery(t, `{"total_saved_usd": 0.01, "saved_unique": 1}`, &gotQuery)
	stdin := statuslinePayload("legit-id&session=someone-elses-session", 0.10, 1000, 100)
	_, code, _ := runStatusline(t, routedEnv(port), stdin)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(gotQuery, "someone-elses-session") {
		t.Fatalf("got query %q — an unsanitised session_id reached the request", gotQuery)
	}
	if gotQuery != "" {
		t.Errorf("got query %q, want an unscoped request (empty query) for a session_id this "+
			"script does not trust", gotQuery)
	}
}

// TestStatuslineCachesPerSession is TestStatuslineCachesStatsAcrossQuickRenders' sibling for the
// defect its own fix could reintroduce: caching /api/stats by PORT ALONE would let one terminal
// read back another terminal's session's cached savings figure whenever both share a proxy (a
// single local proxy commonly serves every project on a machine). Two session ids, same port,
// same TMPDIR, a stub whose body depends on which session was requested — each render must get
// its OWN session's figure, never the other's.
func TestStatuslineCachesPerSession(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.RawQuery {
		case "session=sess-a":
			w.Write([]byte(`{"total_saved_usd": 0.01, "saved_unique": 100}`))
		case "session=sess-b":
			w.Write([]byte(`{"total_saved_usd": 9.99, "saved_unique": 9000}`))
		default:
			t.Errorf("unexpected query %q", r.URL.RawQuery)
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	port := fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)

	py := requireTool(t, "python3")
	tmp := t.TempDir() // shared TMPDIR on purpose: this is what could let the cache files collide
	run := func(sessionID string) string {
		cmd := exec.Command(py, filepath.Join(scriptsDir(t), "statusline.py"))
		cmd.Env = append(sandboxEnv(t), "TMPDIR="+tmp,
			"ANTHROPIC_BASE_URL=http://127.0.0.1:"+port+"/anthropic",
			"CLAUDE_PLUGIN_OPTION_PORT="+port)
		cmd.Stdin = strings.NewReader(statuslinePayload(sessionID, 1.00, 10000, 1000))
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("statusline.py: %v (%s)", err, b)
		}
		return string(b)
	}
	a := run("sess-a")
	b := run("sess-b")
	if !strings.Contains(a, "$0.01/100 saved") {
		t.Errorf("session a: got %q, want its own $0.01/100 saved", a)
	}
	if !strings.Contains(b, "$9.99/9.0k saved") {
		t.Errorf("session b: got %q, want its own $9.99/9.0k saved — not session a's cached figure", b)
	}
}

// TestStatuslineCachesStatsAcrossQuickRenders is the "cheap" requirement made concrete: Claude
// Code can re-render on every token of a streamed response, and without a cache this would be
// one HTTP request per render against the user's own local proxy.
func TestStatuslineCachesStatsAcrossQuickRenders(t *testing.T) {
	var hits int64
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"total_saved_usd": 0.01, "saved_unique": 1}`))
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	port := fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)

	// Same TMPDIR for every call in this test, deliberately — that is what makes the on-disk
	// cache file shared between them. runStatusline gives each call a FRESH TMPDIR (correct
	// isolation between different tests), so this test drives the script directly instead.
	py := requireTool(t, "python3")
	tmp := t.TempDir()
	run := func() string {
		cmd := exec.Command(py, filepath.Join(scriptsDir(t), "statusline.py"))
		cmd.Env = append(sandboxEnv(t), "TMPDIR="+tmp,
			"ANTHROPIC_BASE_URL=http://127.0.0.1:"+port+"/anthropic",
			"CLAUDE_PLUGIN_OPTION_PORT="+port)
		cmd.Stdin = strings.NewReader("{}")
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("statusline.py: %v (%s)", err, b)
		}
		return string(b)
	}
	for i := 0; i < 5; i++ {
		run()
	}
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Errorf("5 renders inside the cache TTL produced %d requests to /api/stats, want 1", got)
	}
}

// --- the keep-alive opt-in toggle -----------------------------------------------------------

// keepalivePort is deliberately not 8787. `settings.py strategy` names the config file after the
// port, so a regression to a defaulted port writes a filename nothing reads — and at a fixture port
// of 8787 that regression passes by coincidence, which is exactly how it shipped once.
const keepalivePort = "4041"

// The keep-alive toggle used to be a heredoc embedded in skills/keepalive/SKILL.md, and the tests
// below used to EXTRACT and execute that heredoc. They no longer do, because the heredoc is gone:
// `settings.py strategy` is now the one place that decides what a strategy name means, and the skill
// carries a name rather than four tuning numbers. These tests therefore assert behaviour instead of
// prose, which is a strict improvement — a skill can be reworded without breaking them, and the
// invariants they defend (ownership, the stated preset, no file for `split`) hold for every caller
// rather than for one code path in one markdown file.
//
// Kept from the old suite, deliberately, one property at a time:
//   - a config that omits its own `preset:` line silently turns compaction off, because --config
//     REPLACES --preset rather than layering over it;
//   - an unresolved preset must be REFUSED rather than written empty, which is the same defect
//     arriving by a different route;
//   - a file we did not write is never modified or removed;
//   - the port is never defaulted, because the file is NAMED after it.

// strategyCfg is where `settings.py strategy` writes, for the fixture port.
func strategyCfg(state string) string {
	return filepath.Join(state, "keepalive-"+keepalivePort+".yaml")
}

// TestStrategyListIsTheAuthoritativeSetAndNamesItsDefault. The names exist so that switching back is
// one word; a list that omitted one, or disagreed about the default, would send a model to invent a
// name and get `unknown_strategy`.
func TestStrategyListIsTheAuthoritativeSetAndNamesItsDefault(t *testing.T) {
	state, home := t.TempDir(), t.TempDir()
	facts, code := settingsIn(t, state, home, "strategy", "list")
	if code != 0 {
		t.Fatalf("exit %d: %v", code, facts)
	}
	if facts["default"] != "5-min-ping" {
		t.Errorf("default strategy is %q, want 5-min-ping: this is the value the install writes, and "+
			"plugin.json's cache_strategy default has to agree with it", facts["default"])
	}
	for _, k := range []string{"strategy_none", "strategy_5_min_ping", "strategy_1_hour_head"} {
		if facts[k] == "" {
			t.Errorf("`strategy list` does not describe %s; a name the picker offers but list omits "+
				"cannot be discovered", k)
		}
	}
	// The honesty that matters most: exactly one strategy spends the caller's money, and it must be
	// the one that says so. A `spends=false` on 5-min-ping would make the install's one-line
	// disclosure wrong.
	if facts["spends_5_min_ping"] != "true" {
		t.Errorf("5-min-ping reports spends=%q; it pings on idle turns with the caller's own "+
			"credential, and the install's disclosure is generated from this",
			facts["spends_5_min_ping"])
	}
	for _, k := range []string{"spends_none", "spends_1_hour_head"} {
		if facts[k] != "false" {
			t.Errorf("%s reports spends=%q, want false: neither sends a ping, and claiming they "+
				"spend would push users off the free strategies for no reason", k, facts[k])
		}
	}
}

// TestStrategySetWritesTheStatedPresetAndRecordsTheName is the old
// TestKeepaliveEnableWritesAPresetPreservingConfig, moved to the script. --config REPLACES --preset
// entirely (loadConfig in cmd/context-guru-proxy/main.go reads --preset only when --config is
// ABSENT), so a strategy file that omitted its own `preset:` line would silently turn compaction off
// at the moment a cache strategy was armed — the opposite of the intent.
func TestStrategySetWritesTheStatedPresetAndRecordsTheName(t *testing.T) {
	state, home := t.TempDir(), t.TempDir()
	facts, code := settingsIn(t, state, home, "strategy", "set",
		"--name", "5-min-ping", "--port", keepalivePort, "--preset", "codesmart")
	if code != 0 {
		t.Fatalf("exit %d: %v", code, facts)
	}
	if facts["strategy"] != "5-min-ping" || facts["spends"] != "true" {
		t.Errorf("set reported strategy=%q spends=%q; the caller reports both to the user",
			facts["strategy"], facts["spends"])
	}
	b, err := os.ReadFile(strategyCfg(state))
	if err != nil {
		t.Fatalf("no config written at %s: %v", strategyCfg(state), err)
	}
	body := string(b)
	if !strings.Contains(body, "preset: codesmart") {
		t.Errorf("the CONFIGURED preset did not survive into the file, which silently drops "+
			"compaction the moment --config is passed:\n%s", body)
	}
	if !strings.Contains(body, "keepalive: true") {
		t.Errorf("cache.keepalive: true is missing, so the strategy would not actually ping:\n%s", body)
	}
	// The name has to be IN the file, not merely in the command that wrote it: `strategy show` and
	// start-proxy.sh both read it back, and the whole point of a name is that it survives.
	if !strings.Contains(body, "strategy=5-min-ping") {
		t.Errorf("the strategy name was not recorded in the file, so nothing can report it back and "+
			"the user cannot be told what to switch back to:\n%s", body)
	}
	// Written 0600 like every other file this script owns: it is not secret, but it is ours, and a
	// world-writable config that decides whether money is spent is a bad shape.
	if fi, serr := os.Stat(strategyCfg(state)); serr == nil && fi.Mode().Perm() != 0o600 {
		t.Errorf("config mode is %v, want 0600", fi.Mode().Perm())
	}
}

// TestStrategyNoneIsTheAbsenceOfAConfig pins the one non-obvious choice in the table: `none` is
// expressed by REMOVING the file, never by writing `keepalive: false`. Because --config replaces the
// preset, a file that said "off" would still hijack preset resolution — so the only faithful way to
// say "just the preset" is to leave no config at all.
func TestStrategySplitIsTheAbsenceOfAConfig(t *testing.T) {
	state, home := t.TempDir(), t.TempDir()
	if _, code := settingsIn(t, state, home, "strategy", "set",
		"--name", "5-min-ping", "--port", keepalivePort, "--preset", "cache"); code != 0 {
		t.Fatal("could not arm the strategy this test then switches away from")
	}
	facts, code := settingsIn(t, state, home, "strategy", "set",
		"--name", "none", "--port", keepalivePort, "--preset", "cache")
	if code != 0 {
		t.Fatalf("exit %d: %v", code, facts)
	}
	if _, err := os.Stat(strategyCfg(state)); err == nil {
		b, _ := os.ReadFile(strategyCfg(state))
		t.Errorf("switching to `none` left a config behind, so --config is still passed and the "+
			"preset is still overridden:\n%s", b)
	}
	if facts["strategy"] != "none" {
		t.Errorf("reported strategy=%q after switching to none", facts["strategy"])
	}
	// And `show` must call that state `none` rather than "unknown" or silence: it is a real,
	// selectable strategy, and the install leaves exactly this behind for --cache-strategy none.
	shown, _ := settingsIn(t, state, home, "strategy", "show", "--port", keepalivePort)
	if shown["strategy"] != "none" {
		t.Errorf("with no config, show reports strategy=%q; want none", shown["strategy"])
	}
}

// TestStrategyNeverTouchesAFileItDidNotWrite is the old
// TestKeepaliveEnableRefusesToClobberAForeignFile and TestKeepaliveDisableOnlyRemovesOurOwnFile,
// merged: the same ownership discipline settings.py enforces for everything else it writes, applied
// to both directions. A config at our path may be somebody's hand-tuned file.
func TestStrategyNeverTouchesAFileItDidNotWrite(t *testing.T) {
	const foreign = "# hand-written, not ours\npreset: house\ncache:\n  keepalive: true\n"
	for _, op := range []string{"set", "clear"} {
		t.Run(op, func(t *testing.T) {
			state, home := t.TempDir(), t.TempDir()
			if err := os.MkdirAll(state, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(strategyCfg(state), []byte(foreign), 0o644); err != nil {
				t.Fatal(err)
			}
			args := []string{"strategy", op, "--port", keepalivePort}
			if op == "set" {
				args = append(args, "--name", "5-min-ping", "--preset", "cache")
			}
			facts, code := settingsIn(t, state, home, args...)
			if code != 2 {
				t.Errorf("exit %d, want 2 (a refusal the caller can distinguish from a failure): %v",
					code, facts)
			}
			if facts["reason"] != "not_ours" {
				t.Errorf("reason=%q, want not_ours", facts["reason"])
			}
			got, err := os.ReadFile(strategyCfg(state))
			if err != nil || string(got) != foreign {
				t.Errorf("a config this plugin never wrote was modified or removed: %v, %q", err, got)
			}
		})
	}
	t.Run("show reports it as foreign rather than guessing", func(t *testing.T) {
		state, home := t.TempDir(), t.TempDir()
		if err := os.MkdirAll(state, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(strategyCfg(state), []byte(foreign), 0o644); err != nil {
			t.Fatal(err)
		}
		facts, code := settingsIn(t, state, home, "strategy", "show", "--port", keepalivePort)
		if code != 0 || facts["strategy"] != "(foreign)" {
			t.Errorf("show on a foreign config: exit %d strategy=%q, want 0 and (foreign) — naming a "+
				"strategy here would attribute somebody else's tuning to us", code, facts["strategy"])
		}
	})
}

// TestStrategySetRefusesAnEmptyPreset is the old TestKeepaliveEnableRefusesAnEmptyPreset, and the
// hole it closes is unchanged: `settings.py config` prints `option_preset=` only when that key is
// actually configured, so a user who set the port and never touched the preset leaves the caller with
// nothing to substitute. Writing `preset:` empty is not an error anywhere downstream — applyPreset
// returns early on `Preset == ""`, so Load reports success, no pipeline is filled, and compaction is
// entirely OFF while the strategy keeps spending the caller's credential on idle pings.
//
// That is strictly worse than a defaulted `cache` would be, since `cache` at least ran the split. So
// the refusal must leave no file behind at all.
func TestStrategySetRefusesAnEmptyPreset(t *testing.T) {
	state, home := t.TempDir(), t.TempDir()
	facts, code := settingsIn(t, state, home, "strategy", "set",
		"--name", "5-min-ping", "--port", keepalivePort, "--preset", "")
	if code == 0 {
		t.Errorf("an unresolved preset was accepted; that writes `preset:` empty, which turns "+
			"compaction off while the strategy keeps paying for pings: %v", facts)
	}
	if facts["reason"] != "empty_preset" {
		t.Errorf("reason=%q, want empty_preset", facts["reason"])
	}
	if _, err := os.Stat(strategyCfg(state)); err == nil {
		t.Errorf("a config was written anyway at %s, so the proxy would start with compaction off",
			strategyCfg(state))
	}
}

// TestStrategySetRefusesAnUnknownName. A name is the interface, so an invented one must fail loudly
// rather than write a file whose contents nobody chose.
func TestStrategySetRefusesAnUnknownName(t *testing.T) {
	state, home := t.TempDir(), t.TempDir()
	facts, code := settingsIn(t, state, home, "strategy", "set",
		"--name", "turbo", "--port", keepalivePort, "--preset", "cache")
	if code != 2 || facts["reason"] != "unknown_strategy" {
		t.Errorf("exit %d reason=%q, want 2 / unknown_strategy: %v", code, facts["reason"], facts)
	}
	if facts["known"] == "" {
		t.Error("the refusal does not list the names that WOULD work, so the caller has to guess again")
	}
	if _, err := os.Stat(strategyCfg(state)); err == nil {
		t.Error("a config was written for a name that does not exist")
	}
}

// TestStrategyRequiresThePortRatherThanDefaultingIt is the defect that shipped once already, in the
// form `${CLAUDE_PLUGIN_OPTION_PORT:-8787}`: the config file is NAMED after the port, so a defaulted
// 8787 writes a file nothing ever reads and then reports success. The fixture port is deliberately
// not 8787, because at 8787 that regression passes by coincidence.
func TestStrategyRequiresThePortRatherThanDefaultingIt(t *testing.T) {
	state, home := t.TempDir(), t.TempDir()
	for _, op := range [][]string{
		{"strategy", "set", "--name", "5-min-ping", "--preset", "cache"},
		{"strategy", "show"},
		{"strategy", "clear"},
	} {
		facts, code := settingsIn(t, state, home, op...)
		if code == 0 {
			t.Errorf("%v was accepted without --port; a defaulted port writes or reads a file for the "+
				"wrong proxy: %v", op, facts)
		}
	}
	if _, err := os.Stat(filepath.Join(state, "keepalive-8787.yaml")); err == nil {
		t.Error("a config was written at the DEFAULT port, which is exactly the shipped defect")
	}
}

// TestStrategyStillRecognisesTheOldKeepaliveMarker. Files written by the previous
// /context-guru:keepalive skill exist on real machines. Treating them as foreign would strand anyone
// who armed keep-alive before the rename: they could neither re-point nor clear their own config.
func TestStrategyStillRecognisesTheOldKeepaliveMarker(t *testing.T) {
	const old = "# context-guru: written by /context-guru:keepalive\npreset: cache\ncache:\n  keepalive: true\n"
	state, home := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(strategyCfg(state), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	shown, _ := settingsIn(t, state, home, "strategy", "show", "--port", keepalivePort)
	if shown["strategy"] != "(unnamed)" {
		t.Errorf("show on a pre-rename config reports strategy=%q; want (unnamed) — it is ours, but "+
			"it carries no name, and inventing one would misreport what is armed", shown["strategy"])
	}
	facts, code := settingsIn(t, state, home, "strategy", "set",
		"--name", "5-min-ping", "--port", keepalivePort, "--preset", "cache")
	if code != 0 {
		t.Fatalf("a pre-rename config could not be re-set, which strands its owner: exit %d %v",
			code, facts)
	}
	b, _ := os.ReadFile(strategyCfg(state))
	if !strings.Contains(string(b), "strategy=5-min-ping") {
		t.Errorf("re-setting a pre-rename config did not give it a name:\n%s", b)
	}
}

// TestPluginJSONCacheStrategyAgreesWithTheScript. Two sources of truth for the default would drift,
// and the drift is invisible: plugin.json is what the user's settings UI shows, and the script is
// what actually gets written.
func TestPluginJSONCacheStrategyAgreesWithTheScript(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(".claude-plugin", "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		UserConfig map[string]struct {
			Default     any    `json:"default"`
			Description string `json:"description"`
		} `json:"userConfig"`
	}
	if err := json.Unmarshal(b, &manifest); err != nil {
		t.Fatalf("plugin.json does not parse: %v", err)
	}
	cs, ok := manifest.UserConfig["cache_strategy"]
	if !ok {
		t.Fatal("plugin.json declares no cache_strategy option, so the strategy cannot be set at " +
			"install time and the picker is the only way in")
	}
	state, home := t.TempDir(), t.TempDir()
	facts, _ := settingsIn(t, state, home, "strategy", "list")
	if got, want := cs.Default, facts["default"]; got != want {
		t.Errorf("plugin.json cache_strategy default is %v but the script's default is %q; the UI "+
			"would advertise one strategy while the install wrote another", got, want)
	}
	// The default spends money, so the option that selects it has to say so where the user reads it.
	if !strings.Contains(strings.ToUpper(cs.Description), "SPENDS") {
		t.Errorf("the cache_strategy description never says the default SPENDS the user's own "+
			"credential; that disclosure cannot live only in a skill the user never reads:\n%s",
			cs.Description)
	}

	// And the preset option must no longer make the claim that default-on keep-alive falsifies.
	// "no model calls" was true when `cache` meant cachesplit alone; an idle ping is a model call.
	preset, ok := manifest.UserConfig["preset"]
	if !ok {
		t.Fatal("plugin.json declares no preset option")
	}
	if strings.Contains(preset.Description, "no model calls") &&
		!strings.Contains(preset.Description, "cache_strategy") {
		t.Errorf("the preset description still promises \"no model calls\" without pointing at "+
			"cache_strategy, whose default pings. A trust claim that has quietly become false is "+
			"worse than no claim:\n%s", preset.Description)
	}
}

// TestPickerSkillOffersOnlyRealStrategies is a docdrift guard: the picker's table is what a model
// reads before choosing a --name, so a name that exists only in prose becomes an
// `unknown_strategy` at the worst moment, and a strategy missing from the table is undiscoverable.
func TestPickerSkillOffersOnlyRealStrategies(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("skills", "cache-strategy-picker", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	state, home := t.TempDir(), t.TempDir()
	facts, _ := settingsIn(t, state, home, "strategy", "list")
	real := map[string]bool{}
	for k := range facts {
		if name, ok := strings.CutPrefix(k, "strategy_"); ok {
			real[name] = true
		}
	}
	if len(real) == 0 {
		t.Fatal("no strategies were discovered, so this guard proved nothing")
	}
	for name := range real {
		// The skill spells names with hyphens; `strategy list` keys use underscores.
		spelled := strings.ReplaceAll(name, "_", "-")
		if !strings.Contains(body, "`"+spelled+"`") {
			t.Errorf("the picker skill never mentions the strategy %q, so a user cannot discover it",
				spelled)
		}
	}
	// Every backticked hyphenated token that LOOKS like a strategy must be one.
	for _, m := range regexp.MustCompile("`([a-z0-9]+(?:-[a-z0-9]+){1,3})`").FindAllStringSubmatch(body, -1) {
		tok := m[1]
		if strings.Contains(tok, ".") || strings.HasSuffix(tok, ".py") {
			continue
		}
		if !real[strings.ReplaceAll(tok, "-", "_")] && isStrategyShaped(tok) {
			t.Errorf("the picker skill offers %q, which `strategy list` does not know: a model that "+
				"passes it gets reason=unknown_strategy", tok)
		}
	}
}

// isStrategyShaped keeps the guard above from flagging ordinary hyphenated prose. Only tokens that
// look like a strategy NAME are candidates: they end in a word this vocabulary actually uses.
func isStrategyShaped(tok string) bool {
	for _, suffix := range []string{"-ping", "-head", "-split"} {
		if strings.HasSuffix(tok, suffix) {
			return true
		}
	}
	// `split` stays in this vocabulary although it was REMOVED, and that is the point: the guard's
	// job is to notice a strategy-shaped token in prose, and a document still saying `split` names
	// something that no longer exists — exactly what it should catch.
	return tok == "split" || tok == "none"
}

// TestStartProxyPicksUpAKeepaliveConfig proves the wiring between the opt-in toggle above and the
// hook that actually launches the proxy: presence of the file is what decides, and it is the ONLY
// thing that decides — nothing else about this test's environment names keep-alive at all.
func TestStartProxyPicksUpAKeepaliveConfig(t *testing.T) {
	for _, c := range []struct {
		name       string
		writeCfg   bool
		wantConfig bool
	}{
		{"no keepalive config: --config is never passed", false, false},
		{"a keepalive config exists: --config names it", true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			requireTool(t, "bash")
			dir := t.TempDir()
			argvFile := filepath.Join(dir, "argv")
			fake := filepath.Join(dir, "fake-proxy")
			port := freePort(t)
			py := requireTool(t, "python3")
			script := "#!/usr/bin/env bash\n" +
				"printf '%s\\n' \"$*\" > " + argvFile + "\n" +
				"exec " + py + " -c '\n" +
				"import http.server\n" +
				"class H(http.server.BaseHTTPRequestHandler):\n" +
				"    def do_GET(self):\n" +
				"        self.send_response(200); self.end_headers(); self.wfile.write(b\"ok\")\n" +
				"    def log_message(self, *a): pass\n" +
				"http.server.HTTPServer((\"127.0.0.1\", " + port + "), H).serve_forever()\n'\n"
			if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}

			state := filepath.Join(dir, "state")
			var cfgPath string
			if c.writeCfg {
				stateDir := filepath.Join(state, "context-guru")
				if err := os.MkdirAll(stateDir, 0o755); err != nil {
					t.Fatal(err)
				}
				cfgPath = filepath.Join(stateDir, "keepalive-"+port+".yaml")
				if err := os.WriteFile(cfgPath, []byte("preset: cache\ncache:\n  keepalive: true\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "start-proxy.sh"))
			cmd.Env = append(sandboxEnv(t),
				"CLAUDE_PLUGIN_OPTION_PORT="+port,
				"ANTHROPIC_BASE_URL=http://127.0.0.1:"+port+"/anthropic",
				"CONTEXT_GURU_BIN="+fake,
				"XDG_STATE_HOME="+state,
				"TMPDIR="+dir)
			out, err := cmd.CombinedOutput()
			t.Cleanup(func() {
				if b, e := os.ReadFile(filepath.Join(state, "context-guru", "proxy-"+port+".pid")); e == nil {
					exec.Command("kill", strings.TrimSpace(string(b))).Run() //nolint:errcheck
				}
			})
			if err != nil {
				t.Fatalf("start-proxy.sh failed: %v\n%s", err, out)
			}
			argv, rerr := os.ReadFile(argvFile)
			if rerr != nil {
				t.Fatalf("the fake proxy never ran: %v", rerr)
			}
			gotConfig := strings.Contains(string(argv), "--config "+cfgPath) ||
				(c.writeCfg && strings.Contains(string(argv), "--config"))
			hasAnyConfig := strings.Contains(string(argv), "--config")
			if c.wantConfig && !gotConfig {
				t.Errorf("wanted --config %s in argv, got %q", cfgPath, argv)
			}
			if !c.wantConfig && hasAnyConfig {
				t.Errorf("no keepalive config was written, but --config appeared anyway: %q", argv)
			}
		})
	}
}

// TestEverySkillStatesThePerOptionFallback guards the wording, because the wording is the whole fix.
//
// `settings.py config` prints an `option_<name>=` line only for keys the user actually set, and reports
// `source=(none)` only when the options object is missing or empty. All four skills that call it used to
// document the fallback as keyed on `source=(none)` — so a PARTIAL config (a real `source=`, one option
// absent) fell through every documented branch and left the model inventing a value at the exact point
// those sections warn against it. For `uninstall` that builds a URL matching nothing, so the removal
// silently finds nothing to remove.
//
// This asserts prose, which is unusual here and deliberate: the skills ARE the program on this path, and
// the defect was a sentence rather than a statement. Kept to one required phrase rather than exact text,
// so rewording stays cheap and deleting the rule does not.
func TestEverySkillStatesThePerOptionFallback(t *testing.T) {
	entries, err := os.ReadDir("skills")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join("skills", e.Name(), "SKILL.md"))
		if err != nil {
			t.Errorf("reading %s: %v", e.Name(), err)
			continue
		}
		body := string(b)
		if !strings.Contains(body, `settings.py" config`) {
			continue // this skill does not read the options, so it has no fallback to state
		}
		checked++
		// The positive alone is the reassurance I suspected it of being: review demonstrated that
		// restoring the old sentence while KEEPING the new rule passes. So assert the absence of the old
		// instruction shape too.
		//
		// The needle is the CONDITION, "if it reports `source=(none)`", not "or 8787 if it reports". The
		// narrower form was coupled to one phrasing and this repo had two: keepalive's own sentence said
		// "or 8787 and `cache` if it reports …", which slipped straight through — the same failure as
		// keying the fence scan on "```bash". Verified against both real phrasings (origin/main's status
		// and e0ea8a9's keepalive: 1 each) and all five current skills (0).
		//
		// It cannot be `source=(none)` alone: the corrected prose says "reports `source=(none)` only
		// when …", which is an explanation rather than a condition, so the token appears in every FIXED
		// file. "if it reports" is what makes it an instruction to act on.
		if strings.Contains(body, "if it reports `source=(none)`") {
			t.Errorf("skills/%s/SKILL.md still tells the model to fall back \"if it reports "+
				"`source=(none)`\", which is the condition this per-option rule replaces. A skill can "+
				"state the rule and contradict it two lines later; that is what this catches.", e.Name())
		}
		if !strings.Contains(body, "is unconfigured") {
			t.Errorf("skills/%s/SKILL.md reads `settings.py config` but never says that an option the "+
				"output does not list is UNCONFIGURED. Without that, a partial config (a real `source=` "+
				"with one option missing) matches no documented branch and the model invents a value.",
				e.Name())
		}
	}
	// Three, not four, since install/SKILL.md stopped reading the options directly: the orchestrator
	// (`install.sh --route`) resolves them, with the same per-option fallback enforced in shell and
	// covered by TestRoutePlanResolvesTheConfiguredPortPerOption. A skill that delegates cannot state
	// a fallback it no longer performs, so requiring it to would force prose back into a file whose
	// whole point is that it no longer carries the mechanism. The remaining three (status, uninstall,
	// cache-strategy-picker) still substitute the values themselves and still need the rule.
	if checked < 3 {
		t.Fatalf("only %d skills were found to read `settings.py config`; expected at least the three "+
			"(status, uninstall, cache-strategy-picker), so this guard proved less than it claims",
			checked)
	}
	t.Logf("checked %d skills that read the configured options", checked)
}

// fenceOpener matches a markdown fence and captures its info string. Leading whitespace is allowed
// because an indented fence is still a fence — a block indented as a list continuation is executed
// exactly like one at column zero.
var fenceOpener = regexp.MustCompile("^[ \t]*```+[ \t]*([A-Za-z0-9_.+-]*)")

// isShellFence reports whether a fence's info string means "this is shell the model will run". The
// label is not a semantic boundary: ```sh and a bare ``` are executed the same as ```bash, so keying
// a guard on the literal "```bash" leaves the same defect reachable one keystroke away.
func isShellFence(info string) bool {
	switch strings.ToLower(info) {
	case "", "bash", "sh", "shell", "zsh", "console", "shell-session":
		return true
	}
	return false
}

// TestNoSkillBlockReadsAPluginOption is a guard, not a discovery: it is the same defect as
// TestTheConfiguredPortCanActuallyBeHonoured, which was found once in install/status/uninstall, fixed
// there, and then reintroduced wholesale by a later skill that was written from the older pattern.
//
// CLAUDE_PLUGIN_OPTION_* reaches HOOK environments only, never a Bash tool call, so any
// `${CLAUDE_PLUGIN_OPTION_X:-default}` inside a skill's fenced bash block is a silent wrong answer —
// it always yields the default, whatever the user configured, and reports success while doing it.
// Skills must obtain these values from `settings.py config` and substitute them.
//
// Scoped to fenced SHELL blocks in skills/, on purpose: the hook SCRIPTS (start-proxy.sh,
// check-proxy.sh) do run in a hook environment and read these variables legitimately, and the skills'
// PROSE has to be able to name the variable in order to warn about it.
//
// "Shell block" deliberately includes ```sh, a bare ```, and an indented fence, not just ```bash at
// column zero. Round-1 review demonstrated both escapes: the same defect re-added under a ```sh fence,
// and under a two-space-indented ```bash fence, each left this guard silent. A guard for a CLASS has
// to match how the model reads a block, and the model does not care what the info string says.
func TestNoSkillBlockReadsAPluginOption(t *testing.T) {
	entries, err := os.ReadDir("skills")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join("skills", e.Name(), "SKILL.md"))
		if err != nil {
			t.Errorf("reading %s: %v", e.Name(), err)
			continue
		}
		// Two pieces of state, not one. Tracking only "am I in a block I care about" gets the
		// PARITY wrong: a ```json fence would not open anything, so its CLOSING fence reads as an
		// opener and every prose line after it looks like shell. That is not hypothetical — it
		// misfired on install/SKILL.md's prose the first time this guard was widened.
		inFence, isShell := false, false
		for i, line := range strings.Split(string(b), "\n") {
			if m := fenceOpener.FindStringSubmatch(line); m != nil {
				if inFence {
					inFence, isShell = false, false
				} else {
					inFence, isShell = true, isShellFence(m[1])
				}
				continue
			}
			if inFence && isShell && strings.Contains(line, "CLAUDE_PLUGIN_OPTION_") {
				t.Errorf("skills/%s/SKILL.md:%d reads a plugin option inside an executable block, "+
					"which always expands to the default in a Bash tool call:\n\t%s\n"+
					"Obtain it from `settings.py config` and substitute a <placeholder> instead.",
					e.Name(), i+1, strings.TrimSpace(line))
			}
		}
		// An odd number of fence lines ends the loop still inside one, which means the scan lost its
		// bearings partway through and everything after that point went unexamined. That is a false
		// negative over the whole remainder rather than a misfire: round-2 review deleted one closing
		// fence, re-added the defect to all four blocks, and this guard reported 0 of 4. A missing
		// backtick line is an ordinary editing slip, so detect it — same argument as `checked == 0`
		// below, and it costs nothing while every skill stays balanced.
		if inFence {
			t.Errorf("skills/%s/SKILL.md: fences are unbalanced, so the scan lost its bearings partway "+
				"through and this guard proved nothing for the rest of the file", e.Name())
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no skills were scanned, so this guard proved nothing")
	}
	t.Logf("scanned %d skills", checked)
}

// hasArgvFlag reports whether argv contains flag as a WHOLE token, and flagValue returns the token
// after it. Substring matching is not good enough here and the difference is not academic:
// `--dashboard` is a PREFIX of `--dashboard-db`, so `strings.Contains(argv, "--dashboard")` is
// satisfied by the flag that follows it. Review deleted `--dashboard` from start-proxy.sh's launch
// line and the assertion that exists to catch exactly that stayed green, with the dashboard off and
// /dashboard/ returning 404.
func hasArgvFlag(argv []string, flag string) bool {
	for _, f := range argv {
		if f == flag {
			return true
		}
	}
	return false
}

func argvFlagValue(argv []string, flag string) (string, bool) {
	for i, f := range argv {
		if f == flag && i+1 < len(argv) {
			return argv[i+1], true
		}
	}
	return "", false
}

// TestCheckProxyRecoveryCommandIsTheRealLaunchPath: the note printed when nothing answers on the port
// used to carry a hand-rolled `context-guru-proxy --listen … --preset …`, and that duplicate had drifted
// from what start-proxy.sh actually launches in four ways at once — it named the plugin option's preset
// (which --config replaces anyway) and omitted --config, --anthropic-upstream and --idle-exit.
//
// Pasting it therefore turned keep-alive OFF for somebody who had enabled it and was paying for the
// pings, bypassed a configured gateway, and left a proxy that never idle-exits. All silently, while the
// user was already troubleshooting.
//
// So this does not assert on the note's WORDING. It extracts the command the note prints and RUNS it,
// then asserts on the argv the proxy was launched with. A flag list can be repaired and drift again; what
// has to hold is that the printed command produces the same proxy the hook would have started.
func TestCheckProxyRecoveryCommandIsTheRealLaunchPath(t *testing.T) {
	// The upstream cases matter because the printed command is re-parsed by the user's shell. An
	// unquoted `&` backgrounds the command mid-way, so the paste succeeds, starts a proxy, and chains it
	// to a TRUNCATED upstream with nothing said — a silent partial success, which is the same failure
	// mode as everything else in this file.
	for _, c := range []struct{ name, upstream string }{
		{"a plain upstream", "http://gw.example:4000"},
		{"an upstream whose query string contains &", "http://gw.example:4000/v1?tenant=acme&mode=chain"},
	} {
		t.Run(c.name, func(t *testing.T) {
			requireTool(t, "bash")
			const preset, idle = "house", "90m"

			dir := t.TempDir()
			state := filepath.Join(dir, "state")
			stateDir := filepath.Join(state, "context-guru")
			if err := os.MkdirAll(stateDir, 0o755); err != nil {
				t.Fatal(err)
			}
			port := freePort(t)
			// A keep-alive config exists, which is the case the old command silently discarded.
			keepalive := filepath.Join(stateDir, "keepalive-"+port+".yaml")
			if err := os.WriteFile(keepalive, []byte("preset: "+preset+"\ncache:\n  keepalive: true\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			// Step 1: get the note. Auto-recovery has to FAIL for it to print, so the binary is absent.
			root, err := filepath.Abs(".")
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "check-proxy.sh"))
			cmd.Env = append(sandboxEnv(t),
				"CLAUDE_PLUGIN_ROOT="+root,
				"CLAUDE_PLUGIN_OPTION_PORT="+port,
				"CLAUDE_PLUGIN_OPTION_PRESET="+preset,
				"CLAUDE_PLUGIN_OPTION_IDLE_EXIT="+idle,
				"CLAUDE_PLUGIN_OPTION_UPSTREAM="+c.upstream,
				"ANTHROPIC_BASE_URL=http://127.0.0.1:"+port+"/anthropic",
				"CONTEXT_GURU_BIN="+filepath.Join(dir, "does-not-exist"),
				"CONTEXT_GURU_HEALTH_BUDGET=1",
				"XDG_STATE_HOME="+state,
				"TMPDIR="+dir)
			b, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("check-proxy.sh must never fail a prompt: %v\n%s", err, b)
			}
			note := string(b)
			t.Logf("note ->\n%s", note)

			// The duplicated command line must be gone, not merely corrected.
			if strings.Contains(note, "context-guru-proxy --listen") {
				t.Errorf("the note still prints a hand-rolled proxy command line; that duplicate is what "+
					"drifted from the real launch path four times:\n%s", note)
			}

			// Step 2: pull the command out of the note and run it, with a proxy binary that works.
			var recover string
			for _, line := range strings.Split(note, "\n") {
				if strings.Contains(line, "start-proxy.sh") && strings.Contains(line, "--unrouted") {
					recover = strings.TrimSpace(line)
					break
				}
			}
			if recover == "" {
				t.Fatalf("the note offers no runnable recovery command:\n%s", note)
			}

			argvFile := filepath.Join(dir, "argv")
			fake := filepath.Join(dir, "fake-proxy")
			py := requireTool(t, "python3")
			script := "#!/usr/bin/env bash\n" +
				"printf '%s\\n' \"$*\" > " + argvFile + "\n" +
				"exec " + py + " -c '\n" +
				"import http.server\n" +
				"class H(http.server.BaseHTTPRequestHandler):\n" +
				"    def do_GET(self):\n" +
				"        self.send_response(200); self.end_headers(); self.wfile.write(b\"ok\")\n" +
				"    def log_message(self, *a): pass\n" +
				"http.server.HTTPServer((\"127.0.0.1\", " + port + "), H).serve_forever()\n'\n"
			if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}

			// Deliberately NOT carrying the CLAUDE_PLUGIN_OPTION_* values: a terminal does not have them,
			// which is the whole reason the printed command has to pass them as flags. Only CONTEXT_GURU_BIN
			// and the state/tmp redirections are kept, because the test cannot install a real binary.
			run := exec.Command("bash", "-c", recover)
			run.Env = append(sandboxEnv(t),
				"CLAUDE_PLUGIN_OPTION_PORT=", "CLAUDE_PLUGIN_OPTION_PRESET=", "CLAUDE_PLUGIN_OPTION_IDLE_EXIT=",
				"CLAUDE_PLUGIN_OPTION_UPSTREAM=", "ANTHROPIC_BASE_URL=", "ANTHROPIC_UPSTREAM=",
				"CONTEXT_GURU_BIN="+fake,
				"XDG_STATE_HOME="+state,
				"TMPDIR="+dir)
			out2, err := run.CombinedOutput()
			t.Cleanup(func() {
				if pb, e := os.ReadFile(filepath.Join(stateDir, "proxy-"+port+".pid")); e == nil {
					exec.Command("kill", strings.TrimSpace(string(pb))).Run() //nolint:errcheck
				}
			})
			if err != nil {
				t.Fatalf("the printed recovery command failed: %v\n%s", err, out2)
			}
			raw, rerr := os.ReadFile(argvFile)
			if rerr != nil {
				t.Fatalf("the printed recovery command never launched a proxy: %v\nnote:\n%s\nrun:\n%s",
					rerr, note, out2)
			}
			argv := strings.Fields(string(raw))
			t.Logf("recovery command launched: %s", strings.TrimSpace(string(raw)))

			// Flags carrying a value: assert the VALUE, so a truncated one fails.
			for _, want := range []struct{ what, flag, value string }{
				{"the configured port", "--listen", "127.0.0.1:" + port},
				{"the keep-alive config (omitting it turns off keep-alive the user is paying for)",
					"--config", keepalive},
				{"the configured upstream (a truncated one bypasses the gateway holding their credential)",
					"--anthropic-upstream", c.upstream},
			} {
				got, ok := argvFlagValue(argv, want.flag)
				if !ok {
					t.Errorf("the recovered proxy never got %s (%s missing): %v", want.what, want.flag, argv)
					continue
				}
				if got != want.value {
					t.Errorf("%s is wrong: %s = %q, want %q", want.what, want.flag, got, want.value)
				}
			}
			// Whole-token flags. --dashboard MUST be matched as a token: it is a prefix of --dashboard-db,
			// and a substring check here passed with the bare flag deleted and the dashboard off.
			for _, flag := range []string{"--dashboard", "--idle-exit=" + idle} {
				if !hasArgvFlag(argv, flag) {
					t.Errorf("the recovered proxy is missing %s, so it is not the proxy the hook would have "+
						"started: %v", flag, argv)
				}
			}
			// The pidfile is what uninstall uses; a recovery proxy it cannot find is unstoppable.
			if _, err := os.Stat(filepath.Join(stateDir, "proxy-"+port+".pid")); err != nil {
				t.Errorf("no pidfile, so uninstall could not stop the recovered proxy: %v", err)
			}
		})
	}
}

// TestCheckProxySaysSoWhenThereIsNoStarterToPointAt: with CLAUDE_PLUGIN_ROOT wrong or the plugin
// half-installed there is no script to delegate to. The note must say that rather than fall back to a
// hand-written command line — reintroducing the duplicate in the one case where nothing is verifiable is
// how the original defect would come back.
func TestCheckProxySaysSoWhenThereIsNoStarterToPointAt(t *testing.T) {
	requireTool(t, "bash")
	dir := t.TempDir()
	port := freePort(t)
	cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "check-proxy.sh"))
	cmd.Env = append(sandboxEnv(t),
		"CLAUDE_PLUGIN_ROOT="+filepath.Join(dir, "not-the-plugin"),
		"CLAUDE_PLUGIN_OPTION_PORT="+port,
		"ANTHROPIC_BASE_URL=http://127.0.0.1:"+port+"/anthropic",
		"CONTEXT_GURU_HEALTH_BUDGET=1",
		"XDG_STATE_HOME="+filepath.Join(dir, "state"),
		"TMPDIR="+dir)
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("must never fail a prompt: %v\n%s", err, b)
	}
	note := string(b)
	t.Logf("note ->\n%s", note)
	if strings.Contains(note, "context-guru-proxy --listen") {
		t.Errorf("fell back to a hand-rolled command line, which is the defect this replaced:\n%s", note)
	}
	if !strings.Contains(note, "not where this hook expects it") {
		t.Errorf("the missing starter is not reported:\n%s", note)
	}
	if !strings.Contains(note, "/context-guru:install") {
		t.Errorf("no way forward is offered:\n%s", note)
	}
}

// TestStartProxyTakesPresetAndIdleExitAsArguments: both existed only as CLAUDE_PLUGIN_OPTION_* reads,
// which a terminal does not have — so the command check-proxy.sh prints could not carry them without an
// env prefix, and this file's own gate comment records a human losing three rounds of debugging to a
// pasted env prefix that split across lines and became a no-op.
func TestStartProxyTakesPresetAndIdleExitAsArguments(t *testing.T) {
	requireTool(t, "bash")
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	fake := filepath.Join(dir, "fake-proxy")
	body := "#!/usr/bin/env bash\nprintf '%s\\n' \"$*\" > " + argvFile + "\nprintenv PRESET >> " +
		argvFile + "\nsleep 30\n"
	if err := os.WriteFile(fake, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	state := filepath.Join(dir, "state")
	cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "start-proxy.sh"),
		"--unrouted", "--bin", fake, "--port", port, "--preset", "house", "--idle-exit", "90m")
	// The options say something DIFFERENT, so a passing test cannot be reading them instead.
	cmd.Env = append(sandboxEnv(t),
		"CLAUDE_PLUGIN_OPTION_PRESET=codesmart",
		"CLAUDE_PLUGIN_OPTION_IDLE_EXIT=24h",
		"ANTHROPIC_BASE_URL=",
		"CONTEXT_GURU_BIN=",
		"CONTEXT_GURU_HEALTH_BUDGET=1",
		"XDG_STATE_HOME="+state,
		"TMPDIR="+dir)
	out, err := cmd.CombinedOutput()
	t.Cleanup(func() { exec.Command("pkill", "-f", fake).Run() }) //nolint:errcheck
	if err != nil {
		t.Fatalf("must never fail a session: %v\n%s", err, out)
	}
	got, rerr := os.ReadFile(argvFile)
	if rerr != nil {
		t.Fatalf("the proxy never ran: %v\n%s", rerr, out)
	}
	launched := string(got)
	t.Logf("launched: %s", launched)
	if !strings.Contains(launched, "--idle-exit=90m") {
		t.Errorf("--idle-exit was not honoured as an argument: %s", launched)
	}
	if strings.Contains(launched, "--idle-exit=24h") {
		t.Errorf("the option beat the argument for idle-exit: %s", launched)
	}
	// PRESET reaches the proxy as an environment variable, so it is printed by the fake rather than argv.
	if !strings.Contains(launched, "house") {
		t.Errorf("--preset was not honoured as an argument: %s", launched)
	}
	if strings.Contains(launched, "codesmart") {
		t.Errorf("the option beat the argument for preset: %s", launched)
	}
}

// TestStartProxyReportsThePresetActuallyInEffect: the success note used to print $PRESET, which comes
// from the plugin option, even though --config REPLACES the preset entirely. So with a keep-alive config
// in play the proxy ran whatever `preset:` that file recorded while the note named the option — and the
// two diverge the moment somebody changes the option after enabling keep-alive. Same species as the
// defect this branch started from: a confident report of a value that is not in effect.
//
// The reading of the file happens on the SessionStart path, so every degenerate config must still START
// the proxy, and must not produce a note that is wrong in the other direction. "Reported nothing" beats
// "reported wrong", and neither beats "did not start" — hence the exit-0 and launched checks on every
// row, not just the happy one.
func TestStartProxyReportsThePresetActuallyInEffect(t *testing.T) {
	const optionPreset = "codesmart" // what the PLUGIN OPTION says, and what the note must not parrot
	for _, c := range []struct {
		name       string
		writeCfg   bool
		cfg        string
		unreadable bool // chmod 000 after writing: the ONLY config that makes sed itself fail
		wantIn     []string
		wantNotIn  []string
	}{
		{
			name:     "the config's preset is reported, not the plugin option's",
			writeCfg: true, cfg: "preset: house\ncache:\n  keepalive: true\n",
			wantIn:    []string{"preset house"},
			wantNotIn: []string{"preset " + optionPreset},
		},
		{
			name:     "a quoted and padded value is still read",
			writeCfg: true, cfg: "preset:   \"house\"  \ncache:\n  keepalive: true\n",
			wantIn:    []string{"preset house"},
			wantNotIn: []string{"preset " + optionPreset},
		},
		{
			// Older files predate the always-state-the-preset rule. Report it as unstated: a config may
			// set `pipeline:` directly, so claiming compaction is OFF would be wrong in the other
			// direction — the failure mode this whole test exists to prevent.
			name:     "no preset line: unstated, and never claimed to be off",
			writeCfg: true, cfg: "cache:\n  keepalive: true\n",
			wantIn:    []string{"unstated"},
			wantNotIn: []string{"preset " + optionPreset, "compaction is OFF"},
		},
		{
			name:     "a comment-only config claims nothing",
			writeCfg: true, cfg: "# written by hand, nothing else\n",
			wantIn:    []string{"unstated"},
			wantNotIn: []string{"preset " + optionPreset},
		},
		{
			name:     "an empty config claims nothing",
			writeCfg: true, cfg: "",
			wantIn:    []string{"unstated"},
			wantNotIn: []string{"preset " + optionPreset},
		},
		{
			name:     "malformed junk still starts the proxy and claims nothing",
			writeCfg: true, cfg: "\x00\x01 not: [yaml: at all\n",
			wantIn:    []string{"unstated"},
			wantNotIn: []string{"preset " + optionPreset},
		},
		{
			// The row that defends the fail-open property itself. Every other row has a config sed can
			// READ — empty, comment-only and junk files all match nothing and exit 0 — so none of them
			// can see `set -e` being added. This one can: with pipefail in force, an unreadable file
			// makes the assignment non-zero, and -e would then kill the script before the proxy starts.
			name:     "an unreadable config still starts the proxy (this is what -e would break)",
			writeCfg: true, cfg: "preset: house\ncache:\n  keepalive: true\n", unreadable: true,
			wantIn:    []string{"unstated"},
			wantNotIn: []string{"preset " + optionPreset},
		},
		{
			// Reachable by hand edit, not theoretical: this document LOADS, with a top-level preset of
			// house and a full house pipeline, so a first-match-at-any-indentation read would name the
			// inner value while the proxy ran the outer one.
			name:      "a nested preset: does not shadow the top-level one",
			writeCfg:  true,
			cfg:       "components:\n  offload:\n    preset: inner\npreset: house\ncache:\n  keepalive: true\n",
			wantIn:    []string{"preset house"},
			wantNotIn: []string{"inner", "preset " + optionPreset},
		},
		{
			name:     "an inline comment does not leak into the note",
			writeCfg: true, cfg: "preset: house # kept for the split\ncache:\n  keepalive: true\n",
			wantIn:    []string{"preset house"},
			wantNotIn: []string{"kept for the split", "preset " + optionPreset},
		},
		{
			// With no config there is no --config, so the plugin option IS what is in effect and the
			// note should say so. Without this row the test would pass on a note that never reports a
			// preset at all.
			name:      "no keepalive config: the plugin option is the truth and is reported",
			writeCfg:  false,
			wantIn:    []string{"preset " + optionPreset, "cache strategy none"},
			wantNotIn: []string{"unstated"},
		},
		{
			// The NAME is the whole reason strategies are named: it is the only thing a user can say
			// back to us. It has to survive from the file into the note, or "switch it back to
			// 5-min-ping" is not a sentence anybody can act on.
			//
			// THE PRESET HALF OF THIS ROW FLIPPED, deliberately. It used to assert that the file's
			// `preset: house` was reported and the plugin option was NOT — the file being the source
			// of truth for the preset was the whole point. That is the defect #264 removes: --config
			// REPLACES --preset, so whatever preset was recorded when the strategy was first armed
			// stayed in force forever, and a user who changed the option watched it stick in the
			// config UI while the old one kept running. `strategy sync` now re-renders an OWNED file's
			// preset from the option before every launch, so the option is what is in effect and
			// reporting it is correct.
			//
			// The invariant this row actually defends is unchanged and is why it still earns its
			// place: the note must name the preset IN EFFECT rather than one merely configured
			// somewhere. Only which of the two that is has changed.
			//
			// Worth reading with the four rows around it: they carry configs with no marker of ours,
			// sync declines to touch them, and they still report `preset house`. That contrast is the
			// ownership gate on sync demonstrating itself — this row is the only one whose behaviour
			// moved, which is exactly the set of files sync is licensed to rewrite.
			name:     "the strategy name in the marker is reported",
			writeCfg: true,
			cfg: "# context-guru: strategy=1-hour-head written by /context-guru:cache-strategy-picker\n" +
				"preset: house\ncache:\n  head_ttl_1h: true\n",
			wantIn:    []string{"cache strategy 1-hour-head", "preset " + optionPreset},
			wantNotIn: []string{"cache strategy none", "preset house"},
		},
		{
			// A file we do NOT own whose first line happens to carry `strategy=`. The name read ran the
			// `sed` on line 1 of whatever was at that path, with no ownership test — so a foreign config
			// was announced as `cache strategy <theirs>` in the startup note while `strategy show` called
			// the same file `(foreign)`. The stated point of recording the name in the file is that the
			// two readers cannot disagree, so this is the row that holds them to it.
			name:      "a foreign config's strategy= is not adopted as ours",
			writeCfg:  true,
			cfg:       "# rolled by hand strategy=evil\npreset: house\ncache:\n  keepalive: true\n",
			wantIn:    []string{"cache strategy unnamed"},
			wantNotIn: []string{"cache strategy evil", "strategy evil"},
		},
		{
			// A config we own but that predates named strategies, or one we do not own at all: report
			// it as unnamed rather than guessing. Naming the DEFAULT here would be the worst answer —
			// it would tell the user 5-min-ping is armed when the file might say anything.
			name:     "a config with no strategy marker is unnamed, never guessed",
			writeCfg: true, cfg: "preset: house\ncache:\n  keepalive: true\n",
			wantIn:    []string{"cache strategy unnamed"},
			wantNotIn: []string{"cache strategy 5-min-ping", "cache strategy none"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			requireTool(t, "bash")
			dir := t.TempDir()
			argvFile := filepath.Join(dir, "argv")
			fake := filepath.Join(dir, "fake-proxy")
			port := freePort(t)
			py := requireTool(t, "python3")
			script := "#!/usr/bin/env bash\n" +
				"printf '%s\\n' \"$*\" > " + argvFile + "\n" +
				"exec " + py + " -c '\n" +
				"import http.server\n" +
				"class H(http.server.BaseHTTPRequestHandler):\n" +
				"    def do_GET(self):\n" +
				"        self.send_response(200); self.end_headers(); self.wfile.write(b\"ok\")\n" +
				"    def log_message(self, *a): pass\n" +
				"http.server.HTTPServer((\"127.0.0.1\", " + port + "), H).serve_forever()\n'\n"
			if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}

			state := filepath.Join(dir, "state")
			if c.writeCfg {
				stateDir := filepath.Join(state, "context-guru")
				if err := os.MkdirAll(stateDir, 0o755); err != nil {
					t.Fatal(err)
				}
				cfgPath := filepath.Join(stateDir, "keepalive-"+port+".yaml")
				if err := os.WriteFile(cfgPath, []byte(c.cfg), 0o644); err != nil {
					t.Fatal(err)
				}
				if c.unreadable {
					if os.Geteuid() == 0 {
						t.Skip("root ignores mode 000, so this row cannot make the read fail")
					}
					if err := os.Chmod(cfgPath, 0o000); err != nil {
						t.Fatal(err)
					}
				}
			}

			cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "start-proxy.sh"))
			cmd.Env = append(sandboxEnv(t),
				"CLAUDE_PLUGIN_OPTION_PORT="+port,
				"CLAUDE_PLUGIN_OPTION_PRESET="+optionPreset,
				"ANTHROPIC_BASE_URL=http://127.0.0.1:"+port+"/anthropic",
				"CONTEXT_GURU_BIN="+fake,
				"XDG_STATE_HOME="+state,
				"TMPDIR="+dir)
			out, err := cmd.CombinedOutput()
			t.Cleanup(func() {
				if b, e := os.ReadFile(filepath.Join(state, "context-guru", "proxy-"+port+".pid")); e == nil {
					exec.Command("kill", strings.TrimSpace(string(b))).Run() //nolint:errcheck
				}
			})
			// Fails open: property 5 of this script is that it never fails a session, whatever the
			// config on disk looks like.
			if err != nil {
				t.Fatalf("must never fail a session: %v\n%s", err, out)
			}
			if _, rerr := os.Stat(argvFile); rerr != nil {
				t.Fatalf("the proxy was never launched, so this config stopped a session starting: %v\n%s",
					rerr, out)
			}
			got := string(out)
			t.Logf("note ->\n%s", got)
			for _, w := range c.wantIn {
				if !strings.Contains(got, w) {
					t.Errorf("note does not contain %q:\n%s", w, got)
				}
			}
			for _, w := range c.wantNotIn {
				if strings.Contains(got, w) {
					t.Errorf("note contains %q, which is not what is in effect:\n%s", w, got)
				}
			}
		})
	}
}

// --- findings from Osher's end-to-end review of #160 ------------------------------------------

// TestTheConfiguredPortCanActuallyBeHonoured is the blocker, and it is about a silent mismatch rather
// than a wrong default.
//
// PORT came only from CLAUDE_PLUGIN_OPTION_PORT, which Claude Code puts into HOOK environments and NOT
// into a Bash tool call — so an install skill reading `${CLAUDE_PLUGIN_OPTION_PORT:-8787}` always got
// 8787 whatever the user configured, and start-proxy.sh had no argument form to override it. The
// consequence is the failure this plugin exists to prevent: the routing key names 8787 while every
// later hook reads the CONFIGURED port and self-gates on it, so the hooks treat the project as
// unrouted and do nothing. The one running proxy has no auto-restart behind it, and once it idle-exits
// nothing brings it back, silently.
//
// Two halves, both tested: the port must be discoverable from disk, and it must be passable as an
// argument a permission rule can cover.
func TestTheConfiguredPortCanActuallyBeHonoured(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hook is POSIX-only")
	}

	t.Run("discoverable from the settings file, with no env var present", func(t *testing.T) {
		dir := t.TempDir()
		cfg := filepath.Join(dir, "cfg")
		if err := os.MkdirAll(cfg, 0o755); err != nil {
			t.Fatal(err)
		}
		// The shape Claude Code writes for `--config port=4041`.
		writeJSON(t, filepath.Join(cfg, "settings.json"), map[string]any{
			"pluginConfigs": map[string]any{
				"context-guru@context-guru": map[string]any{
					"options": map[string]any{"port": 4041, "preset": "house"},
				},
			},
		})
		py := requireTool(t, "python3")
		cmd := exec.Command(py, filepath.Join(scriptsDir(t), "settings.py"), "config")
		cmd.Env = append(sandboxEnv(t), "CLAUDE_CONFIG_DIR="+cfg)
		cmd.Dir = dir // so the project-scope candidates do not accidentally match
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("config failed: %v\n%s", err, out)
		}
		got := string(out)
		t.Logf("config ->\n%s", got)
		if !strings.Contains(got, "option_port=4041") {
			t.Errorf("the configured port was not discovered; without this the install can only ever "+
				"use 8787:\n%s", got)
		}
		if !strings.Contains(got, "option_preset=house") {
			t.Errorf("other options are not reported either:\n%s", got)
		}
	})

	t.Run("passable as an argument, and everything derived from it follows", func(t *testing.T) {
		requireTool(t, "bash")
		dir := t.TempDir()
		argv := filepath.Join(dir, "argv.log")
		fake := filepath.Join(dir, "fake")
		body := "#!/usr/bin/env bash\nprintf '%s\\n' \"$*\" >> " + argv + "\nsleep 30\n"
		if err := os.WriteFile(fake, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		port := freePort(t)
		state := filepath.Join(dir, "state")
		cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "start-proxy.sh"),
			"--unrouted", "--bin", fake, "--port", port)
		// Deliberately NO CLAUDE_PLUGIN_OPTION_PORT: that is the situation a Bash tool call is in.
		cmd.Env = append(sandboxEnv(t),
			"CLAUDE_PLUGIN_OPTION_PORT=",
			"ANTHROPIC_BASE_URL=",
			"CONTEXT_GURU_BIN=",
			"CONTEXT_GURU_HEALTH_BUDGET=1",
			"XDG_STATE_HOME="+state,
			"TMPDIR="+dir)
		out, err := cmd.CombinedOutput()
		t.Cleanup(func() { exec.Command("pkill", "-f", fake).Run() }) //nolint:errcheck
		if err != nil {
			t.Fatalf("must never fail a session: %v\n%s", err, out)
		}
		launched, _ := os.ReadFile(argv)
		if !strings.Contains(string(launched), "--listen 127.0.0.1:"+port) {
			t.Errorf("the proxy did not listen on the port passed as an argument: %s\n%s", launched, out)
		}
		// The pidfile is what uninstall uses, and it is named after the port — so a port that reached
		// --listen but not the pidfile would leave an unstoppable proxy.
		if _, err := os.Stat(filepath.Join(state, "context-guru", "proxy-"+port+".pid")); err != nil {
			t.Errorf("no pidfile for the configured port, so uninstall could not find this proxy: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "context-guru-proxy-"+port+".log")); err != nil {
			t.Errorf("the log is not named after the configured port: %v", err)
		}
	})

	// The state none of the four skills was written for: SOME options set, others not. cmd_config
	// prints a line only for keys actually present and falls back to `source=(none)` only when the
	// whole options object is missing, so a partial config reports a real `source=` and simply omits
	// the rest. Every skill documented its fallback as keyed on `source=(none)`, which does not apply
	// here — leaving the model to invent a value at the exact point those sections warn is dangerous.
	t.Run("a partial config reports a real source and omits the options not set", func(t *testing.T) {
		dir := t.TempDir()
		cfg := filepath.Join(dir, "cfg")
		if err := os.MkdirAll(cfg, 0o755); err != nil {
			t.Fatal(err)
		}
		// port set, preset never touched — the common case, and the one that used to mislead.
		writeJSON(t, filepath.Join(cfg, "settings.json"), map[string]any{
			"pluginConfigs": map[string]any{
				"context-guru@context-guru": map[string]any{
					"options": map[string]any{"port": 4041},
				},
			},
		})
		py := requireTool(t, "python3")
		cmd := exec.Command(py, filepath.Join(scriptsDir(t), "settings.py"), "config")
		cmd.Env = append(sandboxEnv(t), "CLAUDE_CONFIG_DIR="+cfg)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("config failed: %v\n%s", err, out)
		}
		got := string(out)
		t.Logf("config ->\n%s", got)
		if !strings.Contains(got, "option_port=4041") {
			t.Errorf("the configured port was not reported:\n%s", got)
		}
		if strings.Contains(got, "option_preset=") {
			t.Errorf("an unconfigured option was reported as though it had a value:\n%s", got)
		}
		if strings.Contains(got, "source=(none)") {
			t.Errorf("a partial config reported source=(none); the skills' per-option fallback rule "+
				"exists precisely because this reports a real source:\n%s", got)
		}
	})

	t.Run("a non-numeric port is reported, not used", func(t *testing.T) {
		requireTool(t, "bash")
		dir := t.TempDir()
		cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "start-proxy.sh"),
			"--unrouted", "--port", "not-a-port")
		cmd.Env = append(sandboxEnv(t), "ANTHROPIC_BASE_URL=", "CONTEXT_GURU_BIN=/nonexistent/x",
			"CONTEXT_GURU_HEALTH_BUDGET=1", "XDG_STATE_HOME="+dir, "TMPDIR="+dir)
		out, _ := cmd.CombinedOutput()
		if !strings.Contains(string(out), "ignoring --port") {
			t.Errorf("a junk port was accepted silently:\n%s", out)
		}
	})
}

// TestSettingsReportsOSErrorsAsData: the module docstring promises one key=value line per fact, and the
// calling skill is told to read those rather than guess. Every failure path honoured that EXCEPT genuine
// OS errors in backup()/save(), which produced a raw Python traceback and no `result=` line at all —
// so the caller had nothing to act on precisely when something was already wrong.
//
// Non-destructive in every case observed, but "it did not damage anything" is not "the caller can tell
// what happened".
func TestSettingsReportsOSErrorsAsData(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes only")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory mode this test relies on")
	}
	dir := t.TempDir()
	ro := filepath.Join(dir, "ro")
	if err := os.MkdirAll(ro, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ro, "settings.json")
	writeJSON(t, path, map[string]any{"env": map[string]any{"MINE": "keep"}})
	if err := os.Chmod(ro, 0o555); err != nil { // no writes in this directory: backup() cannot create
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })

	facts, code := settings(t, "add", "--file", path, "--url", ourURL)
	if code == 0 {
		t.Errorf("an unwritable directory reported success: %v", facts)
	}
	if facts["result"] != "error" || facts["reason"] != "os_error" {
		t.Errorf("result=%q reason=%q, want error/os_error — a traceback breaks the key=value contract "+
			"the skill is told to rely on", facts["result"], facts["reason"])
	}
	// And it must not have damaged the file it could not replace.
	if env := readJSON(t, path)["env"].(map[string]any); env["MINE"] != "keep" {
		t.Errorf("the original file was altered on a failed write: %v", env)
	}
}

// ---------------------------------------------------------------------------
// The escape hatch.
//
// These tests exist because of an incident rather than a design review. A colleague installed the
// plugin, ended up with 401 on every request, and found that `/context-guru:uninstall` — the
// documented undo — could not run: it is a skill, and a skill needs a session that can reach a
// model. The undo path was unavailable for precisely the reason he needed it.
//
// So the properties below are not "the hatch works". They are "the hatch works when everything
// this plugin provides is gone": no Claude, no proxy, no plugin directory, no interpreter beyond
// POSIX sh. Each test removes one of those and asserts recovery anyway.
// ---------------------------------------------------------------------------

// hatchPath is where settings.py installs the hatch, and asserts the two properties that make it a
// hatch at all: it is executable, and it is NOT inside the plugin.
func hatchPath(t *testing.T, state string) string {
	t.Helper()
	p := filepath.Join(state, "context-guru-reset")
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("no escape hatch at %s: %v", p, err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("the hatch at %s is not executable (mode %v) — a user in trouble should not have "+
			"to work out that they need to prefix it with `sh`", p, fi.Mode().Perm())
	}
	if strings.HasPrefix(p, scriptsDir(t)) {
		t.Fatalf("the hatch was installed INSIDE the plugin (%s). `/plugin uninstall`, a "+
			"marketplace refresh or a wiped plugin cache would take the recovery tool with it", p)
	}
	return p
}

// runHatch executes the installed hatch with a controlled environment. cwd matters: the no-record
// path reports files relative to the project the user is in.
func runHatch(t *testing.T, state, home, cwd string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(hatchPath(t, state), args...)
	cmd.Dir = cwd
	cmd.Env = []string{
		"CONTEXT_GURU_STATE=" + state,
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running the hatch: %v (%s)", err, out)
	}
	t.Logf("hatch %v -> exit %d\n%s", args, code, out)
	return string(out), code
}

// TestInstallLeavesAWayBackOutsideThePlugin is the whole feature in one assertion: after an install
// there is a runnable recovery tool that does not live in the plugin, its path was REPORTED so the
// skill can tell the user, and a recovery copy of the file that was edited exists BESIDE it — not
// in a shared, hashed ledger under the state directory. See recovery_dir_for() in settings.py.
func TestInstallLeavesAWayBackOutsideThePlugin(t *testing.T) {
	state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
	path := filepath.Join(proj, "settings.json")
	writeJSON(t, path, map[string]any{"theme": "dark"})

	facts, code := settingsIn(t, state, home, "add", "--file", path, "--url", ourURL)
	if code != 0 {
		t.Fatalf("add: exit %d, %v", code, facts)
	}
	// Reported, not merely present. The install skill has to hand the path to the user while the
	// session still works, because afterwards it may not.
	if facts["reset_hatch"] == "" || facts["reset_hatch"] == "unavailable" {
		t.Errorf("add did not report a usable reset_hatch: %v", facts)
	}
	if got, want := facts["reset_hatch"], hatchPath(t, state); got != want {
		t.Errorf("reported reset_hatch=%q, but the hatch is at %q", got, want)
	}
	wantRecovery := filepath.Join(proj, "context-guru-settings-json")
	if facts["recovery_dir"] != wantRecovery {
		t.Errorf("recovery_dir=%q, want %q", facts["recovery_dir"], wantRecovery)
	}
	original := filepath.Join(wantRecovery, "settings.pre-install.json")
	if facts["recovery_original"] != original {
		t.Errorf("recovery_original=%q, want %q", facts["recovery_original"], original)
	}
	got := readJSON(t, original)
	if got["theme"] != "dark" {
		t.Errorf("the pre-install copy does not hold what the file looked like before: %v", got)
	}
}

// TestRecoveryDirCreationDoesNotTightenTheUsersOwnDirectory is the regression test for a real
// review finding: ensure_dir_0700 (called via recovery_dir_for()) used to chmod BOTH the recovery
// folder AND its parent to 0700. For every other caller that parent is plugin-owned state, but for
// a recovery folder it is the settings file's own directory — `.claude/`, or a project root for a
// user-scope install — which the user or their team may have deliberately made group- or
// world-readable (a shared box, a team repo). Verified independently: a 775 directory silently
// became 700 on the very first settings write, and stayed that way on every one after, with
// nothing disclosing it anywhere. The recovery folder itself still gets 0700 either way — it can
// hold a credential-bearing copy of the settings file, the same threat model B3 describes — only
// the PARENT's mode has to survive untouched, because only the parent is not ours to manage.
func TestRecoveryDirCreationDoesNotTightenTheUsersOwnDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits only")
	}
	state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
	claudeDir := filepath.Join(proj, ".claude")
	if err := os.MkdirAll(claudeDir, 0o775); err != nil {
		t.Fatal(err)
	}
	// MkdirAll's mode is masked by this process's umask too, so set exactly what the test needs
	// regardless of it, the same reason ensure_dir_0700 itself chmods explicitly rather than
	// trusting mkdir's mode argument.
	if err := os.Chmod(claudeDir, 0o775); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(claudeDir, "settings.local.json")

	if _, code := settingsIn(t, state, home, "add", "--file", path, "--url", ourURL); code != 0 {
		t.Fatal("add failed")
	}

	fi, err := os.Stat(claudeDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o775 {
		t.Errorf(".claude/ mode changed from 0775 to %o — a settings write silently tightened a "+
			"directory it does not own", got)
	}
	rfi, err := os.Stat(filepath.Join(claudeDir, "context-guru-settings-json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := rfi.Mode().Perm(); got != 0o700 {
		t.Errorf("recovery folder mode = %o, want 0700 — it can hold a credential-bearing copy", got)
	}
}

// TestTheHatchNeedsNothingButPOSIXSh: the hatch runs in a state where the proxy is down and Claude
// cannot talk, so anything it depends on is a way for it to be unavailable too. Asserted against
// the script's text, since "it happened to work on this machine" is not the claim.
// TestTheHatchRunsUnderEveryShellItClaims does what the static check below cannot: it RUNS the hatch
// under each POSIX shell available on this machine and asserts it actually recovers.
//
// S7 in review, and the repo has a commit titled "assert the fallback CONDITION, not one sentence that
// expressed it" — the static test asserts a shebang string and five banned words, which is a proxy for
// portability, not portability. `dash` and `busybox sh` are where a bashism actually shows up, and the
// filter now contains an awk program, which the word list would never have caught.
func TestTheHatchRunsUnderEveryShellItClaims(t *testing.T) {
	var shells []string
	for _, sh := range []string{"sh", "dash", "bash", "busybox"} {
		if p, err := exec.LookPath(sh); err == nil {
			shells = append(shells, p)
		}
	}
	if len(shells) == 0 {
		t.Fatal("no POSIX shell on PATH at all")
	}
	for _, sh := range shells {
		name := filepath.Base(sh)
		t.Run(name, func(t *testing.T) {
			state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
			// One of the three canonical scope paths the hatch itself now checks — it no longer
			// reads an arbitrary manifest entry, so a settings file anywhere else is invisible to
			// it. See reset.sh's fixed candidate list.
			if err := os.MkdirAll(filepath.Join(proj, ".claude"), 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(proj, ".claude", "settings.local.json")
			writeJSON(t, path, map[string]any{"model": "opus",
				"permissions": map[string]any{"allow": []string{"Bash(ls:*)"}}})
			pristine, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, code := settingsIn(t, state, home, "add", "--file", path, "--url", ourURL); code != 0 {
				t.Fatal("add failed")
			}
			args := []string{filepath.Join(state, "context-guru-reset"), "--yes"}
			if name == "busybox" {
				args = append([]string{"sh"}, args...)
			}
			cmd := exec.Command(sh, args...)
			cmd.Dir = proj
			cmd.Env = sandboxEnv(t, "CONTEXT_GURU_STATE="+state, "HOME="+home)
			b, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s: %v\n%s", name, err, b)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(pristine) {
				t.Errorf("%s: did not restore the file\n got:  %s\n want: %s\n%s",
					name, got, pristine, b)
			}
			if !strings.Contains(string(b), "Done. 1 file(s) put back.") {
				t.Errorf("%s: no success summary:\n%s", name, b)
			}
		})
	}
}

func TestTheHatchNeedsNothingButPOSIXSh(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(scriptsDir(t), "reset.sh"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	if !strings.HasPrefix(body, "#!/bin/sh") {
		t.Errorf("the hatch must be POSIX sh; its shebang is %q",
			strings.SplitN(body, "\n", 2)[0])
	}
	// Line-by-line, with comments stripped, so a word that appears while EXPLAINING the absence of
	// a dependency is not read as the dependency. `claude` is deliberately not in this list: the
	// script names it in the prose it prints ("start a NEW claude session"), which is advice rather
	// than an invocation — the first version of this test failed on exactly that line.
	for i, line := range strings.Split(body, "\n") {
		code := line
		if idx := strings.Index(line, "#"); idx >= 0 {
			code = line[:idx]
		}
		for _, banned := range []string{"python", "curl ", "context-guru-proxy", "jq ", "$(claude"} {
			if strings.Contains(code, banned) {
				t.Errorf("reset.sh:%d depends on %q, which may be exactly what is missing:\n%s",
					i+1, banned, line)
			}
		}
	}
}

// TestTheOriginalCopyOutlivesTheRollingBackups is the reason the hatch keeps its own copy rather
// than trusting *.context-guru-backup-* files: those are capped at ten while add is writing them,
// and a clean remove now deletes all of them outright (see forget_backups()) — either way, on a
// machine that has installed and uninstalled a few times, nothing about a rolling backup window is
// where the user's pre-context-guru state can be made to survive. The copy recovery depends on
// cannot be on that window.
func TestTheOriginalCopyOutlivesTheRollingBackups(t *testing.T) {
	state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
	path := filepath.Join(proj, "settings.json")
	writeJSON(t, path, map[string]any{"env": map[string]any{"MY_OWN": "keepme"}})
	pristine, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 12; i++ { // twelve install/uninstall cycles is well past any rolling window
		if _, code := settingsIn(t, state, home, "add", "--file", path, "--url", ourURL); code != 0 {
			t.Fatalf("cycle %d: add failed", i)
		}
		if _, code := settingsIn(t, state, home, "remove", "--file", path, "--url", ourURL); code != 0 {
			t.Fatalf("cycle %d: remove failed", i)
		}
	}

	// Beside the file now, not in a shared, hashed `originals/` directory under the state dir — see
	// recovery_dir_for() in settings.py. copy_once's own O_EXCL/hardlink-once semantics are what
	// keep it surviving every one of the twelve cycles above, unlike the rolling backups.
	found := filepath.Join(proj, "context-guru-settings-json", "settings.pre-install.json")
	// A leading dot would make the copy invisible to `ls` and to every glob — including a user's
	// own, in a directory that exists to be read by hand. Deterministic naming off the settings
	// file's own basename makes this true by construction now, but the property is worth asserting
	// rather than assuming.
	if strings.HasPrefix(filepath.Base(found), ".") {
		t.Errorf("the pre-edit copy %q is a hidden file", found)
	}
	got, err := os.ReadFile(found)
	if err != nil {
		t.Fatalf("no pre-edit copy survived twelve install/uninstall cycles: %v", err)
	}
	if string(got) != string(pristine) {
		t.Errorf("the pre-edit copy is not the file as the user had it:\ngot:  %s\nwant: %s",
			got, pristine)
	}
}

// TestHatchRestoresWithThePluginDeleted is the scenario the hatch is for. The user's way out must
// not be inside the thing they are trying to get away from, so: install, delete the plugin's own
// copy of the script, and recover anyway.
func TestHatchRestoresWithThePluginDeleted(t *testing.T) {
	state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
	// A canonical scope path: the hatch checks exactly three fixed candidates now (see reset.sh),
	// not an arbitrary manifest entry, so a settings file anywhere else would be invisible to it.
	if err := os.MkdirAll(filepath.Join(proj, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, ".claude", "settings.local.json")
	writeJSON(t, path, map[string]any{
		"theme":       "dark",
		"env":         map[string]any{"MY_OWN": "keepme"},
		"permissions": map[string]any{"allow": []string{"Bash(ls)"}},
	})
	pristine, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, code := settingsIn(t, state, home, "add", "--file", path, "--url", ourURL,
		"--upstream", "https://gateway.corp.example"); code != 0 {
		t.Fatal("add failed")
	}
	// Prove the fixture: routing really is in the file, so the restore below is doing work.
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "127.0.0.1:8787") {
		t.Fatalf("fixture is wrong — no routing key was written:\n%s", b)
	}

	// The hatch's own copy is the one that runs. Delete the plugin's, restoring it afterwards, to
	// prove the installed copy is not quietly delegating to a file that may no longer exist.
	src := filepath.Join(scriptsDir(t), "reset.sh")
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(src, body, 0o755) })

	out, code := runHatch(t, state, home, proj, "--yes")
	if code != 0 {
		t.Fatalf("hatch: exit %d\n%s", code, out)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(pristine) {
		t.Errorf("the file was not restored to its pre-install state:\ngot:  %s\nwant: %s",
			got, pristine)
	}
	// Reversible in its own right: whatever was there before the restore is kept, for the user who
	// runs this and then finds the routing was not their problem.
	// In the recovery folder BESIDE the settings file now, not under the state directory (S6): a
	// complete copy of a file that can hold a credential belongs where /context-guru:install's
	// gitignore-ensure step can protect it, not in a shared location nobody would think to check.
	pre, err := filepath.Glob(filepath.Join(proj, ".claude", "context-guru-settings-json",
		"settings.local.pre-reset-*.json"))
	if err != nil || len(pre) == 0 {
		t.Errorf("the hatch overwrote the routed file without keeping a copy of it")
	}
	stray, _ := filepath.Glob(path + ".context-guru-prereset-*.json")
	if len(stray) != 0 {
		t.Errorf("left a full copy of a settings file in the project tree: %v", stray)
	}
}

// TestHatchDeletesAFileTheInstallCreated: "put it back" means DELETE when the install is the reason
// the file exists. Restoring an empty file instead would leave a stub in the user's project that
// they did not write and would have no reason to suspect.
func TestHatchDeletesAFileTheInstallCreated(t *testing.T) {
	state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
	// A canonical scope path — see the comment in TestHatchRestoresWithThePluginDeleted.
	if err := os.MkdirAll(filepath.Join(proj, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, ".claude", "settings.local.json")

	if _, code := settingsIn(t, state, home, "add", "--file", path, "--url", ourURL); code != 0 {
		t.Fatal("add failed")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fixture: the install did not create the file: %v", err)
	}
	out, code := runHatch(t, state, home, proj, "--yes")
	if code != 0 {
		t.Fatalf("hatch: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a file the install created was left behind (err=%v)", err)
	}
}

// TestHatchSecondRunChangesNothing: people re-run a recovery tool — the first run tells them to
// start a new session, and they check. The second run must be a real no-op rather than another
// copy, another backup file, and another "1 file put back".
func TestHatchSecondRunChangesNothing(t *testing.T) {
	state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
	// A canonical scope path — see the comment in TestHatchRestoresWithThePluginDeleted.
	if err := os.MkdirAll(filepath.Join(proj, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, ".claude", "settings.local.json")
	writeJSON(t, path, map[string]any{"theme": "dark"})

	if _, code := settingsIn(t, state, home, "add", "--file", path, "--url", ourURL); code != 0 {
		t.Fatal("add failed")
	}
	if _, code := runHatch(t, state, home, proj, "--yes"); code != 0 {
		t.Fatal("first hatch run failed")
	}
	recovery := filepath.Join(proj, ".claude", "context-guru-settings-json")
	before, _ := filepath.Glob(filepath.Join(recovery, "*.pre-reset-*.json"))

	out, code := runHatch(t, state, home, proj, "--yes")
	if code != 0 {
		t.Fatalf("second run: exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "already matches") && !strings.Contains(out, "Nothing to restore") {
		t.Errorf("the second run did not report itself as a no-op:\n%s", out)
	}
	after, _ := filepath.Glob(filepath.Join(recovery, "*.pre-reset-*.json"))
	if len(after) != len(before) {
		t.Errorf("the second run wrote %d more backup(s) for no reason", len(after)-len(before))
	}
}

// TestHatchDryRunWritesNothing. The routed file is what the user may still need; a preview that
// modifies it is not a preview.
func TestHatchDryRunWritesNothing(t *testing.T) {
	state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
	// A canonical scope path — see the comment in TestHatchRestoresWithThePluginDeleted.
	if err := os.MkdirAll(filepath.Join(proj, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, ".claude", "settings.local.json")
	writeJSON(t, path, map[string]any{"theme": "dark"})
	if _, code := settingsIn(t, state, home, "add", "--file", path, "--url", ourURL); code != 0 {
		t.Fatal("add failed")
	}
	routed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	out, code := runHatch(t, state, home, proj, "--dry-run")
	if code != 0 {
		t.Fatalf("dry run: exit %d\n%s", code, out)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(routed) {
		t.Errorf("--dry-run modified the file")
	}
	if !strings.Contains(out, "nothing written") {
		t.Errorf("--dry-run did not say it wrote nothing:\n%s", out)
	}
}

// TestHatchWithNoRecordTellsTheUserWhereToLook: a hand-edited settings file, or a wiped state
// directory. "Nothing to do" is the least useful thing that could be said to somebody whose
// sessions are down, so the absent-record path has to do the diagnosis a human would.
func TestHatchWithNoRecordTellsTheUserWhereToLook(t *testing.T) {
	state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
	// The real target, at the real path, because this branch's whole job is to point a user at the
	// places routing hides — and the paths it prints are project-relative.
	if err := os.MkdirAll(filepath.Join(proj, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, ".claude", "settings.local.json")
	writeJSON(t, path, map[string]any{"theme": "dark"})
	if _, code := settingsIn(t, state, home, "add", "--file", path, "--url", ourURL); code != 0 {
		t.Fatal("add failed")
	}
	hatch := hatchPath(t, state)
	// Keep the hatch, lose the record — the recovery folder is not sacred, and a user may well
	// have cleaned it out by hand (it is no longer tucked away under the state directory; it is
	// right there beside the settings file, exactly where a "clean up my project" pass would find
	// it too).
	if err := os.RemoveAll(filepath.Join(proj, ".claude", "context-guru-settings-json")); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(hatch, "--yes")
	cmd.Dir = proj
	cmd.Env = []string{"CONTEXT_GURU_STATE=" + state, "HOME=" + home, "PATH=" + os.Getenv("PATH")}
	b, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	if code != 3 {
		t.Errorf("exit %d; want 3 (finished, with something left for a human)\n%s", code, out)
	}
	for _, want := range []string{"settings.local.json", "ANTHROPIC_BASE_URL", "127.0.0.1"} {
		if !strings.Contains(out, want) {
			t.Errorf("the no-record path never mentions %q:\n%s", want, out)
		}
	}
}

// TestHatchReportsCredentialsByLocationAndNeverByValue. The incident that produced this hatch was
// not a routing fault at all: both ANTHROPIC_API_KEY and ANTHROPIC_AUTH_TOKEN were set, the gateway
// rejected whichever won, and the export was one line in a shell rc. Restoring settings cannot
// reach that, so the hatch reports it — and must do so without echoing a live key into a terminal
// buffer, a log, or a pasted transcript.
func TestHatchReportsCredentialsByLocationAndNeverByValue(t *testing.T) {
	state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
	// A canonical scope path — see the comment in TestHatchRestoresWithThePluginDeleted.
	if err := os.MkdirAll(filepath.Join(proj, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, ".claude", "settings.local.json")
	writeJSON(t, path, map[string]any{"theme": "dark"})
	if _, code := settingsIn(t, state, home, "add", "--file", path, "--url", ourURL); code != 0 {
		t.Fatal("add failed")
	}

	const secret = "sk-THIS-MUST-NEVER-BE-PRINTED"
	rc := "# a comment\nexport PATH=\"$HOME/bin:$PATH\"\nexport ANTHROPIC_API_KEY=" + secret + "\n"
	if err := os.WriteFile(filepath.Join(home, ".zshrc"), []byte(rc), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(hatchPath(t, state), "--dry-run")
	cmd.Dir = proj
	cmd.Env = []string{
		"CONTEXT_GURU_STATE=" + state,
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
		"ANTHROPIC_API_KEY=" + secret,
		"ANTHROPIC_AUTH_TOKEN=sk-a-different-one",
	}
	b, err := cmd.CombinedOutput()
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatal(err)
		}
	}
	out := string(b)
	if strings.Contains(out, secret) {
		t.Errorf("the hatch printed a credential value:\n%s", out)
	}
	if !strings.Contains(out, "BOTH set") {
		t.Errorf("the hatch did not flag two credential variables being set at once:\n%s", out)
	}
	if !strings.Contains(out, ".zshrc:3") {
		t.Errorf("the hatch did not report the file and line of the export:\n%s", out)
	}
}

// TestDeadProxyNoteNamesTheEscapeHatch. This hook fires on the prompt that is ABOUT to hang, which
// makes it the single best place to hand the user their way out — and the advice it used to end on
// was "edit the JSON by hand, or run /context-guru:uninstall from a session that still works". The
// second half is unavailable to the user this note is written for: if their projects route through
// the same dead port, no session still works. That is how the incident behind the hatch played out.
func TestDeadProxyNoteNamesTheEscapeHatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hook is POSIX-only")
	}
	requireTool(t, "bash")
	dir := t.TempDir()
	state := filepath.Join(dir, "state", "context-guru")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	hatch := filepath.Join(state, "context-guru-reset")
	if err := os.WriteFile(hatch, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A binary that cannot start, so the hook reaches its "I could not fix this" note.
	fake := filepath.Join(dir, "fake-proxy")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	port := freePort(t) // nothing listening: refused fast, so the hook goes straight to the note

	cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "check-proxy.sh"))
	cmd.Env = append(sandboxEnv(t),
		"CLAUDE_PLUGIN_ROOT="+root,
		"CLAUDE_PLUGIN_OPTION_PORT="+port,
		"ANTHROPIC_BASE_URL=http://127.0.0.1:"+port+"/anthropic",
		"CONTEXT_GURU_BIN="+fake,
		"XDG_STATE_HOME="+filepath.Join(dir, "state"),
		"TMPDIR="+dir)
	b, err := cmd.CombinedOutput()
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("running check-proxy.sh: %v (%s)", err, b)
		}
	}
	out := string(b)
	if !strings.Contains(out, hatch) {
		t.Errorf("the dead-proxy note does not name the escape hatch at %s:\n%s", hatch, out)
	}
	if !strings.Contains(out, "no\nworking Claude session") &&
		!strings.Contains(strings.Join(strings.Fields(out), " "), "no working Claude session") {
		t.Errorf("the note names the hatch without saying it works when Claude cannot:\n%s", out)
	}
}

// TestDeadProxyNoteFallsBackWhenThereIsNoHatch is the negative control for the test above: pointing
// a stuck user at a path that does not exist would be worse than the manual instructions, so the
// hatch is named only when it is on disk.
func TestDeadProxyNoteFallsBackWhenThereIsNoHatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hook is POSIX-only")
	}
	out, _, _ := runCheck(t, freePort(t), "#!/bin/sh\nexit 1\n")
	flat := strings.Join(strings.Fields(out), " ")
	if strings.Contains(flat, "context-guru-reset") {
		t.Errorf("named a hatch that is not installed:\n%s", out)
	}
	if !strings.Contains(flat, "remove env.ANTHROPIC_BASE_URL from .claude/settings.local.json") {
		t.Errorf("the fallback instructions are gone, so a user with no hatch is told nothing:\n%s", out)
	}
}

// TestAnAlreadyRoutedProjectStillGetsAHatch covers the machines that need one most: everybody who
// installed BEFORE the hatch existed. Their re-run of /context-guru:install reports
// `result=unchanged` and writes nothing, so the code path that installs the hatch never runs — and
// re-running the install is the first thing anyone does when something looks wrong.
//
// There is no pre-edit copy to invent, so the hatch must be honest rather than absent: it names the
// file, and says the original content is not recoverable from its own record.
func TestAnAlreadyRoutedProjectStillGetsAHatch(t *testing.T) {
	state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
	// A canonical scope path — see the comment in TestHatchRestoresWithThePluginDeleted. The hatch
	// now names it by this fixed RELATIVE candidate string (reset.sh's own literal), not by the
	// test's absolute path — there is no manifest entry to echo an absolute path back from anymore.
	if err := os.MkdirAll(filepath.Join(proj, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, ".claude", "settings.local.json")
	const relPath = "./.claude/settings.local.json"
	// Routed already, by a version of the plugin that kept no record — which is what an absent
	// recovery folder represents now (no `settings.py add` has ever run against this file).
	writeJSON(t, path, map[string]any{"env": map[string]any{"ANTHROPIC_BASE_URL": ourURL}})

	facts, code := settingsIn(t, state, home, "add", "--file", path, "--url", ourURL)
	if code != 0 || facts["result"] != "unchanged" {
		t.Fatalf("fixture: wanted result=unchanged exit 0, got %v exit %d", facts, code)
	}
	if facts["reset_hatch"] == "" || facts["reset_hatch"] == "unavailable" {
		t.Fatalf("a no-op re-run left the machine with no hatch: %v", facts)
	}
	if facts["recovery_original"] != "unavailable" {
		t.Errorf("recovery_original=%q; the pre-install content genuinely is not recoverable here and "+
			"claiming otherwise would promise a restore that cannot happen", facts["recovery_original"])
	}
	out, code := runHatch(t, state, home, proj, "--yes")
	if code != 3 {
		t.Errorf("hatch exit %d; want 3 — it has a file to name but cannot restore it\n%s", code, out)
	}
	if !strings.Contains(out, relPath) {
		t.Errorf("the hatch does not name the routed file:\n%s", out)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), ourURL) {
		t.Errorf("the hatch modified a file it had no pre-edit copy of")
	}
}

// TestAConflictIsNeverRecordedAsOurEdit is the guard on the branch above. `result=conflict` is
// somebody else's base URL that this script refused to touch — a corporate gateway, a benchmark
// endpoint. Recording it would tell the hatch that context-guru edited a file it never wrote to,
// and the hatch's whole safety property is that it only touches files it has a record of editing.
func TestAConflictIsNeverRecordedAsOurEdit(t *testing.T) {
	state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
	path := filepath.Join(proj, "settings.json")
	theirs := "https://gateway.corp.example/anthropic"
	writeJSON(t, path, map[string]any{"env": map[string]any{"ANTHROPIC_BASE_URL": theirs}})

	facts, code := settingsIn(t, state, home, "add", "--file", path, "--url", ourURL)
	if code != 2 || facts["result"] != "conflict" {
		t.Fatalf("fixture: wanted result=conflict exit 2, got %v exit %d", facts, code)
	}
	// A refused conflict never reaches save(), so no recovery folder should exist at all — there is
	// no manifest to check for an absent entry anymore; the folder's mere existence would BE the
	// record of an edit that never happened.
	if _, err := os.Stat(filepath.Join(proj, "context-guru-settings-json")); err == nil {
		t.Errorf("a refused conflict was recorded as our own edit: a recovery folder was created")
	}
}

// TestAddRefusesTheMachineWideFileWithoutBeingTold. The default scope was documented in the install
// skill and nowhere else — the script wrote whatever --file it was handed. A prompt is not a
// guardrail against a machine-wide lockout: it can be skipped, or read differently by the next
// model, and the cost is every Claude Code session on the machine, including the ones the user would
// use to recover. So the refusal lives in the script.
func TestAddRefusesTheMachineWideFileWithoutBeingTold(t *testing.T) {
	state, home := t.TempDir(), t.TempDir()
	userScope := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(userScope), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, userScope, map[string]any{"theme": "dark"})
	before, err := os.ReadFile(userScope)
	if err != nil {
		t.Fatal(err)
	}

	facts, code := settingsIn(t, state, home, "add", "--file", userScope, "--url", ourURL)
	if code != 2 || facts["reason"] != "user_scope_needs_flag" {
		t.Fatalf("wanted exit 2 reason=user_scope_needs_flag, got exit %d %v", code, facts)
	}
	after, err := os.ReadFile(userScope)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("the machine-wide file was modified by a call that reported refusing:\n%s", after)
	}

	// With the flag — i.e. the user asked for --global and confirmed — it goes through.
	facts, code = settingsIn(t, state, home, "add", "--file", userScope, "--url", ourURL, "--user-scope")
	if code != 0 || facts["result"] != "added" {
		t.Fatalf("--user-scope did not permit the write: exit %d %v", code, facts)
	}
	env, _ := readJSON(t, userScope)["env"].(map[string]any)
	if env["ANTHROPIC_BASE_URL"] != ourURL {
		t.Errorf("routing not written with --user-scope: %v", env)
	}
}

// TestRemoveIsNeverGatedByScope: uninstall loops over all three scopes, and it is the recovery path.
// Gating removal the way `add` is gated would make a machine-wide install unremovable by the tool
// that installed it — erring on exactly the wrong side.
func TestRemoveIsNeverGatedByScope(t *testing.T) {
	state, home := t.TempDir(), t.TempDir()
	userScope := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(userScope), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, userScope, map[string]any{"theme": "dark"})
	if _, code := settingsIn(t, state, home, "add", "--file", userScope, "--url", ourURL,
		"--user-scope"); code != 0 {
		t.Fatal("fixture: --user-scope add failed")
	}

	facts, code := settingsIn(t, state, home, "remove", "--file", userScope, "--url", ourURL)
	if code != 0 {
		t.Fatalf("remove from user scope: exit %d %v", code, facts)
	}
	env, _ := readJSON(t, userScope)["env"].(map[string]any)
	if _, still := env["ANTHROPIC_BASE_URL"]; still {
		t.Errorf("routing survived removal from the machine-wide file: %v", env)
	}
	if readJSON(t, userScope)["theme"] != "dark" {
		t.Errorf("removal damaged the machine-wide file")
	}
}

// --- persisted install-time scope: the statusline (and preset) must follow it, not guess -----
//
// Reported incident: installing context-guru on a single local project also modified and backed
// up the user's machine-wide ~/.claude/settings.json. Root cause was `install.sh` writing the
// status line at user scope UNCONDITIONALLY, regardless of what routing itself just chose. The
// fix: routing's own `add` records which scope it used (`record_install_scope`), and everything
// else that writes a settings file — the statusline, `preset`'s fallback — reads that record back
// (`resolve_install_scope` / `settings.py resolve-scope`) instead of deciding on its own.

// TestStatuslineRefusesMachineWideWithoutTheFlag is the direct regression test: a statusline-only
// add to the machine-wide file, with no --user-scope, must be refused AND must leave no trace
// beside the file — no write, no backup. A refusal that still left a backup would be the bug with
// extra steps.
func TestStatuslineRefusesMachineWideWithoutTheFlag(t *testing.T) {
	state, home := t.TempDir(), t.TempDir()
	userScope := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(userScope), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, userScope, map[string]any{"theme": "dark"})
	before, err := os.ReadFile(userScope)
	if err != nil {
		t.Fatal(err)
	}

	facts, code := settingsIn(t, state, home, "add", "--file", userScope,
		"--statusline", "/opt/cg/statusline.py")
	if code != 2 || facts["reason"] != "user_scope_needs_flag" {
		t.Fatalf("wanted exit 2 reason=user_scope_needs_flag, got exit %d %v", code, facts)
	}
	if facts["writes"] != "statusline" {
		t.Errorf("writes= should name the statusline half specifically, got %v", facts)
	}
	after, err := os.ReadFile(userScope)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("the machine-wide file was modified by a call that reported refusing:\n%s", after)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(userScope), "context-guru-settings-json",
		"settings.context-guru-backup-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("a refused statusline write left a backup behind, which is the reported bug: %v", matches)
	}

	// --user-scope still permits it, once a person has actually agreed to that blast radius.
	facts, code = settingsIn(t, state, home, "add", "--file", userScope,
		"--statusline", "/opt/cg/statusline.py", "--user-scope")
	if code != 0 || facts["result"] != "added" {
		t.Fatalf("--user-scope did not permit the statusline write: exit %d %v", code, facts)
	}
}

// TestResolveInstallScopeRoundTrips: a routing `add` records the scope it used; `resolve-scope`
// in a later, separate invocation reads it back — this is the mechanism the statusline skill and
// `preset`'s fallback are supposed to consume instead of hardcoding or re-deriving scope.
func TestResolveInstallScopeRoundTrips(t *testing.T) {
	state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
	projLocal := filepath.Join(proj, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(projLocal), 0o755); err != nil {
		t.Fatal(err)
	}

	py := requireTool(t, "python3")
	runResolve := func() map[string]string {
		cmd := exec.Command(py, filepath.Join(scriptsDir(t), "settings.py"), "resolve-scope")
		cmd.Env = append(sandboxEnv(t), "CONTEXT_GURU_STATE="+state, "HOME="+home)
		cmd.Dir = proj
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("resolve-scope failed: %v\n%s", err, out)
		}
		facts := map[string]string{}
		for _, line := range strings.Split(string(out), "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
				facts[k] = v
			}
		}
		return facts
	}

	if facts := runResolve(); facts["scope"] != "(ask)" {
		t.Fatalf("before any routing, want scope=(ask), got %v", facts)
	}

	if _, code := settingsInDir(t, state, home, proj, "add", "--file", projLocal, "--url", ourURL); code != 0 {
		t.Fatal("fixture: routing add failed")
	}
	facts := runResolve()
	if facts["scope"] != "project-local" || facts["source"] != "recorded" {
		t.Fatalf("after routing, want scope=project-local source=recorded, got %v", facts)
	}
	if facts["file"] != projLocal {
		t.Errorf("file = %v, want %v", facts["file"], projLocal)
	}

	// A second, unrelated project directory that was never routed at all still says "ask" — the
	// record is per-project, not global.
	otherProj := t.TempDir()
	cmd := exec.Command(py, filepath.Join(scriptsDir(t), "settings.py"), "resolve-scope")
	cmd.Env = append(sandboxEnv(t), "CONTEXT_GURU_STATE="+state, "HOME="+home)
	cmd.Dir = otherProj
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("resolve-scope failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "scope=(ask)") {
		t.Errorf("an unrelated project should not inherit another project's recorded scope:\n%s", out)
	}
}

// TestResolveInstallScopeSelfHealsALegacyProject: a project routed by hand, or by a version of
// this plugin that predates the persisted record, has routing but no entry in install-scope.json.
// resolve-scope must find it by checking which candidate file actually owns routing, answer
// correctly, and persist that answer so the next call is a plain lookup rather than a repeat scan.
func TestResolveInstallScopeSelfHealsALegacyProject(t *testing.T) {
	state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
	projLocal := filepath.Join(proj, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(projLocal), 0o755); err != nil {
		t.Fatal(err)
	}
	// Written directly, bypassing `add`, so no install-scope.json entry exists — exactly what a
	// pre-existing install (or a hand-edited file) looks like.
	writeJSON(t, projLocal, map[string]any{
		"env":           map[string]any{"ANTHROPIC_BASE_URL": ourURL},
		"$context-guru": map[string]any{"installed_base_url": ourURL},
	})

	py := requireTool(t, "python3")
	run := func() map[string]string {
		cmd := exec.Command(py, filepath.Join(scriptsDir(t), "settings.py"), "resolve-scope")
		cmd.Env = append(sandboxEnv(t), "CONTEXT_GURU_STATE="+state, "HOME="+home)
		cmd.Dir = proj
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("resolve-scope failed: %v\n%s", err, out)
		}
		facts := map[string]string{}
		for _, line := range strings.Split(string(out), "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
				facts[k] = v
			}
		}
		return facts
	}

	facts := run()
	if facts["scope"] != "project-local" || facts["source"] != "inferred" {
		t.Fatalf("want scope=project-local source=inferred on first call, got %v", facts)
	}
	if facts := run(); facts["source"] != "recorded" {
		t.Errorf("the self-heal should have persisted; want source=recorded on the second call, got %v",
			facts)
	}
}

// TestInstallScopeMigratesALegacyKeyToProjectKey: install-scope.json records were keyed on
// realpath(cwd) before project_key() existed. A record still filed under that old key must be
// found, rewritten under the NEW key, and dropped from the old one — the same self-healing shape
// resolve_install_scope already uses for a project with no record at all (see
// TestResolveInstallScopeSelfHealsALegacyProject above), so a pre-existing install stays visible
// to every project_key()-keyed lookup instead of quietly becoming invisible to it.
//
// The case that actually exercises the migration is a git WORKTREE: project_key() resolves a
// worktree's directory to its MAIN checkout, which differs from realpath(worktree) — the old key
// — and is exactly the situation a record predating project_key() would be in.
func TestInstallScopeMigratesALegacyKeyToProjectKey(t *testing.T) {
	requireTool(t, "git")
	state, home, root := t.TempDir(), t.TempDir(), t.TempDir()

	runGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v (dir=%s): %v\n%s", args, dir, err, out)
		}
	}
	realpath := func(p string) string {
		t.Helper()
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatalf("EvalSymlinks(%s): %v", p, err)
		}
		return r
	}

	mainRepo := filepath.Join(root, "main")
	if err := os.MkdirAll(mainRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(mainRepo, "init", "-q")
	runGit(mainRepo, "-c", "user.email=t@example.com", "-c", "user.name=t",
		"commit", "--allow-empty", "-q", "-m", "init")
	worktree := filepath.Join(root, "wt")
	runGit(mainRepo, "worktree", "add", "-q", worktree, "-b", "wt-branch")

	projLocal := filepath.Join(worktree, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(projLocal), 0o755); err != nil {
		t.Fatal(err)
	}

	oldKey := realpath(worktree)
	newKey := realpath(mainRepo)

	// Seed install-scope.json exactly as a pre-project_key() version of this plugin would have
	// left it: keyed on realpath(worktree), the OLD identity — never on the main checkout.
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(state, "install-scope.json"), map[string]any{
		"version": 1,
		"projects": map[string]any{
			oldKey: map[string]any{
				"scope":       "project-local",
				"file":        projLocal,
				"recorded_at": "2020-01-01T00:00:00Z",
			},
		},
	})

	facts, code := settingsInDir(t, state, home, worktree, "resolve-scope")
	if code != 0 {
		t.Fatalf("resolve-scope failed: exit %d %v", code, facts)
	}
	if facts["scope"] != "project-local" || facts["source"] != "recorded" {
		t.Fatalf("want scope=project-local source=recorded (migrated), got %v", facts)
	}

	scopes := readJSON(t, filepath.Join(state, "install-scope.json"))
	projects, _ := scopes["projects"].(map[string]any)
	if _, stillOld := projects[oldKey]; stillOld {
		t.Errorf("the old, pre-project_key() key survived migration: %v", projects)
	}
	if _, hasNew := projects[newKey]; !hasNew {
		t.Errorf("no record under the new project_key() (main checkout) after migration: %v", projects)
	}
	if len(projects) != 1 {
		t.Errorf("migration should REWRITE the one record, not add a second: %v", projects)
	}

	// And a worktree with no record at all resolves to the SAME key as its main checkout, so a
	// second worktree of the same repo inherits the first worktree's (now-migrated) routing.
	worktree2 := filepath.Join(root, "wt2")
	runGit(mainRepo, "worktree", "add", "-q", worktree2, "-b", "wt2-branch")
	facts2, code2 := settingsInDir(t, state, home, worktree2, "resolve-scope")
	if code2 != 0 {
		t.Fatalf("resolve-scope from second worktree failed: exit %d %v", code2, facts2)
	}
	if facts2["scope"] != "project-local" || facts2["source"] != "recorded" {
		t.Errorf("a second worktree of the same repo should see the migrated record too, got %v", facts2)
	}
}

// TestProjectKeyForAPlainCheckoutIsItsOwnDirectory exercises the non-worktree branch of
// project_key(): a plain checkout's `--git-common-dir` IS its own `.git`, so `_resolve_project_key`
// must key it as ITSELF, not as some ancestor. This is the case the bare (non `--path-format`)
// fallback can get wrong: `git rev-parse --git-common-dir` from inside a plain checkout answers
// the RELATIVE path `.git`, and naively taking `dirname(".git")` gives `"."`, which resolves
// against the WRONG base if not joined against the checkout dir first — collapsing every plain
// checkout on the machine toward its parent directory instead of keying each one separately.
func TestProjectKeyForAPlainCheckoutIsItsOwnDirectory(t *testing.T) {
	requireTool(t, "git")
	state, home, root := t.TempDir(), t.TempDir(), t.TempDir()

	runGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v (dir=%s): %v\n%s", args, dir, err, out)
		}
	}
	realpath := func(p string) string {
		t.Helper()
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatalf("EvalSymlinks(%s): %v", p, err)
		}
		return r
	}

	// A plain checkout, deliberately nested a few levels under `root` — if the key ever collapsed
	// to a parent directory, it would collapse to one of THESE, not to the repo itself.
	repo := filepath.Join(root, "some", "nested", "path", "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(repo, "init", "-q")
	runGit(repo, "-c", "user.email=t@example.com", "-c", "user.name=t",
		"commit", "--allow-empty", "-q", "-m", "init")

	projLocal := filepath.Join(repo, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(projLocal), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, projLocal, map[string]any{
		"env":           map[string]any{"ANTHROPIC_BASE_URL": ourURL},
		"$context-guru": map[string]any{"installed_base_url": ourURL},
	})

	facts, code := settingsInDir(t, state, home, repo, "resolve-scope")
	if code != 0 {
		t.Fatalf("resolve-scope failed: exit %d %v", code, facts)
	}
	if facts["scope"] != "project-local" || facts["source"] != "inferred" {
		t.Fatalf("want scope=project-local source=inferred, got %v", facts)
	}

	scopes := readJSON(t, filepath.Join(state, "install-scope.json"))
	projects, _ := scopes["projects"].(map[string]any)
	if _, hasSelf := projects[realpath(repo)]; !hasSelf {
		t.Errorf("a plain checkout must be keyed as ITSELF, got keys: %v", projects)
	}
	for key := range projects {
		if key != realpath(repo) {
			t.Errorf("plain checkout keyed as an ancestor directory instead of itself: %q (want %q)",
				key, realpath(repo))
		}
	}
}

// TestProjectKeyFailsOpenWithoutGitBinary: project_key() must fail open to realpath(dir), never
// raise, when git is not on PATH at all — this is reached from hook-time code paths (every
// SessionStart), where a missing interpreter must degrade to "not worktree-aware" rather than
// break the session.
func TestProjectKeyFailsOpenWithoutGitBinary(t *testing.T) {
	py := requireTool(t, "python3")
	state, home, dir := t.TempDir(), t.TempDir(), t.TempDir()

	projLocal := filepath.Join(dir, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(projLocal), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, projLocal, map[string]any{
		"env":           map[string]any{"ANTHROPIC_BASE_URL": ourURL},
		"$context-guru": map[string]any{"installed_base_url": ourURL},
	})

	env := append(sandboxEnv(t), "CONTEXT_GURU_STATE="+state, "HOME="+home)
	for i, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			// No `git` reachable from here at all — not merely absent from a candidate directory.
			env[i] = "PATH=/nonexistent-bin-dir-for-this-test"
		}
	}

	cmd := exec.Command(py, filepath.Join(scriptsDir(t), "settings.py"), "resolve-scope")
	cmd.Env = env
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		t.Fatalf("resolve-scope raised/failed with no git on PATH: exit %d\n%s", ee.ExitCode(), out)
	} else if err != nil {
		t.Fatalf("running settings.py: %v (%s)", err, out)
	}

	facts := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			facts[k] = v
		}
	}
	if facts["scope"] != "project-local" || facts["source"] != "inferred" {
		t.Fatalf("want scope=project-local source=inferred (no git present, fail-open to realpath), got %v", facts)
	}
}

// TestMigrationTiebreakPrefersTheStillRoutingWorktree: project_key() collapses every worktree of
// one repo onto the SAME key, but each worktree kept its own pre-migration install-scope.json
// record (one per realpath(worktree)). Those records migrate lazily, one project_dir at a time —
// so it is possible for the new key to already hold a migrated record (written by an earlier call
// for a DIFFERENT worktree) while THIS worktree's own legacy record is still unmigrated. Whichever
// call happened to run first must not win permanently just because it ran first: the record that
// is actually routing the project must win, not the one that was resolved first.
func TestMigrationTiebreakPrefersTheStillRoutingWorktree(t *testing.T) {
	requireTool(t, "git")
	state, home, root := t.TempDir(), t.TempDir(), t.TempDir()

	runGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v (dir=%s): %v\n%s", args, dir, err, out)
		}
	}
	realpath := func(p string) string {
		t.Helper()
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatalf("EvalSymlinks(%s): %v", p, err)
		}
		return r
	}

	mainRepo := filepath.Join(root, "main")
	if err := os.MkdirAll(mainRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(mainRepo, "init", "-q")
	runGit(mainRepo, "-c", "user.email=t@example.com", "-c", "user.name=t",
		"commit", "--allow-empty", "-q", "-m", "init")

	wtStale := filepath.Join(root, "wt-stale")
	wtLive := filepath.Join(root, "wt-live")
	runGit(mainRepo, "worktree", "add", "-q", wtStale, "-b", "stale-branch")
	runGit(mainRepo, "worktree", "add", "-q", wtLive, "-b", "live-branch")

	// The STALE worktree's settings file no longer carries our routing — abandoned, exactly like
	// a worktree whose install was later removed or overwritten by hand.
	staleLocal := filepath.Join(wtStale, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(staleLocal), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, staleLocal, map[string]any{"env": map[string]any{}})

	// The LIVE worktree's settings file still actually routes to us.
	liveLocal := filepath.Join(wtLive, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(liveLocal), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, liveLocal, map[string]any{
		"env":           map[string]any{"ANTHROPIC_BASE_URL": ourURL},
		"$context-guru": map[string]any{"installed_base_url": ourURL},
	})

	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	// Seed BOTH pre-migration records, each under its OWN worktree's old key — as if each
	// worktree had been installed separately, before project_key() existed. The stale one's
	// recorded_at is deliberately the NEWER of the two, so a naive "prefer recorded_at" tiebreak
	// (or a naive "whoever migrates first wins") would pick the wrong one.
	writeJSON(t, filepath.Join(state, "install-scope.json"), map[string]any{
		"version": 1,
		"projects": map[string]any{
			realpath(wtStale): map[string]any{
				"scope":       "project-local",
				"file":        staleLocal,
				"recorded_at": "2020-06-01T00:00:00Z",
			},
			realpath(wtLive): map[string]any{
				"scope":       "project-local",
				"file":        liveLocal,
				"recorded_at": "2020-01-01T00:00:00Z",
			},
		},
	})

	// Resolve from the STALE worktree FIRST — this is the race the tiebreak has to survive: the
	// stale worktree's own call is the first to touch the shared new_key.
	factsStale, code := settingsInDir(t, state, home, wtStale, "resolve-scope")
	if code != 0 {
		t.Fatalf("resolve-scope (stale) failed: exit %d %v", code, factsStale)
	}
	// The FIRST call must already have collapsed the file: the tiebreak's LOSERS are removed in
	// the same write, not left for whichever sibling happens to be resolved from next. Asserted
	// here and not only at the end because the end state is reachable either way — the live
	// worktree's own call below drops its own old key regardless. A loser left behind is not
	// cosmetic: resolve_install_scope re-probes every surviving sibling key on every call, so it
	// costs a `git` fork on every SessionStart, and `port alloc` reads its port as belonging to
	// another project and skips it.
	if projects, _ := readJSON(t, filepath.Join(state, "install-scope.json"))["projects"].(map[string]any); len(projects) != 1 {
		t.Fatalf("the first resolve must drop the tiebreak losers, leaving one record; got: %v", projects)
	}

	factsLive, code := settingsInDir(t, state, home, wtLive, "resolve-scope")
	if code != 0 {
		t.Fatalf("resolve-scope (live) failed: exit %d %v", code, factsLive)
	}

	if factsStale["file"] != liveLocal || factsLive["file"] != liveLocal {
		t.Fatalf("both worktrees must resolve to the STILL-ROUTING file, got stale=%v live=%v",
			factsStale, factsLive)
	}

	scopes := readJSON(t, filepath.Join(state, "install-scope.json"))
	projects, _ := scopes["projects"].(map[string]any)
	if len(projects) != 1 {
		t.Fatalf("the tiebreak must leave exactly one record for the repo, got: %v", projects)
	}
	newKey := realpath(mainRepo)
	rec, _ := projects[newKey].(map[string]any)
	if rec == nil || rec["file"] != liveLocal {
		t.Errorf("the surviving record must point at the still-routing file, got: %v", projects)
	}
}

// --- per-project port allocation (`settings.py port alloc|show|release`) ----------------------
//
// The port used to default to 8787 for every project, so two projects with different presets
// shared one proxy process and start-proxy.sh's fingerprint check "resolved" the disagreement by
// killing whichever proxy did not match — wiping the in-memory cache/store on every flip. These
// tests cover the allocation rules in isolation; TestStartProxyDoesNotStopAnotherProjectsProxy
// (Phase 3) covers the actual proxy-killing regression this exists to remove.
//
// Every test below that actually SCANS for a port (as opposed to `--dry-run`, `show` or an
// explicitly-configured port, none of which bind anything) pins `CONTEXT_GURU_PORT_BASE` to a
// high, test-specific base via `portScanBase`, rather than letting the scan start at the real
// default of 8787. `go test` for this package runs on a box SHARED with another engineer's live
// services and other Claude sessions as the same unix user — binding the plain 8787..8850 range
// for real would be indistinguishable from a genuine port squat, and would make this suite fail
// for a reason that has nothing to do with the code under test if someone else already holds one
// of those ports. 8787 itself is asserted only as the *default value* of the unpinned constant, in
// TestPluginJSONPortDefaultAgreesWithTheScript below, which never binds anything.
var portTestBaseCounter atomic.Int64

// portScanBase pins CONTEXT_GURU_PORT_BASE to a base this test alone uses (t.Setenv, so it is
// restored automatically and cannot leak into another test), high enough to stay well clear of
// any real service's ports on a shared box, and counted up per call so tests in this same package
// that both scan for a port cannot collide with EACH OTHER either.
func portScanBase(t *testing.T) {
	t.Helper()
	base := 39000 + int(portTestBaseCounter.Add(1))*100
	t.Setenv("CONTEXT_GURU_PORT_BASE", strconv.Itoa(base))
}

// TestPortAllocAssignsDistinctPortsAndIsIdempotent: two fresh projects get two different ports,
// scanning up from the default; re-running `alloc` for either one returns the SAME port rather
// than reallocating — reinstall, update and repair must never move a project's port, because the
// URL naming it is already written into a settings file.
func TestPortAllocAssignsDistinctPortsAndIsIdempotent(t *testing.T) {
	portScanBase(t)
	state, home := t.TempDir(), t.TempDir()
	projA, projB := t.TempDir(), t.TempDir()
	for _, p := range []string{projA, projB} {
		if err := os.MkdirAll(filepath.Join(p, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	factsA, code := settingsInDir(t, state, home, projA, "port", "alloc")
	if code != 0 || factsA["source"] != "allocated" {
		t.Fatalf("project A alloc: exit %d %v", code, factsA)
	}
	factsB, code := settingsInDir(t, state, home, projB, "port", "alloc")
	if code != 0 || factsB["source"] != "allocated" {
		t.Fatalf("project B alloc: exit %d %v", code, factsB)
	}
	if factsA["port"] == factsB["port"] {
		t.Fatalf("two distinct projects were allocated the SAME port: %v / %v", factsA, factsB)
	}

	// Re-running alloc for A must return exactly the port it already has, sourced as "recorded"
	// rather than scanned again.
	again, code := settingsInDir(t, state, home, projA, "port", "alloc")
	if code != 0 {
		t.Fatalf("re-alloc for A failed: exit %d %v", code, again)
	}
	if again["port"] != factsA["port"] || again["source"] != "recorded" {
		t.Errorf("re-alloc moved project A's port: first %v, second %v", factsA, again)
	}

	// `port show` agrees with what `alloc` recorded.
	shown, code := settingsInDir(t, state, home, projA, "port", "show")
	if code != 0 || shown["port"] != factsA["port"] {
		t.Errorf("port show disagrees with alloc: show=%v alloc=%v", shown, factsA)
	}

	// The port landed in the project's own settings.local.json as a JSON NUMBER, never a string —
	// plugin.json types `port` as "number", and CLAUDE_PLUGIN_OPTION_PORT downstream expects one.
	got := readJSON(t, filepath.Join(projA, ".claude", "settings.local.json"))
	opts, _ := ((got["pluginConfigs"].(map[string]any))["context-guru@context-guru"].(map[string]any))["options"].(map[string]any)
	portVal, ok := opts["port"]
	if !ok {
		t.Fatalf("port was not written into pluginConfigs.options: %v", got)
	}
	if _, isFloat := portVal.(float64); !isFloat {
		t.Errorf("port in settings.local.json is not a JSON number: %T %v", portVal, portVal)
	}
	if s, isString := portVal.(string); isString {
		t.Errorf("port in settings.local.json was written as a STRING (%q), not a number", s)
	}
}

// TestPortAllocHonoursAnExplicitlyConfiguredPort: a project that already has `pluginConfigs`
// options.port set (by hand, or via /plugin configure before install) must have that value
// honoured and recorded, never overwritten by the scan — and never reallocated even though the
// scan would otherwise have started from the same default.
func TestPortAllocHonoursAnExplicitlyConfiguredPort(t *testing.T) {
	state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
	projLocal := filepath.Join(proj, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(projLocal), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, projLocal, map[string]any{
		"pluginConfigs": map[string]any{
			"context-guru@context-guru": map[string]any{
				"options": map[string]any{"port": 9999},
			},
		},
	})

	facts, code := settingsInDir(t, state, home, proj, "port", "alloc")
	if code != 0 {
		t.Fatalf("alloc failed: exit %d %v", code, facts)
	}
	if facts["port"] != "9999" || facts["source"] != "configured" {
		t.Fatalf("want port=9999 source=configured, got %v", facts)
	}

	// The file is not rewritten with a second, redundant copy of the same value.
	before, err := os.ReadFile(projLocal)
	if err != nil {
		t.Fatal(err)
	}
	if _, code := settingsInDir(t, state, home, proj, "port", "alloc"); code != 0 {
		t.Fatal("second alloc failed")
	}
	after, err := os.ReadFile(projLocal)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("an already-explicit port was rewritten:\nbefore: %s\nafter:  %s", before, after)
	}
}

// TestPortAllocDryRunWritesNothing: install.sh's `--plan` must be able to preview the port that
// would be used without allocating it for real — an allocation made during a plan the user never
// confirms would take a port out of the pool (and write a settings file) before consent was ever
// asked.
func TestPortAllocDryRunWritesNothing(t *testing.T) {
	portScanBase(t)
	state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(proj, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	facts, code := settingsInDir(t, state, home, proj, "port", "alloc", "--dry-run")
	if code != 0 || facts["port"] == "" {
		t.Fatalf("dry-run alloc failed: exit %d %v", code, facts)
	}
	if _, err := os.Stat(filepath.Join(state, "install-scope.json")); !os.IsNotExist(err) {
		t.Errorf("dry-run wrote install-scope.json, which must not happen until the real run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(proj, ".claude", "settings.local.json")); !os.IsNotExist(err) {
		t.Errorf("dry-run wrote a settings file, which must not happen until the real run: %v", err)
	}

	shown, code := settingsInDir(t, state, home, proj, "port", "show")
	if code != 0 || shown["port"] != "(none)" {
		t.Errorf("dry-run left a port recorded for `show` to find: %v", shown)
	}
}

// TestPortReleaseFreesTheProjectsPortForReuse: uninstall's call. Releasing project A's port must
// remove ONLY A's record, leave B's untouched, and free A's port number for a later scan (project
// C, allocated after the release, may land on it).
func TestPortReleaseFreesTheProjectsPortForReuse(t *testing.T) {
	portScanBase(t)
	state, home := t.TempDir(), t.TempDir()
	projA, projB := t.TempDir(), t.TempDir()
	for _, p := range []string{projA, projB} {
		if err := os.MkdirAll(filepath.Join(p, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	factsA, code := settingsInDir(t, state, home, projA, "port", "alloc")
	if code != 0 {
		t.Fatalf("alloc A failed: exit %d %v", code, factsA)
	}
	factsB, code := settingsInDir(t, state, home, projB, "port", "alloc")
	if code != 0 {
		t.Fatalf("alloc B failed: exit %d %v", code, factsB)
	}

	rel, code := settingsInDir(t, state, home, projA, "port", "release")
	if code != 0 || rel["result"] != "released" || rel["port"] != factsA["port"] {
		t.Fatalf("release A: exit %d %v (want released port=%s)", code, rel, factsA["port"])
	}

	// B's record survived untouched.
	shownB, code := settingsInDir(t, state, home, projB, "port", "show")
	if code != 0 || shownB["port"] != factsB["port"] {
		t.Errorf("releasing A disturbed B's record: %v", shownB)
	}
	shownA, code := settingsInDir(t, state, home, projA, "port", "show")
	if code != 0 || shownA["port"] != "(none)" {
		t.Errorf("A's port was not actually released: %v", shownA)
	}

	// A second release of an already-released project is a harmless no-op, not an error.
	relAgain, code := settingsInDir(t, state, home, projA, "port", "release")
	if code != 0 || relAgain["result"] != "unchanged" {
		t.Errorf("re-releasing an unrecorded project should be result=unchanged, got exit %d %v",
			code, relAgain)
	}
}

// TestPortReleaseWorksWithoutGitBinary: `release` must be reachable, and must free the RIGHT
// project's slot, even with no `git` on PATH at all — project_key() fails open to realpath(dir)
// in that case, and release must key off exactly the same identity `alloc` used, or it would free
// the wrong project's port (or none at all) on a machine where git is missing.
// TestPortAllocReusesThePortOfADeletedProject: `release` is not the only way a project ends. A
// checkout that is simply deleted leaves its record behind — and since the scan range is only 64
// ports wide and nothing else ever frees a record, a held-forever port per abandoned clone shrinks
// the pool until `alloc` reports no_port_available on a machine where nothing is listening on any
// of them. Reuse is safe because it is not the only guard: _port_bindable still refuses any port
// something is actually serving, so a live proxy can never be taken this way.
func TestPortAllocReusesThePortOfADeletedProject(t *testing.T) {
	portScanBase(t)
	state, home := t.TempDir(), t.TempDir()
	gone, keep, fresh := t.TempDir(), t.TempDir(), t.TempDir()
	for _, d := range []string{gone, keep, fresh} {
		if err := os.MkdirAll(filepath.Join(d, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	goneFacts, code := settingsInDir(t, state, home, gone, "port", "alloc")
	if code != 0 || goneFacts["source"] != "allocated" {
		t.Fatalf("first alloc: exit %d %v", code, goneFacts)
	}
	// CONTROL, and it has to come first: while that directory still exists its port must NOT be
	// reused. Without this, a broken implementation that ignored recorded ports entirely — handing
	// the same port to everyone — would satisfy the assertion below.
	keepFacts, code := settingsInDir(t, state, home, keep, "port", "alloc")
	if code != 0 {
		t.Fatalf("second alloc: exit %d %v", code, keepFacts)
	}
	if keepFacts["port"] == goneFacts["port"] {
		t.Fatalf("a LIVE project's recorded port was handed out again: %v / %v", goneFacts, keepFacts)
	}

	// Resolved BEFORE the directory goes: the record is keyed on the resolved path, and afterwards
	// there is nothing left to resolve.
	goneReal, err := filepath.EvalSymlinks(gone)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	freshFacts, code := settingsInDir(t, state, home, fresh, "port", "alloc")
	if code != 0 {
		t.Fatalf("alloc after the project was deleted: exit %d %v", code, freshFacts)
	}
	if freshFacts["port"] != goneFacts["port"] {
		t.Errorf("the deleted project still holds port %s out of the pool (a new project got %s "+
			"instead); every abandoned checkout would cost a port permanently",
			goneFacts["port"], freshFacts["port"])
	}
	// And the orphan record goes WITH the port, in that same write. Leaving it was the first shape
	// of this fix and it was wrong: `alloc` rule 1 hands a recorded port straight back, so a
	// directory that came back — a re-clone at a path used before, a volume remounted — got a record
	// naming a port since reissued to somebody else, which is two projects on one port, the exact
	// defect this whole change exists to remove. Nothing is lost by dropping it: alloc wrote
	// `options.port` into that project's own settings file, so if it returns, rule 2 (`configured`)
	// hands it the same port back without the record.
	// Checked against the EXACT key, not a prefix: every project in this test is a sibling under one
	// temp root, so a prefix match would be satisfied by the other two and prove nothing.
	// settingsInDir sets CONTEXT_GURU_STATE directly, so `state` IS the state dir — no
	// `context-guru/` segment, unlike the route tests, which set XDG_STATE_HOME instead.
	scopes := readJSON(t, filepath.Join(state, "install-scope.json"))
	projects, _ := scopes["projects"].(map[string]any)
	if _, kept := projects[goneReal]; kept {
		t.Errorf("the deleted project's record (%s) survived the reissue of its port, so two "+
			"projects are now recorded on port %s: %v", goneReal, freshFacts["port"], projects)
	}
	// CONTROL for that assertion: the LIVE project's record must still be there. A prune that
	// emptied the file would satisfy the check above while destroying every install on the machine.
	keepReal, err := filepath.EvalSymlinks(keep)
	if err != nil {
		t.Fatal(err)
	}
	if _, kept := projects[keepReal]; !kept {
		t.Errorf("alloc dropped a LIVE project's record (%s): %v", keepReal, projects)
	}
}

// TestPortAllocRefusesAPortTwoProjectsRecorded: rule 1 of `alloc` returns a port already recorded
// for this project UNCHANGED, which is right — the URL naming it is already written into a settings
// file, and moving it would strand that file — but it must VERIFY rather than assume. A port
// recorded for two projects whose directories both exist is reachable from a hand-edited
// install-scope.json or a restored backup, and no allocation can resolve it: both records are
// equally real, and picking one silently is how two projects end up sharing a proxy with different
// presets.
//
// `port=` is still emitted on the refusal, deliberately: install.sh's plan path falls back to
// `option_port` and then 8787 when it cannot read a port, and 8787 is now a DIFFERENT project's
// proxy — so a refusal that withheld the number would cause the collision it warns about.
func TestPortAllocRefusesAPortTwoProjectsRecorded(t *testing.T) {
	portScanBase(t)
	state, home := t.TempDir(), t.TempDir()
	a, b := t.TempDir(), t.TempDir()
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(filepath.Join(d, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	aFacts, code := settingsInDir(t, state, home, a, "port", "alloc")
	if code != 0 {
		t.Fatalf("alloc A: exit %d %v", code, aFacts)
	}
	if _, code := settingsInDir(t, state, home, b, "port", "alloc"); code != 0 {
		t.Fatalf("alloc B: exit %d", code)
	}
	aReal, err := filepath.EvalSymlinks(a)
	if err != nil {
		t.Fatal(err)
	}
	bReal, err := filepath.EvalSymlinks(b)
	if err != nil {
		t.Fatal(err)
	}
	// Put B onto A's port behind alloc's back, the way a restored backup or a hand edit would.
	scopes := readJSON(t, filepath.Join(state, "install-scope.json"))
	projects, _ := scopes["projects"].(map[string]any)
	brec, _ := projects[bReal].(map[string]any)
	arec, _ := projects[aReal].(map[string]any)
	if brec == nil || arec == nil {
		t.Fatalf("both records should exist: %v", projects)
	}
	brec["port"] = arec["port"]
	writeJSON(t, filepath.Join(state, "install-scope.json"), scopes)

	facts, code := settingsInDir(t, state, home, b, "port", "alloc")
	if code == 0 {
		t.Fatalf("alloc handed back a port another live project also has recorded: %v", facts)
	}
	if facts["reason"] != "port_recorded_by_another_project" {
		t.Errorf("reason=%q, want port_recorded_by_another_project: %v", facts["reason"], facts)
	}
	if facts["other_project"] != aReal {
		t.Errorf("the refusal does not name the other project (%s): %v", aReal, facts)
	}
	if facts["port"] != aFacts["port"] {
		t.Errorf("the refusal withheld the port, so a caller falling open lands on 8787 — which is "+
			"now somebody else's proxy: port=%q want %q", facts["port"], aFacts["port"])
	}
}

func TestPortReleaseWorksWithoutGitBinary(t *testing.T) {
	portScanBase(t)
	py := requireTool(t, "python3")
	state, home := t.TempDir(), t.TempDir()
	projA, projB := t.TempDir(), t.TempDir()
	for _, p := range []string{projA, projB} {
		if err := os.MkdirAll(filepath.Join(p, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	env := append(sandboxEnv(t), "CONTEXT_GURU_STATE="+state, "HOME="+home)
	for i, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			env[i] = "PATH=/nonexistent-bin-dir-for-this-test"
		}
	}
	run := func(dir string, args ...string) map[string]string {
		t.Helper()
		cmd := exec.Command(py, append([]string{filepath.Join(scriptsDir(t), "settings.py")}, args...)...)
		cmd.Env = env
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("settings.py %v (dir=%s) failed with no git on PATH: %v\n%s", args, dir, err, out)
		}
		facts := map[string]string{}
		for _, line := range strings.Split(string(out), "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
				facts[k] = v
			}
		}
		return facts
	}

	factsA := run(projA, "port", "alloc")
	factsB := run(projB, "port", "alloc")
	if factsA["port"] == "" || factsB["port"] == "" || factsA["port"] == factsB["port"] {
		t.Fatalf("alloc without git did not give two distinct ports: A=%v B=%v", factsA, factsB)
	}

	rel := run(projA, "port", "release")
	if rel["result"] != "released" || rel["port"] != factsA["port"] {
		t.Fatalf("release without git: want released port=%s, got %v", factsA["port"], rel)
	}
	shownB := run(projB, "port", "show")
	if shownB["port"] != factsB["port"] {
		t.Errorf("releasing A (no git) disturbed B's record: %v", shownB)
	}
	shownA := run(projA, "port", "show")
	if shownA["port"] != "(none)" {
		t.Errorf("A's port was not actually released (no git): %v", shownA)
	}
}

// TestPortAllocRefusesToWriteADifferentPortThanThePlanPromised: install.sh's --plan does a
// --dry-run alloc to preview the URL it asks the user to confirm. If the REAL alloc that follows
// consent would pick a DIFFERENT port — someone else took the previewed one in the gap — writing
// that different port into the URL would route the user to a port they never agreed to see.
// plugin.json says the port "must be FIXED rather than negotiated"; silently substituting one
// after consent was given is exactly the negotiation that forbids. The real alloc must refuse.
func TestPortAllocRefusesToWriteADifferentPortThanThePlanPromised(t *testing.T) {
	portScanBase(t)
	state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(proj, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	planned, code := settingsInDir(t, state, home, proj, "port", "alloc", "--dry-run")
	if code != 0 || planned["port"] == "" {
		t.Fatalf("planning dry-run failed: exit %d %v", code, planned)
	}

	// Simulate the gap between the plan and the real alloc: an unrelated project's OWN alloc
	// (a genuinely different install racing this one, not a hand-edited fixture) records the
	// exact port `proj` was promised, so `proj`'s real alloc's collision-avoiding scan is forced
	// away from it.
	other := t.TempDir()
	if err := os.MkdirAll(filepath.Join(other, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	promisedPort, err := strconv.Atoi(planned["port"])
	if err != nil {
		t.Fatalf("planned port %q is not numeric: %v", planned["port"], err)
	}
	// `install-scope.json` does not exist yet — the plan above was a `--dry-run`, which writes
	// nothing (TestPortAllocDryRunWritesNothing covers that directly) — so this is the file's
	// first real write, seeding it with only the colliding project's record.
	writeJSON(t, filepath.Join(state, "install-scope.json"), map[string]any{
		"version": 1,
		"projects": map[string]any{
			other: map[string]any{
				"scope": "custom", "file": filepath.Join(other, ".claude", "settings.local.json"),
				"recorded_at": "2020-01-01T00:00:00Z", "port": promisedPort,
			},
		},
	})

	facts, code := settingsInDir(t, state, home, proj, "port", "alloc", "--promised-port", planned["port"])
	if code == 0 {
		t.Fatalf("real alloc succeeded despite the promised port now being taken by another project: %v", facts)
	}
	if facts["reason"] != "port_changed_since_plan" {
		t.Fatalf("want reason=port_changed_since_plan, got exit %d %v", code, facts)
	}
	if _, err := os.Stat(filepath.Join(proj, ".claude", "settings.local.json")); !os.IsNotExist(err) {
		t.Errorf("the refused alloc wrote a settings file anyway: %v", err)
	}

	// The happy path: when the promised port is STILL free, passing --promised-port changes
	// nothing.
	proj2 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(proj2, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	planned2, code := settingsInDir(t, state, home, proj2, "port", "alloc", "--dry-run")
	if code != 0 {
		t.Fatalf("second plan failed: exit %d %v", code, planned2)
	}
	real2, code := settingsInDir(t, state, home, proj2, "port", "alloc", "--promised-port", planned2["port"])
	if code != 0 || real2["port"] != planned2["port"] {
		t.Fatalf("a still-valid promised port should be honoured, got exit %d %v (planned %v)",
			code, real2, planned2)
	}
}

// TestPluginJSONPortDefaultAgreesWithTheScript is a drift guard in the shape of
// TestPluginJSONCacheStrategyAgreesWithTheScript: plugin.json's `port.default` (what the user's
// settings UI shows) and settings.py's PORT_SCAN_START (what the scan actually starts from) are
// two sources of truth for the same number, and a drift between them is invisible until a project
// lands on a port the UI never mentioned. This never binds a socket: it reads PORT_SCAN_START out
// of the module without running `main()`, via `runpy.run_path` under a run_name that is not
// `__main__` so the script's own `if __name__ == "__main__": sys.exit(main())` never fires.
func TestPluginJSONPortDefaultAgreesWithTheScript(t *testing.T) {
	py := requireTool(t, "python3")

	b, err := os.ReadFile(filepath.Join(".claude-plugin", "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		UserConfig map[string]struct {
			Default any `json:"default"`
		} `json:"userConfig"`
	}
	if err := json.Unmarshal(b, &manifest); err != nil {
		t.Fatalf("plugin.json does not parse: %v", err)
	}
	portDefault, ok := manifest.UserConfig["port"]
	if !ok {
		t.Fatal("plugin.json has no userConfig.port entry")
	}
	wantFloat, ok := portDefault.Default.(float64)
	if !ok {
		t.Fatalf("plugin.json's port default is not a number: %v", portDefault.Default)
	}

	code := fmt.Sprintf(`
import runpy
ns = runpy.run_path(%q, run_name="not_main")
print(ns["PORT_SCAN_START"])
`, filepath.Join(scriptsDir(t), "settings.py"))
	cmd := exec.Command(py, "-c", code)
	// Deliberately NOT going through sandboxEnv/CONTEXT_GURU_PORT_BASE: this must read the
	// script's actual DEFAULT, unaffected by the override every other test in this file uses to
	// avoid binding real sockets. Nothing here binds anything.
	cmd.Env = append(os.Environ(), "CONTEXT_GURU_PORT_BASE=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("reading PORT_SCAN_START: %v\n%s", err, out)
	}
	got := strings.TrimSpace(string(out))
	want := strconv.FormatFloat(wantFloat, 'f', -1, 64)
	if got != want {
		t.Errorf("plugin.json port.default=%v but settings.py PORT_SCAN_START=%s — the two "+
			"sources of truth for the default port have drifted", portDefault.Default, got)
	}
}

// TestPresetFallbackInheritsRoutingScope: with nothing configured yet anywhere, `preset set`'s
// fallback used to guess project-local independently of what routing actually chose. It must
// instead land wherever THIS project's routing already lives.
func TestPresetFallbackInheritsRoutingScope(t *testing.T) {
	t.Run("routing at project-local: preset lands there too, no flag needed", func(t *testing.T) {
		state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
		projLocal := filepath.Join(proj, ".claude", "settings.local.json")
		if err := os.MkdirAll(filepath.Dir(projLocal), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, code := settingsInDir(t, state, home, proj, "add", "--file", projLocal, "--url", ourURL); code != 0 {
			t.Fatal("fixture: routing add failed")
		}

		py := requireTool(t, "python3")
		cmd := exec.Command(py, filepath.Join(scriptsDir(t), "settings.py"), "preset", "set", "--name", "house")
		cmd.Env = append(sandboxEnv(t), "CONTEXT_GURU_STATE="+state, "HOME="+home)
		cmd.Dir = proj
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("preset set failed: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "result=set") {
			t.Fatalf("preset set did not report success:\n%s", out)
		}
		got := readJSON(t, projLocal)
		opts, _ := ((got["pluginConfigs"].(map[string]any))["context-guru@context-guru"].(map[string]any))["options"].(map[string]any)
		if opts["preset"] != "house" {
			t.Errorf("preset not written to the project's own routing file: %v", got)
		}
		if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); !os.IsNotExist(err) {
			t.Errorf("preset fell back to the machine-wide file instead of inheriting project scope")
		}
	})

	t.Run("routing at user scope: preset follows it there automatically", func(t *testing.T) {
		state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
		userScope := filepath.Join(home, ".claude", "settings.json")
		if err := os.MkdirAll(filepath.Dir(userScope), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, code := settingsInDir(t, state, home, proj, "add", "--file", userScope, "--url", ourURL,
			"--user-scope"); code != 0 {
			t.Fatal("fixture: user-scope routing add failed")
		}

		py := requireTool(t, "python3")
		cmd := exec.Command(py, filepath.Join(scriptsDir(t), "settings.py"), "preset", "set", "--name", "house")
		cmd.Env = append(sandboxEnv(t), "CONTEXT_GURU_STATE="+state, "HOME="+home)
		cmd.Dir = proj
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("preset set failed: %v\n%s", err, out)
		}
		got := readJSON(t, userScope)
		opts, _ := ((got["pluginConfigs"].(map[string]any))["context-guru@context-guru"].(map[string]any))["options"].(map[string]any)
		if opts["preset"] != "house" {
			t.Errorf("preset did not inherit the recorded user scope: %v\n%s", got, out)
		}
	})
}

// --- fixes from review round 1 of #236 -----------------------------------------------------

// TestRemoveFirstNeverProducesAnOriginalHoldingRouting is the assertion the reviewer asked to have
// pinned, and it names the defect exactly: record_touch() runs from save(), and save() is ALSO
// cmd_remove's write path — so when the first recorded touch of a file was a REMOVAL, the O_EXCL
// "copy taken before the first edit" was a copy of the ROUTED file, permanently. The hatch then
// faithfully restored routing into an unrouted project.
//
// Who reaches it: every pre-hatch install that upgrades and then uninstalls, and anyone whose state
// directory was wiped between install and remove. That is the population this whole feature is for.
func TestRemoveFirstNeverProducesAnOriginalHoldingRouting(t *testing.T) {
	state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
	// A canonical scope path — see the comment in TestHatchRestoresWithThePluginDeleted.
	if err := os.MkdirAll(filepath.Join(proj, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, ".claude", "settings.local.json")
	// A file as a PRE-HATCH install left it: routed, with the metadata that version recorded, and no
	// pre-edit copy anywhere because that version did not take one.
	writeJSON(t, path, map[string]any{
		"env":           map[string]any{"ANTHROPIC_BASE_URL": ourURL},
		"permissions":   map[string]any{"allow": []string{"Bash(ls:*)"}},
		"$context-guru": map[string]any{"installed_base_url": ourURL},
	})

	facts, code := settingsIn(t, state, home, "remove", "--file", path, "--url", ourURL)
	if code != 0 {
		t.Fatal("remove failed")
	}
	// Whatever the recovery folder holds, nothing calling itself a pre-edit copy may contain
	// routing — `_looks_routed_by_us` is the guard, and this fixture is exactly what it exists for
	// (the file already carried context-guru's keys when first recorded, so no copy is taken).
	if facts["recovery_original"] != "unavailable" {
		t.Errorf("recovery_original=%q; this file was already ours when first recorded, so nothing "+
			"should have been captured as its \"original\"", facts["recovery_original"])
	}
	recovery := filepath.Join(proj, ".claude", "context-guru-settings-json")
	originals, _ := filepath.Glob(filepath.Join(recovery, "*.pre-install.json"))
	if len(originals) != 0 {
		t.Errorf("took %d pre-edit copy/copies of a file that was already routed: %v",
			len(originals), originals)
	}
	for _, o := range originals {
		b, err := os.ReadFile(o)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "ANTHROPIC_BASE_URL") {
			t.Fatalf("%s is called a pre-edit copy and contains routing:\n%s", o, b)
		}
	}

	// End to end: the hatch must not put back what the remove took out.
	out, code := runHatch(t, state, home, proj, "--yes")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "ANTHROPIC_BASE_URL") {
		t.Errorf("the recovery tool re-introduced routing (exit %d):\n%s\nfile:\n%s", code, out, b)
	}
	if !strings.Contains(string(b), "Bash(ls:*)") {
		t.Errorf("the user's own permission grant was lost:\n%s", b)
	}
}

// TestEmptyPlanNeverClaimsSuccessWhenNothingCouldBeRestored. An empty plan is reached from two very
// different states — genuinely clean, and "this file is routed and I have no copy to fix it with" —
// and both used to end on "already back to their pre-install state". Exit 3 was correct, but nobody
// reads an exit code; they read the last line, and it said the opposite of the truth to the one user
// who is locked out.
func TestEmptyPlanNeverClaimsSuccessWhenNothingCouldBeRestored(t *testing.T) {
	state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
	// A canonical scope path — see the comment in TestHatchRestoresWithThePluginDeleted.
	if err := os.MkdirAll(filepath.Join(proj, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, ".claude", "settings.local.json")
	writeJSON(t, path, map[string]any{"env": map[string]any{"ANTHROPIC_BASE_URL": ourURL}})
	// Routed already, no record: the pre-hatch install picking up a hatch on a no-op re-run.
	if _, code := settingsIn(t, state, home, "add", "--file", path, "--url", ourURL); code != 0 {
		t.Fatal("fixture: add failed")
	}

	out, code := runHatch(t, state, home, proj, "--yes")
	if code != 3 {
		t.Errorf("exit %d, want 3", code)
	}
	if strings.Contains(out, "already back to their pre-install state") {
		t.Errorf("told the user they are unrouted while the file is still routed:\n%s", out)
	}
	for _, want := range []string{"NOT back to their pre-install state", "ANTHROPIC_BASE_URL"} {
		if !strings.Contains(out, want) {
			t.Errorf("the honest branch is missing %q:\n%s", want, out)
		}
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), ourURL) {
		t.Error("it modified a file it has no pre-edit copy of")
	}
}

// TestARestoreWarnsThatItRevertsTheWholeFile. The primary path is `cp`, so it reverts everything in
// the file — and Claude Code appends permission grants to settings.local.json as the user approves
// tools, so a months-old install means months of grants. The confirmation prompt asked "restore
// these files?" and said nothing about that, which makes it consent to something unstated.
func TestARestoreWarnsThatItRevertsTheWholeFile(t *testing.T) {
	state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
	// A canonical scope path — see the comment in TestHatchRestoresWithThePluginDeleted.
	if err := os.MkdirAll(filepath.Join(proj, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, ".claude", "settings.local.json")
	writeJSON(t, path, map[string]any{"permissions": map[string]any{"allow": []string{"Bash(ls:*)"}}})
	if _, code := settingsIn(t, state, home, "add", "--file", path, "--url", ourURL); code != 0 {
		t.Fatal("add failed")
	}
	// The user then approves another tool and sets a model, the way a real month of use looks.
	cur := readJSON(t, path)
	cur["model"] = "opus"
	cur["permissions"] = map[string]any{"allow": []string{"Bash(ls:*)", "Bash(git push:*)"}}
	writeJSON(t, path, cur)

	out, code := runHatch(t, state, home, proj, "--dry-run")
	if code != 0 {
		t.Fatalf("dry run: exit %d\n%s", code, out)
	}
	flat := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{
		"reverts the WHOLE file",
		"pre-reset",            // and that it is undoable — beside the file now, not "$STATE/prereset/"
		"The two files differ", // shown as evidence, before the prompt
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("the plan does not warn about %q before asking:\n%s", want, out)
		}
	}
	// It must not claim a filtered count of "your" changes: settings.py rewrites the file with
	// indent=2, so a compact original differs on every line, and our own metadata spans lines that
	// carry none of the words such a filter greps out. The first version of this reported 16 lines
	// of user changes for a file whose only real change was one permission grant.
	if strings.Contains(flat, "that are NOT context-guru's") {
		t.Errorf("re-introduced a filtered line count that cannot be computed without a JSON parser:\n%s", out)
	}
}

// TestMissingCopyIsNotRenderedAsAPath: the record carries "-" when no copy was ever taken, and
// printing it verbatim gave "the pre-edit copy is missing (-)" — which reads as a bug in the tool
// rather than a known limit of what it holds, on the pre-hatch path that is now the common case.
func TestMissingCopyIsNotRenderedAsAPath(t *testing.T) {
	state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
	// A canonical scope path — see the comment in TestHatchRestoresWithThePluginDeleted.
	if err := os.MkdirAll(filepath.Join(proj, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, ".claude", "settings.local.json")
	writeJSON(t, path, map[string]any{"env": map[string]any{"ANTHROPIC_BASE_URL": ourURL}})
	if _, code := settingsIn(t, state, home, "add", "--file", path, "--url", ourURL); code != 0 {
		t.Fatal("add failed")
	}
	out, _ := runHatch(t, state, home, proj, "--dry-run")
	if strings.Contains(out, "missing (-)") {
		t.Errorf("rendered the empty-copy marker as a path:\n%s", out)
	}
	if !strings.Contains(out, "no pre-edit copy was taken") {
		t.Errorf("did not explain why there is no copy:\n%s", out)
	}
}

// TestDryRunAgreesWithARealRunAboutWhatIsLeft: --dry-run exited 0 unconditionally, so it and a real
// run disagreed about whether anything was left for a human.
func TestDryRunAgreesWithARealRunAboutWhatIsLeft(t *testing.T) {
	state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
	// A canonical scope path — see the comment in TestHatchRestoresWithThePluginDeleted.
	if err := os.MkdirAll(filepath.Join(proj, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, ".claude", "settings.local.json")
	writeJSON(t, path, map[string]any{"env": map[string]any{"ANTHROPIC_BASE_URL": ourURL}})
	if _, code := settingsIn(t, state, home, "add", "--file", path, "--url", ourURL); code != 0 {
		t.Fatal("add failed")
	}
	_, dry := runHatch(t, state, home, proj, "--dry-run")
	_, real := runHatch(t, state, home, proj, "--yes")
	if dry != real {
		t.Errorf("--dry-run exited %d but the real run exited %d; they must agree about whether "+
			"anything is left for a human", dry, real)
	}
}

// TestAnUnusableStateDirectoryNeverFailsTheInstall. "Fail open, always" is a hard boundary in this
// repo. An install that would otherwise have worked must not be broken by the recovery bookkeeping
// — it must report what it could not do and carry on.
//
// The recovery folder itself now lives BESIDE the settings file, not under the state directory, so
// a state directory this unusable no longer touches it at all — only the centralized hatch SCRIPT
// (still installed under the state dir, since it has no per-project home) is affected. That
// decoupling is worth asserting directly: the install should come away with a real recovery_dir and
// recovery_original despite `reset_hatch=unavailable`, which the old, single-state-dir design could
// not have claimed.
func TestAnUnusableStateDirectoryNeverFailsTheInstall(t *testing.T) {
	home, proj := t.TempDir(), t.TempDir()
	// A FILE where the state directory should be, so every path inside it is unusable.
	blocked := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, "settings.json")
	writeJSON(t, path, map[string]any{"theme": "dark"})

	facts, code := settingsIn(t, blocked, home, "add", "--file", path, "--url", ourURL)
	if code != 0 || facts["result"] != "added" {
		t.Fatalf("an unusable state dir broke the install: exit %d, %v", code, facts)
	}
	if facts["reset_hatch"] != "unavailable" {
		t.Errorf("reset_hatch=%q; it must say so rather than imply a hatch exists", facts["reset_hatch"])
	}
	wantRecovery := filepath.Join(proj, "context-guru-settings-json")
	if facts["recovery_dir"] != wantRecovery {
		t.Errorf("recovery_dir=%q, want %q — it must not depend on the (unusable) state directory",
			facts["recovery_dir"], wantRecovery)
	}
	original := filepath.Join(wantRecovery, "settings.pre-install.json")
	if facts["recovery_original"] != original {
		t.Errorf("recovery_original=%q, want %q", facts["recovery_original"], original)
	}
	if _, err := os.Stat(original); err != nil {
		t.Errorf("the pre-install copy was not actually written despite being reported: %v", err)
	}
	env, _ := readJSON(t, path)["env"].(map[string]any)
	if env["ANTHROPIC_BASE_URL"] != ourURL {
		t.Errorf("the routing itself did not get written: %v", env)
	}
	if readJSON(t, path)["theme"] != "dark" {
		t.Error("the user's settings were damaged")
	}
}

// --- fixes from review round 2 of #236 -----------------------------------------------------

// TestThePlanNeverPrintsACredential. The whole-file warning added in round 1 diffs the settings
// file, and `env` is exactly where people keep ANTHROPIC_API_KEY — so `--dry-run`, the invocation the
// docs tell people to run FIRST, printed a live key twice, once per side of the diff. It contradicted
// the principle stated 150 lines above it in the same script.
//
// The audience is what makes it worse than a secret in a log: somebody debugging a 401, whose next
// move is to paste the output into an issue or a screenshot, precisely because it is written to be
// read and acted on. TestHatchReportsCredentialsByLocationAndNeverByValue covers the rc-file half and
// did not cover this path, which is how it got through.
func TestThePlanNeverPrintsACredential(t *testing.T) {
	state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
	// A canonical scope path — see the comment in TestHatchRestoresWithThePluginDeleted.
	if err := os.MkdirAll(filepath.Join(proj, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, ".claude", "settings.local.json")
	const (
		apiKey    = "sk-ant-SUPERSECRET-do-not-print-me"
		oddlyName = "hunter2-also-secret" // a key-ish name that is not spelled ANTHROPIC_*
		urlCreds  = "pw123"
	)
	// Written COMPACT, the way a hand-edited settings file actually looks, and not via writeJSON.
	// That matters for what this test covers: writeJSON indents, settings.py rewrites with the same
	// indent, so the credential lines came out byte-identical on both sides of the diff and never
	// appeared in it. The secret-absence assertion then passed for the wrong reason — nothing to
	// redact — which is exactly what the "no redaction marker" check below caught.
	fixture := `{"env": {"ANTHROPIC_API_KEY": "` + apiKey + `", "authToken": "` + oddlyName +
		`", "MY_GATEWAY": "https://alice:` + urlCreds + `@gw.example/v1"},` +
		` "permissions": {"allow": ["Bash(ls:*)"]}}`
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, code := settingsIn(t, state, home, "add", "--file", path, "--url", ourURL); code != 0 {
		t.Fatal("add failed")
	}
	// A later change, so the whole-file warning and its diff are reached at all.
	cur := readJSON(t, path)
	cur["permissions"] = map[string]any{"allow": []string{"Bash(ls:*)", "Bash(git push:*)"}}
	writeJSON(t, path, cur)

	for _, args := range [][]string{{"--dry-run"}, {"--yes"}} {
		out, _ := runHatch(t, state, home, proj, args...)
		for _, secret := range []string{apiKey, oddlyName, urlCreds} {
			if strings.Contains(out, secret) {
				t.Errorf("hatch %v printed a credential (%q):\n%s", args, secret, out)
			}
		}
		if args[0] == "--dry-run" {
			// The redaction must not gut the diff: showing THAT a line changed is its whole purpose.
			if !strings.Contains(out, "git push") {
				t.Errorf("redaction removed the change the diff exists to show:\n%s", out)
			}
			if !strings.Contains(out, "value not shown") {
				t.Errorf("no redaction marker, so the diff may simply not have run:\n%s", out)
			}
		}
	}
}

// TestNormalUninstallDoesNotClaimTheOriginalIsGone. An ordinary uninstall of an ordinarily-installed
// project takes the "file already carries our keys" branch — correctly, since a copy of the file at
// that moment would be meaningless. But it also SET recovery_original=unavailable, while the good,
// verified-clean copy from the install sat on disk. install/SKILL.md turns that fact into "the hatch
// can unroute but not restore", so the skill would have told users their content was unrecoverable
// when it was not: the same class of inverted reassurance as round 1's finding 2.
func TestNormalUninstallDoesNotClaimTheOriginalIsGone(t *testing.T) {
	state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
	path := filepath.Join(proj, "settings.json")
	writeJSON(t, path, map[string]any{"permissions": map[string]any{"allow": []string{"Bash(ls:*)"}}})

	if _, code := settingsIn(t, state, home, "add", "--file", path, "--url", ourURL); code != 0 {
		t.Fatal("add failed")
	}
	facts, code := settingsIn(t, state, home, "remove", "--file", path, "--url", ourURL)
	if code != 0 {
		t.Fatalf("remove: exit %d, %v", code, facts)
	}
	if facts["recovery_original"] == "unavailable" {
		t.Errorf("remove reported recovery_original=unavailable; the copy already on disk is what "+
			"matters, not whether THIS call could take one: %v", facts)
	}
	// And the copy really is there, really is clean. Beside the file now, not in a shared
	// `originals/` directory under the state dir.
	originals, _ := filepath.Glob(filepath.Join(proj, "context-guru-settings-json", "*.pre-install.json"))
	if len(originals) != 1 {
		t.Fatalf("want exactly one pre-install copy, got %v", originals)
	}
	b, err := os.ReadFile(originals[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "ANTHROPIC_BASE_URL") {
		t.Errorf("the original holds routing:\n%s", b)
	}
}

// TestAnExportedBaseURLIsNotCalledAFileProblem. INCOMPLETE grew to include an environment condition
// (a base URL exported in the user's shell, which no settings change can override), and wiring that
// into the empty-plan branch made it lie in the other direction: with clean files and a routed shell
// it printed "your settings files are NOT back to their pre-install state" plus instructions to
// repair a file, two lines under a line saying that file already matched its pre-install copy.
//
// Which sentence to print is a question about files; the exit code is a question about whether
// anything is left for a human. Two questions, two flags.
func TestAnExportedBaseURLIsNotCalledAFileProblem(t *testing.T) {
	state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
	// A canonical scope path — see the comment in TestHatchRestoresWithThePluginDeleted.
	if err := os.MkdirAll(filepath.Join(proj, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, ".claude", "settings.local.json")
	writeJSON(t, path, map[string]any{"permissions": map[string]any{"allow": []string{"Bash(ls:*)"}}})
	if _, code := settingsIn(t, state, home, "add", "--file", path, "--url", ourURL); code != 0 {
		t.Fatal("add failed")
	}
	if _, code := runHatch(t, state, home, proj, "--yes"); code != 0 {
		t.Fatal("first run should be a clean restore")
	}

	// Same clean state, but the shell itself is routed.
	cmd := exec.Command(hatchPath(t, state), "--yes")
	cmd.Dir = proj
	cmd.Env = []string{
		"CONTEXT_GURU_STATE=" + state, "HOME=" + home, "PATH=" + os.Getenv("PATH"),
		"ANTHROPIC_BASE_URL=" + ourURL,
	}
	b, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	if code != 3 {
		t.Errorf("exit %d, want 3: an exported loopback base URL is left for a human", code)
	}
	if strings.Contains(out, "NOT back to their pre-install state") {
		t.Errorf("called a clean file a file problem:\n%s", out)
	}
	for _, want := range []string{"already back to their pre-install state", "Your FILES are fine",
		"exported in THIS SHELL"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

// TestLoopbackDetectionCoversTheShapesThatMatter. The predicate guarding round 1's severe finding
// used a startswith tuple, and everything it missed was a false NEGATIVE — the direction that
// re-introduces the defect, by taking a copy of an already-routed file and calling it an original.
func TestLoopbackDetectionCoversTheShapesThatMatter(t *testing.T) {
	for _, tc := range []struct {
		url     string
		ourFile bool
	}{
		{"http://127.0.0.1:8787/anthropic", true},
		{"http://127.0.0.1/anthropic", true}, // no port
		{"http://0.0.0.0:8787/anthropic", true},
		{"http://[::1]/anthropic", true}, // v6, no port
		{"https://localhost:8787/anthropic", true},
		{"https://gateway.corp.example/anthropic", false},
		{"https://not-localhost.example.com/v1", false}, // must not match on substring
	} {
		state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
		path := filepath.Join(proj, "settings.json")
		// No metadata and none of our other keys, so the loopback clause is the only thing deciding.
		writeJSON(t, path, map[string]any{"env": map[string]any{"ANTHROPIC_BASE_URL": tc.url}})
		if _, code := settingsIn(t, state, home, "remove", "--file", path, "--url", tc.url); code != 0 {
			t.Fatalf("%s: remove failed", tc.url)
		}
		originals, _ := filepath.Glob(filepath.Join(proj, "context-guru-settings-json", "*.pre-install.json"))
		if tc.ourFile && len(originals) != 0 {
			b, _ := os.ReadFile(originals[0])
			t.Errorf("%s: took a pre-edit copy of an already-routed file:\n%s", tc.url, b)
		}
		if !tc.ourFile && len(originals) != 1 {
			t.Errorf("%s: declined to copy a file that is not ours (want an original, got %v)",
				tc.url, originals)
		}
	}
}

// TestTroubleshootingLeadsWithTheCommand. Discoverability is part of the feature, not documentation
// polish: a user who needs the hatch is searching the docs with a Claude that cannot answer
// questions. Troubleshooting is the section they open, and it did not name the hatch at all.
func TestTroubleshootingLeadsWithTheCommand(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "docs", "how-to", "install-plugin.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, after, ok := strings.Cut(string(b), "## Troubleshooting")
	if !ok {
		t.Fatal("no Troubleshooting section")
	}
	// Within the first entry, not somewhere further down the section.
	head := after
	if len(head) > 900 {
		head = head[:900]
	}
	for _, want := range []string{"context-guru-reset", "hangs", "cannot run"} {
		if !strings.Contains(head, want) {
			t.Errorf("the first Troubleshooting entry does not cover %q:\n%s", want, head)
		}
	}
}

// --- fixes from review round 3 of #236 -----------------------------------------------------

// redactShapes is the ledger behind reset.sh's `redact` filter, one row per credential shape.
//
// It exists because that filter is a DENYLIST on a recovery tool, and a denylist is a list that will
// be wrong again — it has now been wrong three times in this PR alone, in three different shapes.
// Inverting it to an allowlist is worse on a diff of arbitrary JSON (it would redact the
// `permissions` entries the diff exists to show), so the discipline is this table instead: ADD A ROW
// WHEN YOU ADD A RULE, and the next miss is a row somebody forgot rather than an invisible leak.
//
// The must-survive rows matter as much as the leak rows. A filter that redacts everything passes
// every absence assertion and makes the diff useless, which is the failure this table also pins.
var redactShapes = []struct {
	name string
	line string
	// secret must not appear in the output. Empty means the line must pass through UNCHANGED.
	secret string
}{
	{"ANTHROPIC_API_KEY", `  "ANTHROPIC_API_KEY": "sk-ant-REALKEY0000",`, "REALKEY0000"},
	{"ANTHROPIC_AUTH_TOKEN", `  "ANTHROPIC_AUTH_TOKEN": "REALTOKENVALUE",`, "REALTOKENVALUE"},
	// The one that matters most here, and not a shape invented for the test.
	//
	// PROVENANCE, recorded because it is the strongest argument for this whole table: this leak was
	// LIVE, not theoretical. `ANTHROPIC_CUSTOM_HEADERS` carrying a `<header>: <token>` string is how a
	// Context Guru credential is set on this project's own development machines — set for interactive
	// sessions and explicitly unset for benchmark runs — and it was present in the real
	// ~/.claude/settings.json of the machine this filter was written on, where the author had read
	// that file earlier the same day and not connected it to the filter.
	//
	// So the single most likely credential to appear in a context-guru user's env block was the one
	// the first two versions of the filter did not catch, and its name contains none of
	// key/token/secret/password/credential. HEADER is in the name class because of this row.
	{"ANTHROPIC_CUSTOM_HEADERS", `  "ANTHROPIC_CUSTOM_HEADERS": "x-context-guru-token: cg_live_REALGURU",`, "REALGURU"},
	{"authorization bearer JWT", `  "authorization": "Bearer eyJhbGciOiJIUzI1NiJ9.REALJWTBODY",`, "REALJWTBODY"},
	{"github PAT", `  "GITHUB_PAT": "ghp_REALPAT00000000",`, "REALPAT00000000"},
	{"credential in a query param", `  "ANTHROPIC_UPSTREAM": "https://gw/anthropic?api_key=REALQUERYKEY",`, "REALQUERYKEY"},
	{"credential in URL userinfo", `  "MY_GW": "https://svc:REALURLPASS@gw.corp/anthropic",`, "REALURLPASS"},
	{"slack token", `  "SLACK": "xoxb-REALSLACKTOKEN",`, "REALSLACKTOKEN"},
	{"aws access key id", `  "AWS_ACCESS_KEY_ID": "AKIAREALAWSKEY0000",`, "REALAWSKEY"},
	{"bare exported base URL with userinfo", `https://svc:REALURLPASS@gw.corp/anthropic`, "REALURLPASS"},

	// The four shapes the second review found escaping the denylist. Each is the reason the
	// `"key": value` case is now an allowlist: none of these key names or value shapes was on any list.
	{"sk_live_ (underscore, not sk-)", `  "STRIPE_KEY": "sk_live_REALSTRIPEKEY",`, "REALSTRIPEKEY"},
	{"secret in a URL path", `  "HOOK": "https://hooks.slack.com/services/T00/B00/REALWEBHOOK",`, "REALWEBHOOK"},
	{"credential in ?auth=", `  "ANTHROPIC_BASE_URL": "https://gw.corp/anthropic?auth=REALAUTHSECRET",`, "REALAUTHSECRET"},
	// apiKeyHelper is a real Claude Code settings key whose entire purpose is producing a credential,
	// and a non-string value escaped a rule that required a quote straight after the colon.
	{"apiKeyHelper with an object value", `  "apiKeyHelper": {"cmd": "echo REALHELPERSECRET"},`, "REALHELPERSECRET"},
	// The property the allowlist buys, and the reason it is worth the inversion: a credential key
	// nobody has thought of yet is covered on the day it is invented, not the day a rule is added.
	{"an entirely unknown key", `  "SOME_FUTURE_CREDENTIAL": "REALFUTURESECRET",`, "REALFUTURESECRET"},
	{"a secret in a non-permission array", `      "REALARRAYSECRET",`, "REALARRAYSECRET"},
	{"a deep path on a routing key", `  "ANTHROPIC_BASE_URL": "https://gw/a/b/REALDEEPSECRET",`, "REALDEEPSECRET"},

	{"permission grant must survive", `      "Bash(git push:*)",`, ""},
	{"model must survive", `  "model": "opus",`, ""},
	{"theme must survive", `  "theme": "dark",`, ""},
	{"a plain loopback base URL must survive", `  "ANTHROPIC_BASE_URL": "http://127.0.0.1:8787/anthropic",`, ""},
	// These three are what the verify pass and the no-record grep exist to SHOW the user — they are
	// the routing itself, not a credential. AUTH and HEADER in the name class match more broadly than
	// the original five did, so each is pinned here: over-redacting these would leave a locked-out
	// user reading "<value not shown>" where they need to see which port they are pointed at.
	{"a corporate base URL must survive", `  "ANTHROPIC_BASE_URL": "https://gateway.corp.example/anthropic",`, ""},
	{"ANTHROPIC_UPSTREAM must survive", `  "ANTHROPIC_UPSTREAM": "https://gateway.corp.example",`, ""},
	{"CONTEXT_GURU_BIN must survive", `  "CONTEXT_GURU_BIN": "/home/user/.local/bin/context-guru-proxy",`, ""},
	// Structure must survive, or the diff becomes unreadable: a container opening is not a value, and
	// its members are judged on their own lines.
	{"a container opening must survive", `  "apiKeyHelper": {`, ""},
	{"a diff marker must survive", `> "model": "sonnet",`, ""},
}

// TestRedactCoversEveryKnownCredentialShape drives the real filter, lifted out of reset.sh, rather
// than a copy of it — a copy would drift from the thing that ships.
func TestRedactCoversEveryKnownCredentialShape(t *testing.T) {
	requireTool(t, "bash")
	body, err := os.ReadFile(filepath.Join(scriptsDir(t), "reset.sh"))
	if err != nil {
		t.Fatal(err)
	}
	// Slice out REDACT_NAME through the end of the redact() function.
	_, rest, ok := strings.Cut(string(body), "REDACT_MARK=")
	if !ok {
		t.Fatal("reset.sh no longer defines REDACT_MARK; this test is extracting the wrong thing")
	}
	fnEnd := strings.Index(rest, "\n}\n")
	if fnEnd < 0 {
		t.Fatal("could not find the end of redact(); extraction would be silently partial")
	}
	fn := "REDACT_MARK=" + rest[:fnEnd+3]
	if !strings.Contains(fn, "redact()") || !strings.Contains(fn, "awk") {
		t.Fatalf("extracted fragment does not look like the filter:\n%s", fn)
	}
	fnPath := filepath.Join(t.TempDir(), "redact.sh")
	if err := os.WriteFile(fnPath, []byte(fn), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range redactShapes {
		cmd := exec.Command("sh", "-c",
			`. "$1"; printf '%s\n' "$2" | redact`, "_", fnPath, tc.line)
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: running redact: %v (%s)", tc.name, err, b)
		}
		got := strings.TrimRight(string(b), "\n")
		if tc.secret == "" {
			if got != tc.line {
				t.Errorf("%s: over-redacted, so the diff loses what it exists to show\n got:  %q\n want: %q",
					tc.name, got, tc.line)
			}
			continue
		}
		if strings.Contains(got, tc.secret) {
			t.Errorf("%s: LEAKED %q\n got: %q", tc.name, tc.secret, got)
		}
		if got == tc.line {
			t.Errorf("%s: line passed through untouched, so no rule matched it at all\n got: %q",
				tc.name, got)
		}
	}
}

// TestNoPrinterOfContentEscapesTheFilter walks every site in reset.sh that prints real content and
// plants a credential in each. The round-2 fix covered the plan diff; the review then found a FOURTH
// site inside report_environment itself — the function whose own header promises values are never
// printed — because that test only exercised the diff. So this one is organised by SITE.
func TestNoPrinterOfContentEscapesTheFilter(t *testing.T) {
	const secret = "REALSECRET-do-not-print-me"

	t.Run("the verify pass", func(t *testing.T) {
		// A user's own gateway carrying credentials in the URL, taken over with --force and then
		// handed back. Verify greps the restored file and prints the matching lines.
		state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
		if err := os.MkdirAll(filepath.Join(proj, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(proj, ".claude", "settings.local.json")
		theirs := "https://svc:" + secret + "@gw.corp.example/anthropic"
		writeJSON(t, path, map[string]any{"env": map[string]any{"ANTHROPIC_BASE_URL": theirs}})
		if _, code := settingsIn(t, state, home, "add", "--file", path, "--url", ourURL,
			"--force"); code != 0 {
			t.Fatal("add --force failed")
		}
		out, _ := runHatch(t, state, home, proj, "--yes")
		if strings.Contains(out, secret) {
			t.Errorf("the verify pass printed a credential:\n%s", out)
		}
		if !strings.Contains(out, "still mentions") {
			t.Fatalf("the verify branch never ran, so this asserted nothing:\n%s", out)
		}
	})

	t.Run("the environment report", func(t *testing.T) {
		state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
		if err := os.MkdirAll(filepath.Join(proj, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(proj, ".claude", "settings.local.json")
		writeJSON(t, path, map[string]any{"theme": "dark"})
		if _, code := settingsIn(t, state, home, "add", "--file", path, "--url", ourURL); code != 0 {
			t.Fatal("add failed")
		}
		cmd := exec.Command(hatchPath(t, state), "--dry-run")
		cmd.Dir = proj
		cmd.Env = []string{
			"CONTEXT_GURU_STATE=" + state, "HOME=" + home, "PATH=" + os.Getenv("PATH"),
			"ANTHROPIC_BASE_URL=https://svc:" + secret + "@gw.corp.example/anthropic",
		}
		b, _ := cmd.CombinedOutput()
		out := string(b)
		if strings.Contains(out, secret) {
			t.Errorf("report_environment printed a credential:\n%s", out)
		}
		if !strings.Contains(out, "exported in this shell") {
			t.Fatalf("the branch that prints $base never ran:\n%s", out)
		}
	})

	t.Run("the no-record grep", func(t *testing.T) {
		state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
		if err := os.MkdirAll(filepath.Join(proj, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(proj, ".claude", "settings.local.json")
		writeJSON(t, path, map[string]any{"theme": "dark"})
		if _, code := settingsIn(t, state, home, "add", "--file", path, "--url", ourURL,
			"--upstream", "https://gw.corp/anthropic?api_key="+secret); code != 0 {
			t.Fatal("add failed")
		}
		hatch := hatchPath(t, state)
		// See TestHatchWithNoRecordTellsTheUserWhereToLook — the recovery folder is the record now.
		if err := os.RemoveAll(filepath.Join(proj, ".claude", "context-guru-settings-json")); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(hatch, "--yes")
		cmd.Dir = proj
		cmd.Env = []string{"CONTEXT_GURU_STATE=" + state, "HOME=" + home, "PATH=" + os.Getenv("PATH")}
		b, _ := cmd.CombinedOutput()
		out := string(b)
		if strings.Contains(out, secret) {
			t.Errorf("the no-record grep printed a credential:\n%s", out)
		}
		if !strings.Contains(out, "ANTHROPIC_UPSTREAM") {
			t.Fatalf("the grep never printed the key line, so this asserted nothing:\n%s", out)
		}
	})
}

// TestASuccessfulRestoreSaysSoEvenWhenTheShellIsRouted. The final summary tested INCOMPLETE for a
// question about files — the same bug fixed in the empty-plan branch, one branch further down. So a
// COMPLETELY successful restore, verified unrouted, run from a shell with an exported base URL (a
// hosted agent, or simply the shell they installed from) printed "Finished with something left for
// you" and never printed the count: $RESTORED was thrown away on the one run where it is the good
// news, and the user who had just recovered was told the run did not finish.
//
// It survived three rounds because NO test grepped for "Done." at all.
func TestASuccessfulRestoreSaysSoEvenWhenTheShellIsRouted(t *testing.T) {
	state, home, proj := t.TempDir(), t.TempDir(), t.TempDir()
	// A canonical scope path — see the comment in TestHatchRestoresWithThePluginDeleted.
	if err := os.MkdirAll(filepath.Join(proj, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, ".claude", "settings.local.json")
	writeJSON(t, path, map[string]any{"permissions": map[string]any{"allow": []string{"Bash(ls:*)"}}})
	if _, code := settingsIn(t, state, home, "add", "--file", path, "--url", ourURL); code != 0 {
		t.Fatal("add failed")
	}

	// Non-empty plan (a real restore) AND a routed shell: the two conditions together.
	cmd := exec.Command(hatchPath(t, state), "--yes")
	cmd.Dir = proj
	cmd.Env = []string{
		"CONTEXT_GURU_STATE=" + state, "HOME=" + home, "PATH=" + os.Getenv("PATH"),
		"ANTHROPIC_BASE_URL=" + ourURL,
	}
	b, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	if !strings.Contains(out, "restored:") {
		t.Fatalf("no restore happened, so this test asserted nothing:\n%s", out)
	}
	if !strings.Contains(out, "Done. 1 file(s) put back.") {
		t.Errorf("a fully successful restore never reported its count:\n%s", out)
	}
	if strings.Contains(out, "Finished with something left for you") {
		t.Errorf("told the user the run did not finish after a verified-clean restore:\n%s", out)
	}
	if code != 3 {
		t.Errorf("exit %d, want 3: the exported base URL is still left for a human", code)
	}
	if !strings.Contains(out, "environment report above") {
		t.Errorf("did not point at what is actually left:\n%s", out)
	}
}

// ---------------------------------------------------------------------------------------------
// install.sh --route: the orchestrator
// ---------------------------------------------------------------------------------------------
//
// These tests exist because the seven steps they cover used to be seven paragraphs of prose in
// skills/install/SKILL.md, and every defect in that file's history had one shape: the prose was
// right and the command a model emitted differed from it. Asserting on a script means asserting on
// what will actually run.
//
// Two properties get the most attention here, because both have already shipped as defects:
//   - `--plan` must write NOTHING. It is the only command in this design that is safe to run
//     before the user has agreed to anything, and a plan with a side effect is not a plan.
//   - the ORDER of "start the proxy" and "write the routing key" is load-bearing. Claude Code picks
//     an env change up while the session is running, so writing first once killed the installing
//     session with Connection refused before it reached the step that starts the proxy.

// routeEnv builds the environment a --route run sees: a sandbox HOME and state directory, a
// controlled PATH, and — critically — ANTHROPIC_BASE_URL REMOVED.
//
// That last one is not tidiness. The developer's own shell has a base URL set (that is what this
// plugin does), and inheriting it made every "clean project" fixture look already-routed, so the
// orchestrator refused with base_url_already_set. The leak was caught by a smoke run; without this
// helper it would have made the conflict tests below pass for the wrong reason.
func routeEnv(t *testing.T, home, state, extraPath string) []string {
	t.Helper()
	env := []string{}
	for _, kv := range sandboxEnv(t) {
		if strings.HasPrefix(kv, "ANTHROPIC_BASE_URL=") {
			continue
		}
		env = append(env, kv)
	}
	path := os.Getenv("PATH")
	if extraPath != "" {
		path = extraPath + string(os.PathListSeparator) + path
	}
	return append(env,
		"HOME="+home,
		"XDG_STATE_HOME="+state,
		"CLAUDE_CONFIG_DIR="+filepath.Join(home, ".claude"),
		"PATH="+path,
	)
}

// runRoute runs install.sh --route in `dir` and returns the parsed key=value facts.
func runRoute(t *testing.T, dir string, env []string, args ...string) (map[string]string, int) {
	t.Helper()
	requireTool(t, "bash")
	argv := append([]string{filepath.Join(scriptsDir(t), "install.sh"), "--route"}, args...)
	cmd := exec.Command("bash", argv...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running install.sh --route %v: %v (%s)", args, err, out)
	}
	facts := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			facts[k] = v
		}
	}
	t.Logf("install.sh --route %v -> exit %d\n%s", args, code, out)
	return facts, code
}

// writePluginOptions writes the CONFIGURED plugin options where `settings.py config` reads them.
// The shape matters and is easy to get wrong: pluginConfigs[<plugin>@<marketplace>].options.
func writePluginOptions(t *testing.T, home string, opts map[string]any) {
	t.Helper()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(dir, "settings.json"), map[string]any{
		"pluginConfigs": map[string]any{
			"context-guru@context-guru": map[string]any{"options": opts},
		},
	})
}

// consentOK returns the arguments a WRITING-path test needs: the scope plus the explicit consent
// flag. Spelled out in a helper so it is obvious that these tests stand in for a user who answered
// yes, rather than quietly routing around the gate - and so that the gate's own test,
// TestRouteRefusesWithoutExplicitConsent, is the only place the flag is absent on purpose.
func consentOK(extra ...string) []string {
	return append([]string{"--scope", "project", "--i-consent-to-traffic-interception"}, extra...)
}

// TestRoutePlanWritesNothing. --plan is the command that runs before the user has agreed to
// anything — it is what a `!` block in the skill executes at render time, unprompted. If it can
// create a settings file, a state directory or a strategy config, then rendering the skill is
// itself an install, which is exactly the consent this design is careful about.
func TestRoutePlanWritesNothing(t *testing.T) {
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	facts, code := runRoute(t, proj, routeEnv(t, home, state, ""), "--plan", "--scope", "project")
	if code != 0 || facts["result"] != "planned" {
		t.Fatalf("exit %d result=%q, want 0/planned: %v", code, facts["result"], facts)
	}
	for _, p := range []string{
		filepath.Join(proj, ".claude", "settings.local.json"),
		filepath.Join(state, "context-guru"),
	} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("--plan created %s; a plan with a side effect is not a plan, and this one runs "+
				"before the user has been asked anything", p)
		}
	}
	// It also has to be USEFUL, or the skill is back to placeholders: the plan is what lets the model
	// ask its one question with the user's real file and real endpoint in it.
	for _, k := range []string{"file", "port", "preset", "cache_strategy", "base_url", "permission_rule"} {
		if facts[k] == "" {
			t.Errorf("the plan omits %q, so the model has nothing concrete to report or ask about", k)
		}
	}
}

// TestRoutePlanResolvesTheConfiguredPortPerOption is the defect that shipped once, in the shape
// `${CLAUDE_PLUGIN_OPTION_PORT:-8787}`: plugin options never reach a Bash tool call, so a shell
// default silently won. The routing key then named 8787 while every later hook read the CONFIGURED
// port, self-gated on it, saw an unrouted project and did nothing — leaving the one running proxy
// with no auto-restart behind it, silently, until it idled out.
//
// It also pins the PER-OPTION fallback: `settings.py config` prints a line only for keys the user
// actually set, so a partial config (port set, preset never touched) must not be read as "the rest
// are configured too" or as "nothing is configured".
func TestRoutePlanResolvesTheConfiguredPortPerOption(t *testing.T) {
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	// Deliberately partial: port and strategy set, preset and idle_exit never touched.
	writePluginOptions(t, home, map[string]any{"port": 4041, "cache_strategy": "none"})
	facts, code := runRoute(t, proj, routeEnv(t, home, state, ""), "--plan", "--scope", "project")
	if code != 0 {
		t.Fatalf("exit %d: %v", code, facts)
	}
	if facts["port"] != "4041" {
		t.Errorf("port=%q, want 4041: a defaulted 8787 is the shipped defect this test exists for",
			facts["port"])
	}
	if facts["base_url"] != "http://127.0.0.1:4041/anthropic" {
		t.Errorf("base_url=%q does not carry the configured port; the routing key and the hooks would "+
			"then disagree about which proxy this project uses", facts["base_url"])
	}
	if facts["health_url"] != "http://127.0.0.1:4041/healthz" {
		t.Errorf("health_url=%q does not carry the configured port, so the check would prove nothing "+
			"about the proxy this install actually starts", facts["health_url"])
	}
	if facts["cache_strategy"] != "none" {
		t.Errorf("cache_strategy=%q, want the configured none", facts["cache_strategy"])
	}
	// The two the user never set must fall back individually, not be blanked because the file existed.
	if facts["preset"] != "off" {
		t.Errorf("preset=%q; an unconfigured option must fall back to the plugin.json default on its "+
			"own. Note `off` and empty are NOT the same thing: `off` is a named, deliberate "+
			"passthrough, where an empty preset loads a config with no pipeline and no name for what "+
			"it is doing", facts["preset"])
	}
	if facts["idle_exit"] != "24h" {
		t.Errorf("idle_exit=%q, want the 24h default", facts["idle_exit"])
	}
}

// TestRouteRefusesAnExistingBaseURLUntilTold. Replacing somebody's gateway silently breaks their
// setup while looking like success, and which of chain/replace/abort is right depends on whose
// endpoint it is — not something a script can know. So it must stop, and it must NAME the value.
func TestRouteRefusesAnExistingBaseURLUntilTold(t *testing.T) {
	const theirs = "https://gateway.corp.example/v1"
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	env := append(routeEnv(t, home, state, ""), "ANTHROPIC_BASE_URL="+theirs)

	// Exit 0, NOT 2. The plan runs from a `!` block in the skill, and a non-zero exit there produced
	// an invocation with NO OUTPUT AT ALL in a real sandboxed session — the model never saw the plan
	// and never asked the question, while the run looked fine because nothing had been written. A
	// plan reports; `needs_decision` is data, not a failure.
	facts, code := runRoute(t, proj, env, "--plan", "--scope", "project")
	if code != 0 || facts["result"] != "needs_decision" || facts["reason"] != "base_url_already_set" {
		t.Fatalf("exit %d result=%q reason=%q, want 0/needs_decision/base_url_already_set: %v",
			code, facts["result"], facts["reason"], facts)
	}
	if facts["existing_base_url"] != theirs {
		t.Errorf("the refusal does not name what is already set (%q); the user cannot answer the "+
			"question without it", facts["existing_base_url"])
	}

	// Told to chain: their endpoint becomes the upstream, so their gateway keeps holding the
	// credential and doing model-name rewriting. Forgetting to carry it is how chaining works until
	// the running proxy idles out and the next one is aimed at api.anthropic.com.
	facts, code = runRoute(t, proj, env, "--plan", "--scope", "project", "--on-conflict", "chain")
	if code != 0 || facts["result"] != "planned" {
		t.Fatalf("chain: exit %d result=%q: %v", code, facts["result"], facts)
	}
	if facts["chained"] != "true" || facts["upstream"] != theirs {
		t.Errorf("chained=%q upstream=%q; chaining has to record THEIR endpoint as the upstream",
			facts["chained"], facts["upstream"])
	}

	// Told to abort: exit 0 and change nothing. A refusal the user asked for is not a failure.
	facts, code = runRoute(t, proj, env, "--scope", "project", "--on-conflict", "abort")
	if code != 0 || facts["result"] != "aborted" {
		t.Errorf("abort: exit %d result=%q, want 0/aborted", code, facts["result"])
	}
	if _, err := os.Stat(filepath.Join(proj, ".claude", "settings.local.json")); err == nil {
		t.Error("abort wrote a settings file anyway")
	}
}

// TestRouteRefusesMachineWideWithoutTheFlag. A project-scope mistake costs one project; the same
// mistake machine-wide takes out every session the user could use to fix it. The refusal lives in
// the script rather than only in the skill's prose, because a default that exists only in a prompt
// can be read differently, and what it guards against is a machine-wide lockout.
func TestRouteRefusesMachineWideWithoutTheFlag(t *testing.T) {
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	env := routeEnv(t, home, state, "")
	// Under --plan this is a reported decision at exit 0 (see TestRoutePlanAlwaysExitsZero).
	facts, code := runRoute(t, proj, env, "--plan", "--scope", "user")
	if code != 0 || facts["reason"] != "user_scope_needs_flag" {
		t.Fatalf("exit %d reason=%q, want 0/user_scope_needs_flag: %v", code, facts["reason"], facts)
	}
	// But the WRITING path must still refuse it, with a code a caller can act on.
	if f, c := runRoute(t, proj, env, "--scope", "user"); c != 2 || f["result"] != "refused" {
		t.Errorf("confirm with --scope user: exit %d result=%q, want 2/refused — a machine-wide "+
			"install must not be reachable without the flag", c, f["result"])
	}
	facts, code = runRoute(t, proj, env, "--plan", "--scope", "user", "--i-understand-machine-wide")
	if code != 0 || facts["result"] != "planned" {
		t.Fatalf("with the flag: exit %d result=%q: %v", code, facts["result"], facts)
	}
	if want := filepath.Join(home, ".claude", "settings.json"); facts["file"] != want {
		t.Errorf("file=%q, want the machine-wide file %q", facts["file"], want)
	}
}

// TestRouteRefusesUnknownFlagsRatherThanIgnoringThem. A silently dropped --scope writes the wrong
// file and reports success, which is the worst available outcome: the user is told the thing they
// asked for happened somewhere it did not.
func TestRouteRefusesUnknownFlagsRatherThanIgnoringThem(t *testing.T) {
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	env := routeEnv(t, home, state, "")
	for _, tc := range []struct {
		args   []string
		reason string
	}{
		{[]string{"--plan", "--frobnicate"}, "unknown_flag"},
		{[]string{"--plan", "--mode", "sideways"}, "unknown_mode"},
		{[]string{"--plan", "--on-conflict", "maybe"}, "unknown_on_conflict"},
	} {
		facts, code := runRoute(t, proj, env, tc.args...)
		if code == 0 || facts["reason"] != tc.reason {
			t.Errorf("%v: exit %d reason=%q, want nonzero/%s", tc.args, code, facts["reason"], tc.reason)
		}
	}
}

// TestRouteAttachModeSkipsInstallAndStart. `attach` is not a smaller `local`: on a gateway
// deployment the proxy is landed in the gateway, so there is nothing to install and starting a
// second proxy alongside it is the double-interception case. The URL is also the one thing that
// cannot be derived there, so it is the one thing validated first — before a health check is spent,
// and reported as an invalid URL rather than as a failed write, which names the wrong step.
func TestRouteAttachModeSkipsInstallAndStart(t *testing.T) {
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	env := routeEnv(t, home, state, "")

	t.Run("needs a base url", func(t *testing.T) {
		facts, code := runRoute(t, proj, env, "--plan", "--mode", "attach")
		if code != 0 || facts["reason"] != "attach_needs_base_url" {
			t.Errorf("plan: exit %d reason=%q, want 0/attach_needs_base_url", code, facts["reason"])
		}
		if f, c := runRoute(t, proj, env, "--mode", "attach"); c != 2 {
			t.Errorf("confirm without --base-url: exit %d, want 2 (%v)", c, f)
		}
	})
	t.Run("rejects a malformed one before doing anything", func(t *testing.T) {
		// The exact shape observed in the wild: no scheme, no port.
		facts, code := runRoute(t, proj, env, "--mode", "attach",
			"--base-url", "127.0.0.1/anthropic", "--no-health-check", "--scope", "project")
		if code != 2 || facts["reason"] != "invalid_base_url" {
			t.Errorf("exit %d reason=%q, want 2/invalid_base_url: %v", code, facts["reason"], facts)
		}
		if facts["detail"] != "no_scheme" {
			t.Errorf("detail=%q, want no_scheme so the caller can say WHY", facts["detail"])
		}
		if _, err := os.Stat(filepath.Join(proj, ".claude", "settings.local.json")); err == nil {
			t.Error("a malformed base URL was written into a routing key")
		}
	})
	t.Run("a base url in local mode is refused", func(t *testing.T) {
		facts, code := runRoute(t, proj, env, "--plan", "--scope", "project",
			"--base-url", "https://gw/anthropic")
		if code != 0 || facts["reason"] != "base_url_is_local_mode_nonsense" {
			t.Errorf("plan: exit %d reason=%q: in local mode the URL is derived, so accepting one "+
				"would reopen the class this design closes", code, facts["reason"])
		}
	})
	t.Run("plans no binary and no start", func(t *testing.T) {
		facts, code := runRoute(t, proj, env, "--plan", "--mode", "attach",
			"--base-url", "https://gw.internal/anthropic")
		if code != 0 {
			t.Fatalf("exit %d: %v", code, facts)
		}
		if !strings.Contains(facts["binary"], "not needed") {
			t.Errorf("binary=%q; attach must not plan a binary install", facts["binary"])
		}
		if facts["health_url"] != "https://gw.internal/healthz" {
			t.Errorf("health_url=%q; it should sit beside the base URL, not on a local port",
				facts["health_url"])
		}
	})
}

// fakeProxyDir writes a fake context-guru-proxy that answers /healthz on `port`, and returns the
// directory to put on PATH.
//
// It MUST answer --version and exit: install.sh probes the installed version by running the binary,
// and the first version of this fixture served forever on that probe, hanging the run before a
// single assertion could fail. A fake that does not exit where the real one does is not a fake.
func fakeProxyDir(t *testing.T, port string, listen bool) string {
	t.Helper()
	py := requireTool(t, "python3")
	dir := t.TempDir()
	body := "sleep 300\n"
	if listen {
		body = "exec " + py + " -c '\n" +
			"import http.server\n" +
			"class H(http.server.BaseHTTPRequestHandler):\n" +
			"    def do_GET(self):\n" +
			"        self.send_response(200); self.end_headers(); self.wfile.write(b\"ok\")\n" +
			"    def log_message(self, *a): pass\n" +
			"http.server.HTTPServer((\"127.0.0.1\", " + port + "), H).serve_forever()\n'\n"
	}
	script := "#!/usr/bin/env bash\n" +
		"if [ \"$1\" = --version ]; then echo 'context-guru-proxy vfake (commit none)'; exit 0; fi\n" +
		body
	if err := os.WriteFile(filepath.Join(dir, "context-guru-proxy"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestRouteWritesRoutingOnlyAfterSomethingAnswers is the ordering property, from both sides.
//
// The failure it prevents was observed in a real session driving the old prose: the routing key was
// written, the proxy had not been started yet, and the session's next API call went to 127.0.0.1 and
// died with Connection refused before reaching the step that starts the proxy. The installer
// produced the hang state the whole design exists to avoid.
func TestRouteWritesRoutingOnlyAfterSomethingAnswers(t *testing.T) {
	t.Run("a healthy proxy: routing is written", func(t *testing.T) {
		home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
		port := freePort(t)
		writePluginOptions(t, home, map[string]any{"port": port})
		env := routeEnv(t, home, state, fakeProxyDir(t, port, true))
		facts, code := runRoute(t, proj, env, consentOK()...)
		t.Cleanup(func() {
			if b, e := os.ReadFile(filepath.Join(state, "context-guru", "proxy-"+port+".pid")); e == nil {
				exec.Command("kill", strings.TrimSpace(string(b))).Run() //nolint:errcheck
			}
		})
		if code != 0 || facts["result"] != "routed" {
			t.Fatalf("exit %d result=%q: %v", code, facts["result"], facts)
		}
		b, err := os.ReadFile(filepath.Join(proj, ".claude", "settings.local.json"))
		if err != nil {
			t.Fatalf("no settings written: %v", err)
		}
		if !strings.Contains(string(b), "http://127.0.0.1:"+port+"/anthropic") {
			t.Errorf("the routing key does not name the resolved port:\n%s", b)
		}
		// The strategy has to be written BEFORE the proxy starts, or it does nothing until something
		// restarts it — start-proxy.sh reads that file only when it starts one.
		if _, err := os.Stat(filepath.Join(state, "context-guru", "keepalive-"+port+".yaml")); err != nil {
			t.Errorf("no cache strategy config was written before the proxy started: %v", err)
		}
		// And the undo has to be reported, because it is the only thing that works when routing breaks.
		if facts["reset_hatch"] == "" || facts["reset_hatch"] == "unavailable" {
			t.Errorf("reset_hatch=%q; the skill is required to print this verbatim and cannot if it "+
				"is not reported", facts["reset_hatch"])
		}
	})

	t.Run("a proxy that never answers: settings are NOT touched", func(t *testing.T) {
		home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
		port := freePort(t)
		writePluginOptions(t, home, map[string]any{"port": port})
		env := routeEnv(t, home, state, fakeProxyDir(t, port, false))
		facts, code := runRoute(t, proj, env, consentOK()...)
		if code == 0 || facts["reason"] != "health_check_failed" {
			t.Fatalf("exit %d reason=%q, want nonzero/health_check_failed: %v",
				code, facts["reason"], facts)
		}
		if _, err := os.Stat(filepath.Join(proj, ".claude", "settings.local.json")); err == nil {
			b, _ := os.ReadFile(filepath.Join(proj, ".claude", "settings.local.json"))
			t.Errorf("routing was written with nothing answering — a routed project with no proxy is "+
				"the hang state this ordering exists to prevent:\n%s", b)
		}
	})
}

// TestRouteInstallsStatuslineByDefault: the status line install now rides along with a successful
// route, IN WHATEVER FILE ROUTING ITSELF JUST USED — not unconditionally at user scope. That
// scope-following is the fix for the reported incident: a default (project-scope) install used to
// write and back up the user's machine-wide ~/.claude/settings.json as a side effect, every time.
// --no-statusline is still the opt-out, and a statusline write failure never turns a successful
// route into a reported failure — `statusline=skipped` alongside `result=routed`, not
// `result=error`.
func TestRouteInstallsStatuslineByDefault(t *testing.T) {
	t.Run("default scope (project): the status line follows routing into the project, "+
		"the machine-wide file is never created", func(t *testing.T) {
		home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
		port := freePort(t)
		writePluginOptions(t, home, map[string]any{"port": port})
		env := routeEnv(t, home, state, fakeProxyDir(t, port, true))
		facts, code := runRoute(t, proj, env, consentOK()...)
		t.Cleanup(func() {
			if b, e := os.ReadFile(filepath.Join(state, "context-guru", "proxy-"+port+".pid")); e == nil {
				exec.Command("kill", strings.TrimSpace(string(b))).Run() //nolint:errcheck
			}
		})
		if code != 0 || facts["result"] != "routed" {
			t.Fatalf("exit %d result=%q: %v", code, facts["result"], facts)
		}
		if facts["statusline"] != "on" {
			t.Fatalf("statusline=%q, want on: %v", facts["statusline"], facts)
		}
		got := readJSON(t, filepath.Join(proj, ".claude", "settings.local.json"))
		sl, _ := got["statusLine"].(map[string]any)
		// A REAL absolute path, resolved from install.sh's own $0 via route_here() — NOT the
		// literal ${CLAUDE_PLUGIN_ROOT} placeholder. That placeholder is undocumented (and, in
		// practice, never expanded) for `statusLine`: Claude Code's own plugins reference lists
		// exactly where it substitutes ${CLAUDE_PLUGIN_ROOT} — hook commands, MCP/LSP server
		// config, and skill/agent content — and `statusLine` is not among them. A previous version
		// of this test asserted the placeholder had to survive UN-expanded, on the theory that
		// Claude Code would expand it later when running the status line; it does not, so every
		// automatic install wrote a command that reported `statusline=on` and then rendered
		// nothing, forever. Fixed by resolving the real path at write time instead of deferring to
		// a substitution that was never going to happen.
		scriptsRoot, err := filepath.EvalSymlinks(scriptsDir(t))
		if err != nil {
			t.Fatal(err)
		}
		want := `python3 "` + filepath.Join(scriptsRoot, "statusline.py") + `"`
		if sl["command"] != want {
			t.Errorf("statusLine.command = %v, want %q", got["statusLine"], want)
		}
		if strings.Contains(fmt.Sprint(sl["command"]), "CLAUDE_PLUGIN_ROOT") {
			t.Errorf("the unexpandable placeholder leaked into the written command: %v", sl["command"])
		}
		// The literal reported bug: a project-scope install must never touch the machine-wide
		// file at all. `writePluginOptions` above already created it (as a fixture, to carry the
		// configured port) — the assertion is that the install added nothing to it and never
		// backed it up, not that the file is absent.
		homeSettings := filepath.Join(home, ".claude", "settings.json")
		if gotHome := readJSON(t, homeSettings); gotHome["statusLine"] != nil {
			t.Errorf("statusLine leaked into the machine-wide file from a project-scope install: %v",
				gotHome["statusLine"])
		}
		matches, err := filepath.Glob(filepath.Join(filepath.Dir(homeSettings), "context-guru-settings-json",
			"settings.context-guru-backup-*.json"))
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) != 0 {
			t.Errorf("a project-scope install backed up the machine-wide file, which is the "+
				"reported bug: %v", matches)
		}
	})

	t.Run("--scope user, consented: routing and statusline both land at user scope, "+
		"no second confirmation needed", func(t *testing.T) {
		home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
		port := freePort(t)
		writePluginOptions(t, home, map[string]any{"port": port})
		env := routeEnv(t, home, state, fakeProxyDir(t, port, true))
		facts, code := runRoute(t, proj, env, "--scope", "user", "--i-understand-machine-wide",
			"--i-consent-to-traffic-interception")
		t.Cleanup(func() {
			if b, e := os.ReadFile(filepath.Join(state, "context-guru", "proxy-"+port+".pid")); e == nil {
				exec.Command("kill", strings.TrimSpace(string(b))).Run() //nolint:errcheck
			}
		})
		if code != 0 || facts["result"] != "routed" {
			t.Fatalf("exit %d result=%q: %v", code, facts["result"], facts)
		}
		if facts["statusline"] != "on" {
			t.Fatalf("statusline=%q, want on: %v", facts["statusline"], facts)
		}
		got := readJSON(t, filepath.Join(home, ".claude", "settings.json"))
		sl, _ := got["statusLine"].(map[string]any)
		if sl["command"] == nil {
			t.Errorf("statusLine not written to the user-scope file: %v", got["statusLine"])
		}
		if strings.Contains(fmt.Sprint(sl["command"]), "CLAUDE_PLUGIN_ROOT") {
			t.Errorf("the unexpandable placeholder leaked into the written command: %v", sl["command"])
		}
		if _, err := os.Stat(filepath.Join(proj, ".claude", "settings.local.json")); !os.IsNotExist(err) {
			t.Errorf("a project-local file was created even though --scope user was chosen: %v", err)
		}
	})

	t.Run("--no-statusline: routing succeeds, nothing is written to statusLine", func(t *testing.T) {
		home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
		port := freePort(t)
		writePluginOptions(t, home, map[string]any{"port": port})
		env := routeEnv(t, home, state, fakeProxyDir(t, port, true))
		facts, code := runRoute(t, proj, env, consentOK("--no-statusline")...)
		t.Cleanup(func() {
			if b, e := os.ReadFile(filepath.Join(state, "context-guru", "proxy-"+port+".pid")); e == nil {
				exec.Command("kill", strings.TrimSpace(string(b))).Run() //nolint:errcheck
			}
		})
		if code != 0 || facts["result"] != "routed" {
			t.Fatalf("exit %d result=%q: %v", code, facts["result"], facts)
		}
		if facts["statusline"] != "skipped" {
			t.Fatalf("statusline=%q, want skipped: %v", facts["statusline"], facts)
		}
		got := readJSON(t, filepath.Join(proj, ".claude", "settings.local.json"))
		if _, has := got["statusLine"]; has {
			t.Error("--no-statusline was passed but statusLine was written anyway")
		}
	})

	t.Run("a pre-existing foreign statusLine in the project's OWN routing file: routing still "+
		"succeeds, statusline is reported skipped", func(t *testing.T) {
		home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
		port := freePort(t)
		writePluginOptions(t, home, map[string]any{"port": port})
		projFile := filepath.Join(proj, ".claude", "settings.local.json")
		if err := os.MkdirAll(filepath.Dir(projFile), 0o755); err != nil {
			t.Fatal(err)
		}
		writeJSON(t, projFile, map[string]any{
			"statusLine": map[string]any{"type": "command", "command": "my-own-thing"},
		})
		env := routeEnv(t, home, state, fakeProxyDir(t, port, true))
		facts, code := runRoute(t, proj, env, consentOK()...)
		t.Cleanup(func() {
			if b, e := os.ReadFile(filepath.Join(state, "context-guru", "proxy-"+port+".pid")); e == nil {
				exec.Command("kill", strings.TrimSpace(string(b))).Run() //nolint:errcheck
			}
		})
		if code != 0 || facts["result"] != "routed" {
			t.Fatalf("a foreign statusLine must not fail the route itself: exit %d result=%q: %v",
				code, facts["result"], facts)
		}
		if facts["statusline"] != "skipped" {
			t.Fatalf("statusline=%q, want skipped: %v", facts["statusline"], facts)
		}
		got := readJSON(t, projFile)
		sl, _ := got["statusLine"].(map[string]any)
		if sl["command"] != "my-own-thing" {
			t.Errorf("the user's own statusLine was overwritten: %v", got["statusLine"])
		}
	})
}

// TestRouteAddsGitignoreEntryByDefault: route_ensure_gitignore runs automatically alongside the
// statusline install, right after routing succeeds — deterministic and unasked, unlike the flags
// above that gate on the user's explicit consent, because this changes nothing about what the user
// is exposed to and the check for whether it is even needed is entirely mechanical. See
// settings.py's gitignore-ensure and install.sh's route_ensure_gitignore.
func TestRouteAddsGitignoreEntryByDefault(t *testing.T) {
	requireTool(t, "git")

	t.Run("a git repo with no existing .gitignore: the entry is added", func(t *testing.T) {
		home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
		if out, err := exec.Command("git", "-C", proj, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("git init: %v\n%s", err, out)
		}
		port := freePort(t)
		writePluginOptions(t, home, map[string]any{"port": port})
		env := routeEnv(t, home, state, fakeProxyDir(t, port, true))
		facts, code := runRoute(t, proj, env, consentOK()...)
		t.Cleanup(func() {
			if b, e := os.ReadFile(filepath.Join(state, "context-guru", "proxy-"+port+".pid")); e == nil {
				exec.Command("kill", strings.TrimSpace(string(b))).Run() //nolint:errcheck
			}
		})
		if code != 0 || facts["result"] != "routed" {
			t.Fatalf("exit %d result=%q: %v", code, facts["result"], facts)
		}
		if facts["gitignore"] != "added" {
			t.Fatalf("gitignore=%q, want added: %v", facts["gitignore"], facts)
		}
		gi, err := os.ReadFile(filepath.Join(proj, ".claude", ".gitignore"))
		if err != nil {
			t.Fatalf("no .gitignore was written: %v", err)
		}
		if !strings.Contains(string(gi), "context-guru-settings-json/") {
			t.Errorf(".gitignore does not cover the recovery folder:\n%s", gi)
		}
		// Not just a string in a file — git itself has to agree, since that is the actual property
		// this step exists for.
		out, err := exec.Command("git", "-C", filepath.Join(proj, ".claude"),
			"check-ignore", "-q", "context-guru-settings-json/").CombinedOutput()
		if err != nil {
			t.Errorf("git does not consider the recovery folder ignored: %v\n%s", err, out)
		}
	})

	t.Run("--no-gitignore-check: nothing is written", func(t *testing.T) {
		home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
		if out, err := exec.Command("git", "-C", proj, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("git init: %v\n%s", err, out)
		}
		port := freePort(t)
		writePluginOptions(t, home, map[string]any{"port": port})
		env := routeEnv(t, home, state, fakeProxyDir(t, port, true))
		facts, code := runRoute(t, proj, env, append(consentOK(), "--no-gitignore-check")...)
		t.Cleanup(func() {
			if b, e := os.ReadFile(filepath.Join(state, "context-guru", "proxy-"+port+".pid")); e == nil {
				exec.Command("kill", strings.TrimSpace(string(b))).Run() //nolint:errcheck
			}
		})
		if code != 0 || facts["result"] != "routed" {
			t.Fatalf("exit %d result=%q: %v", code, facts["result"], facts)
		}
		if facts["gitignore"] != "skipped" {
			t.Fatalf("gitignore=%q, want skipped: %v", facts["gitignore"], facts)
		}
		if _, err := os.Stat(filepath.Join(proj, ".claude", ".gitignore")); err == nil {
			t.Error("--no-gitignore-check wrote a .gitignore anyway")
		}
	})

	t.Run("not a git repo: skipped, and the route still succeeds", func(t *testing.T) {
		home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
		port := freePort(t)
		writePluginOptions(t, home, map[string]any{"port": port})
		env := routeEnv(t, home, state, fakeProxyDir(t, port, true))
		facts, code := runRoute(t, proj, env, consentOK()...)
		t.Cleanup(func() {
			if b, e := os.ReadFile(filepath.Join(state, "context-guru", "proxy-"+port+".pid")); e == nil {
				exec.Command("kill", strings.TrimSpace(string(b))).Run() //nolint:errcheck
			}
		})
		if code != 0 || facts["result"] != "routed" {
			t.Fatalf("exit %d result=%q: %v", code, facts["result"], facts)
		}
		if facts["gitignore"] != "skipped" {
			t.Fatalf("gitignore=%q, want skipped (no git repo): %v", facts["gitignore"], facts)
		}
	})
}

// TestRouteIsIdempotent. The install skill may be re-run, and on a hosted agent a re-run IS the
// repair path after an earlier attempt stopped partway. A second run must be a success that changes
// nothing, not a conflict against itself.
func TestRouteIsIdempotent(t *testing.T) {
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	port := freePort(t)
	writePluginOptions(t, home, map[string]any{"port": port})
	env := routeEnv(t, home, state, fakeProxyDir(t, port, true))
	t.Cleanup(func() {
		if b, e := os.ReadFile(filepath.Join(state, "context-guru", "proxy-"+port+".pid")); e == nil {
			exec.Command("kill", strings.TrimSpace(string(b))).Run() //nolint:errcheck
		}
	})
	if facts, code := runRoute(t, proj, env, consentOK()...); code != 0 {
		t.Fatalf("first run: exit %d %v", code, facts)
	}
	facts, code := runRoute(t, proj, env, consentOK()...)
	if code != 0 || facts["result"] != "routed" {
		t.Fatalf("second run: exit %d result=%q, want 0/routed: %v", code, facts["result"], facts)
	}
	if facts["settings_result"] != "unchanged" && facts["settings_result"] != "completed" {
		t.Errorf("settings_result=%q on a re-run; want unchanged or completed rather than a conflict "+
			"against our own key", facts["settings_result"])
	}
	// A re-run must also be distinguishable from a first install, or it gets narrated as one.
	if plan, _ := runRoute(t, proj, env, "--plan", "--scope", "project"); plan["already_routed"] != "true" {
		t.Errorf("already_routed=%q after installing; the model cannot tell a repair from a fresh "+
			"install and will describe the wrong thing", plan["already_routed"])
	}
}

// TestInstallSkillDelegatesRatherThanReimplementing. The point of the orchestrator is that the
// ordering and the option resolution stop being prose. If the skill still spells out the individual
// commands, both copies exist and the one a model follows is whichever it read last.
func TestInstallSkillDelegatesRatherThanReimplementing(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("skills", "install", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	if !strings.Contains(body, "--route --plan") {
		t.Error("the install skill never renders the plan, so the model is back to asking its " +
			"question with placeholders instead of the user's real file and endpoint")
	}
	// The `!` block is what makes the plan un-mistypable: it runs at render, so no model chose it.
	if !strings.Contains(body, "!`") {
		t.Error("the plan is not rendered through a `!` block, so the model must decide to run it " +
			"and can reword it — the failure mode the orchestrator exists to remove")
	}
	for _, banned := range []string{`settings.py" add`, `start-proxy.sh" --unrouted`} {
		if strings.Contains(body, banned) {
			t.Errorf("the install skill still spells out %q. The ordering between the individual "+
				"scripts is load-bearing and getting it wrong once killed the installing session; "+
				"two copies of it means a model can follow the wrong one.", banned)
		}
	}
	// Cut from 423 lines to roughly a third. Not a style preference: 39 of those lines were some
	// form of "do not improvise this", which is what prose has to do when it carries a mechanism.
	//
	// Raised 200 -> 220 when per-project port allocation added three decision branches the script
	// can hand back (project_installs_exist, port_owned_by_another_project, port_changed_since_plan).
	// Relaying a `result=`/`reason=` the script produced is the skill's job and not duplicated
	// mechanism — the `banned` check above is what actually enforces that distinction, and this
	// number only keeps the prose from creeping back. Raise it for a new branch; never to make room
	// for a worked example or a second copy of an ordering.
	//
	// Raised 220 -> 235 for three more things the script hands back and the skill can only relay:
	// the `port_recorded_by_another_project` refusal, and the two per-adopted-project facts
	// (`adopted_project_still_overriding`, `adopted_proxy_left_running`) that a machine-wide install
	// must not fold into "adopted". Relaying, not mechanism — same distinction as above.
	if n := strings.Count(body, "\n"); n > 235 {
		t.Errorf("the install skill is %d lines; it delegates the mechanism now, so it should be "+
			"well under 235", n)
	}
}

// TestRoutePlanAlwaysExitsZero is the regression test for the defect that a real sandboxed session
// found and no unit test could have.
//
// The install skill runs `--route --plan` from a `!` block, which pre-executes at render. The first
// version of this script exited 2 when a base URL was already set — the commonest state of a hosted
// machine, and precisely the case the plan exists to describe. Invoked for real, the whole skill then
// produced NO OUTPUT AT ALL: the model never saw the plan, never asked its question, and the run
// looked successful because nothing had been written.
//
// So the rule is flat, and it is cheaper to assert than to reason about: a plan REPORTS. Every
// condition it can discover is a fact on stdout with exit 0. Only a command that actually declines
// to act — the writing path — exits non-zero.
func TestRoutePlanAlwaysExitsZero(t *testing.T) {
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	base := routeEnv(t, home, state, "")
	withTheirs := append(routeEnv(t, home, state, ""),
		"ANTHROPIC_BASE_URL=https://gateway.corp.example/v1")

	for _, tc := range []struct {
		name string
		env  []string
		args []string
	}{
		{"clean project", base, []string{"--plan", "--scope", "project"}},
		{"a base URL already set", withTheirs, []string{"--plan", "--scope", "project"}},
		{"machine-wide without the flag", base, []string{"--plan", "--scope", "user"}},
		{"attach with no base url", base, []string{"--plan", "--mode", "attach"}},
		{"attach with a malformed base url", base,
			[]string{"--plan", "--mode", "attach", "--base-url", "127.0.0.1/anthropic"}},
		{"a base url in local mode", base,
			[]string{"--plan", "--scope", "project", "--base-url", "https://gw/anthropic"}},
		{"team scope", base, []string{"--plan", "--scope", "team"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts, code := runRoute(t, proj, tc.env, tc.args...)
			if code != 0 {
				t.Errorf("exit %d, want 0. A `!` block that exits non-zero can take the whole skill "+
					"invocation with it, and then the model sees nothing at all: %v", code, facts)
			}
			// Exit 0 is not enough on its own — it must also SAY something the caller can branch on,
			// or a silent success is just a different way of proving nothing.
			switch facts["result"] {
			case "planned", "needs_decision":
			default:
				t.Errorf("result=%q; a plan must report either `planned` or `needs_decision`",
					facts["result"])
			}
		})
	}
}

// TestInstallSkillDoesNotGrantItselfTheGatedCommand.
//
// Measured in a real sandboxed session: with `allowed-tools: Bash(.../install.sh)` in the skill's
// frontmatter, the command that starts a traffic-intercepting proxy and repoints
// ANTHROPIC_BASE_URL ran with NO prompt at all. The `!` block needs no such grant — it pre-executes
// at render — so the only thing that line can grant is the one step whose gate this design calls a
// feature.
//
// A plugin declaring the permission the classifier exists to ask about is the plugin answering a
// question that belongs to its user. That is worth a test rather than a comment, because adding an
// allowed-tools line is an obvious "fix" for anyone who finds the prompt annoying.
func TestInstallSkillDoesNotGrantItselfTheGatedCommand(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("skills", "install", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	front, _, ok := strings.Cut(strings.TrimPrefix(body, "---\n"), "\n---")
	if !ok {
		t.Fatal("no frontmatter in skills/install/SKILL.md")
	}
	for _, line := range strings.Split(front, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "allowed-tools:") {
			t.Errorf("the install skill declares %q. That pre-approves the one command that redirects "+
				"the user's model traffic, which was measured to run with no prompt at all. The `!` "+
				"block does not need it.", strings.TrimSpace(line))
		}
	}
	// And the reason has to stay written down, or the line comes back.
	if !strings.Contains(body, "allowed-tools") {
		t.Error("no note explaining why there is no allowed-tools; without it the next person to " +
			"find the prompt annoying will simply add one")
	}
}

// TestRouteRefusesWithoutExplicitConsent is the gate the owner chose over relying on the approval
// prompt, and the three measurements behind that choice are worth keeping next to the test:
//
//   - the auto-mode classifier is itself a model, so it is probabilistic. The same command string was
//     denied in one trial and allowed in two others (see #248's retracted finding 3).
//   - a skill can declare the prompt away. With `allowed-tools: Bash(.../install.sh)` in its
//     frontmatter, this exact command ran with NO prompt at all.
//   - in a non-interactive session there is no prompt, because there is no human to ask — and the
//     measured result was a complete, unattended install of a traffic interceptor.
//
// The third is what this gate is really for: an unattended run now FAILS CLOSED. What the gate cannot
// do is prove a human said yes, since a caller can pass the flag unprompted. It converts "we hope a
// prompt fires" into a deterministic precondition, which is a different and better kind of guarantee
// than none.
func TestRouteRefusesWithoutExplicitConsent(t *testing.T) {
	for _, mode := range []string{"local", "attach"} {
		t.Run(mode, func(t *testing.T) {
			home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
			port := freePort(t)
			writePluginOptions(t, home, map[string]any{"port": port})
			env := routeEnv(t, home, state, fakeProxyDir(t, port, true))
			args := []string{"--scope", "project"}
			if mode == "attach" {
				// attach starts nothing, but it still repoints the session's traffic, so it needs the
				// same consent. A gate that only covered the mode that runs a binary would be guarding
				// the wrong thing.
				args = append(args, "--mode", "attach",
					"--base-url", "http://127.0.0.1:"+port+"/anthropic")
			}
			facts, code := runRoute(t, proj, env, args...)
			if code != 2 || facts["reason"] != "consent_required" {
				t.Fatalf("exit %d reason=%q, want 2/consent_required: %v", code, facts["reason"], facts)
			}
			// Nothing at all may have happened. Not a settings file, not a proxy, not a strategy.
			for _, p := range []string{
				filepath.Join(proj, ".claude", "settings.local.json"),
				filepath.Join(state, "context-guru", "proxy-"+port+".pid"),
				filepath.Join(state, "context-guru", "keepalive-"+port+".yaml"),
			} {
				if _, err := os.Stat(p); err == nil {
					t.Errorf("refused for want of consent but created %s anyway", p)
				}
			}
			// The refusal has to hand back a runnable command, or a model reassembles one by hand —
			// which is exactly how a scheme-less URL got composed on 2026-09-14.
			if !strings.Contains(facts["confirm_command"], "--i-consent-to-traffic-interception") {
				t.Errorf("confirm_command=%q does not carry the consent flag", facts["confirm_command"])
			}
		})
	}
}

// TestRouteProceedsWithConsentAndThePrintedCommandWorksVerbatim. A refusal that hands back a command
// which does not actually work is worse than no suggestion at all: it invites the caller to improvise
// a fix. So run the printed command exactly as printed, through a shell, and require it to install.
func TestRouteProceedsWithConsentAndThePrintedCommandWorksVerbatim(t *testing.T) {
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	port := freePort(t)
	writePluginOptions(t, home, map[string]any{"port": port})
	env := routeEnv(t, home, state, fakeProxyDir(t, port, true))
	t.Cleanup(func() {
		if b, e := os.ReadFile(filepath.Join(state, "context-guru", "proxy-"+port+".pid")); e == nil {
			exec.Command("kill", strings.TrimSpace(string(b))).Run() //nolint:errcheck
		}
	})

	plan, code := runRoute(t, proj, env, "--plan", "--scope", "project")
	if code != 0 {
		t.Fatalf("plan: exit %d %v", code, plan)
	}
	if plan["consent_required"] != "true" {
		t.Errorf("the plan does not advertise consent_required, so the model has no reason to ask "+
			"before running the command: %v", plan)
	}
	cmdline := plan["confirm_command"]
	if cmdline == "" {
		t.Fatal("the plan printed no confirm_command")
	}

	cmd := exec.Command("bash", "-c", cmdline)
	cmd.Dir = proj
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	t.Logf("verbatim confirm_command -> %v\n%s", err, out)
	if !strings.Contains(string(out), "result=routed") {
		t.Errorf("the command the plan printed did not complete the install when run verbatim:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(proj, ".claude", "settings.local.json")); err != nil {
		t.Errorf("no routing written by the printed command: %v", err)
	}
}

// TestRouteConfirmCommandCarriesEveryDecision. The point of printing it is that the caller appends
// nothing. If the decisions the plan resolved are missing from it, a model has to reassemble the
// command — the failure that produced a scheme-less URL on a colleague's machine.
func TestRouteConfirmCommandCarriesEveryDecision(t *testing.T) {
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	env := append(routeEnv(t, home, state, ""),
		"ANTHROPIC_BASE_URL=https://gateway.corp.example/v1")
	facts, code := runRoute(t, proj, env, "--plan", "--scope", "user",
		"--i-understand-machine-wide", "--on-conflict", "chain", "--cache-strategy", "none",
		"--upstream", "https://gw.example/v1", "--health-url", "http://127.0.0.1:9/healthz",
		"--no-health-check")
	if code != 0 {
		t.Fatalf("exit %d: %v", code, facts)
	}
	for _, want := range []string{
		"--scope user",
		"--on-conflict chain",
		"--i-understand-machine-wide",
		// Three more that travelled in argv and were missing from the printed line. --upstream is the
		// consequential one: it decides what gets WRITTEN, so dropping it made the install record an
		// upstream the caller never asked for. The other two decide how the install VERIFIES itself,
		// and a dropped verification decision is the quieter half of the same defect.
		"--upstream https://gw.example/v1",
		"--health-url http://127.0.0.1:9/healthz",
		"--no-health-check",
		// The decision that spends the user's money was missing from both the command and this list,
		// which is how the drop shipped past a suite that otherwise covers this area well. `none` is
		// the value worth pinning: the regression is a user asking NOT to spend and being charged.
		"--cache-strategy none",
		"--i-consent-to-traffic-interception",
	} {
		if !strings.Contains(facts["confirm_command"], want) {
			t.Errorf("confirm_command is missing %q, so the caller must add it by hand:\n%s",
				want, facts["confirm_command"])
		}
	}
}

// TestInstallSkillAsksForConsentAsAChoice. The gate is only half the mechanism; the other half is
// that a human is actually asked. A skill that mentions the flag without saying how to obtain a yes
// invites a model to append it to make a refusal go away — which is the one thing it must not be.
func TestInstallSkillAsksForConsentAsAChoice(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("skills", "install", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	if !strings.Contains(body, "--i-consent-to-traffic-interception") {
		t.Fatal("the install skill never mentions the consent flag, so the install cannot complete")
	}
	// A pickable choice, not prose to skim.
	if !strings.Contains(body, "AskUserQuestion") {
		t.Error("the skill does not tell the model to ask with AskUserQuestion; a two-option choice " +
			"is much harder to answer sideways than a sentence ending in a question mark")
	}
	// And the absence of an answer must be a refusal, or an unattended run proceeds by default —
	// the exact behaviour this gate was added to stop.
	lowered := strings.ToLower(body)
	if !strings.Contains(lowered, "a silent or absent answer is a no") {
		t.Error("the skill does not say that a silent or absent answer means NO. Without that, a " +
			"non-interactive session has no instruction against passing the flag unasked")
	}
	if !strings.Contains(lowered, "do not pass") {
		t.Error("the skill never prohibits passing the consent flag on the model's own judgement")
	}
}

// TestGitignoreEnsure covers settings.py's `gitignore-ensure` subcommand directly (install.sh's
// TestRouteAddsGitignoreEntryByDefault covers it end-to-end through a real route). Every branch
// must fail open — this must never block or fail an install — which is why "not a git repo" and
// "cannot write" report `result=skipped`, never a nonzero exit.
func TestGitignoreEnsure(t *testing.T) {
	requireTool(t, "git")

	t.Run("not a git repo: skipped, nothing written", func(t *testing.T) {
		state, home, dir := t.TempDir(), t.TempDir(), t.TempDir()
		path := filepath.Join(dir, ".claude", "settings.local.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		writeJSON(t, path, map[string]any{})
		facts, code := settingsIn(t, state, home, "gitignore-ensure", "--file", path)
		if code != 0 || facts["result"] != "skipped" || facts["reason"] != "not_a_git_repo" {
			t.Fatalf("exit %d, %v", code, facts)
		}
		if _, err := os.Stat(filepath.Join(dir, ".claude", ".gitignore")); err == nil {
			t.Error("wrote a .gitignore outside any git repo")
		}
	})

	t.Run("a git repo with no .gitignore: the entry is added", func(t *testing.T) {
		state, home, dir := t.TempDir(), t.TempDir(), t.TempDir()
		if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("git init: %v\n%s", err, out)
		}
		claudeDir := filepath.Join(dir, ".claude")
		path := filepath.Join(claudeDir, "settings.local.json")
		if err := os.MkdirAll(claudeDir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeJSON(t, path, map[string]any{})
		facts, code := settingsIn(t, state, home, "gitignore-ensure", "--file", path)
		if code != 0 || facts["result"] != "added" {
			t.Fatalf("exit %d, %v", code, facts)
		}
		wantFile := filepath.Join(claudeDir, ".gitignore")
		if facts["file"] != wantFile {
			t.Errorf("file=%q, want %q", facts["file"], wantFile)
		}
		gi, err := os.ReadFile(wantFile)
		if err != nil {
			t.Fatalf("no .gitignore written: %v", err)
		}
		if !strings.Contains(string(gi), "context-guru-settings-json/") {
			t.Errorf(".gitignore does not cover the recovery folder:\n%s", gi)
		}
		out, err := exec.Command("git", "-C", claudeDir, "check-ignore", "-q",
			"context-guru-settings-json/").CombinedOutput()
		if err != nil {
			t.Errorf("git does not consider the folder ignored: %v\n%s", err, out)
		}

		// Idempotent: a second run against the same file must not duplicate the line or rewrite it.
		facts2, code2 := settingsIn(t, state, home, "gitignore-ensure", "--file", path)
		if code2 != 0 || facts2["result"] != "unchanged" {
			t.Fatalf("second run: exit %d, %v", code2, facts2)
		}
		gi2, err := os.ReadFile(wantFile)
		if err != nil {
			t.Fatal(err)
		}
		if string(gi2) != string(gi) {
			t.Errorf("a no-op second run changed the file:\nbefore: %q\nafter:  %q", gi, gi2)
		}
	})

	t.Run("already covered by an existing .gitignore: reported unchanged, file untouched", func(t *testing.T) {
		state, home, dir := t.TempDir(), t.TempDir(), t.TempDir()
		if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("git init: %v\n%s", err, out)
		}
		claudeDir := filepath.Join(dir, ".claude")
		path := filepath.Join(claudeDir, "settings.local.json")
		if err := os.MkdirAll(claudeDir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeJSON(t, path, map[string]any{})
		// A broader rule at the REPO root already covers it — this is the common real-world shape
		// (a project .gitignore with `.claude/` or similar), not the exact line settings.py itself
		// would have written.
		if err := os.WriteFile(filepath.Join(dir, ".gitignore"),
			[]byte("*context-guru-settings-json*\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		facts, code := settingsIn(t, state, home, "gitignore-ensure", "--file", path)
		if code != 0 || facts["result"] != "unchanged" || facts["reason"] != "already_ignored" {
			t.Fatalf("exit %d, %v", code, facts)
		}
		if _, err := os.Stat(filepath.Join(claudeDir, ".gitignore")); err == nil {
			t.Error("wrote a redundant .gitignore beside the file when a broader rule already covered it")
		}
	})

	t.Run("unwritable directory: skipped, never fails the caller", func(t *testing.T) {
		state, home, dir := t.TempDir(), t.TempDir(), t.TempDir()
		if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("git init: %v\n%s", err, out)
		}
		claudeDir := filepath.Join(dir, ".claude")
		path := filepath.Join(claudeDir, "settings.local.json")
		// Created writable, populated, THEN locked down — a directory that starts read-only would
		// refuse the file write below too, which is not the condition this subtest means to isolate.
		if err := os.MkdirAll(claudeDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(claudeDir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(claudeDir, 0o755) }) //nolint:errcheck
		facts, code := settingsIn(t, state, home, "gitignore-ensure", "--file", path)
		if code != 0 || facts["result"] != "skipped" || facts["reason"] != "unwritable" {
			t.Fatalf("an unwritable directory must be reported, never fail the caller: exit %d, %v",
				code, facts)
		}
	})
}

// TestValidBaseURLRefusesAHostThatIsNotAHost. The host was checked for emptiness and nothing else,
// which would be a modest gap if the approved string stayed data. It does not: install.sh interpolates
// it into `confirm_command=`, SKILL.md tells the model to run that verbatim, and this suite runs it
// through `bash -c`. So "is this a URL we will write" was also, in effect, "is this safe to put in a
// command line", and it answered yes to `http://x;whoami/anthropic`.
func TestValidBaseURLRefusesAHostThatIsNotAHost(t *testing.T) {
	state, home := t.TempDir(), t.TempDir()
	for _, bad := range []string{
		"http://x;whoami/anthropic", // command separator
		"http://$(id)/anthropic",    // command substitution
		"http://`id`/anthropic",     // the older spelling of the same
		"http://a b/anthropic",      // argument splitting
		"http://x@y/anthropic",      // userinfo, i.e. a host that is not the host
		"http://-/anthropic",        // reads as a flag once it reaches a command line
		"http://x|y/anthropic",      // pipe
		"http://x&y/anthropic",      // background/AND
	} {
		facts, code := settingsIn(t, state, home, "check-url", "--url", bad)
		if code == 0 || facts["detail"] != "bad_host" {
			t.Errorf("check-url %q -> exit %d detail=%q; want a bad_host refusal. This string reaches a "+
				"shell command line the skill tells a model to run verbatim", bad, code, facts["detail"])
		}
	}

	// The control half. A guard that refuses real gateways is a different defect, not a fix — and the
	// underscore case is deliberate: it is not strictly legal in a hostname, internal gateways use it
	// anyway, and it is shell-inert, so refusing it would block installs to buy nothing.
	for _, good := range []string{
		"http://127.0.0.1:8787/anthropic",
		"https://gw.internal/anthropic",
		"https://gw-1.corp_int.example:4000/anthropic",
		"http://[::1]:8787/anthropic",
	} {
		facts, code := settingsIn(t, state, home, "check-url", "--url", good)
		if code != 0 || facts["result"] != "ok" {
			t.Errorf("check-url %q -> exit %d %v; refusing a legitimate endpoint is its own defect",
				good, code, facts)
		}
	}
}

// TestRouteConfirmCommandCannotCarryShellSyntax is the second half of the same defect, and it is the
// half that keeps working when the first is bypassed.
//
// The host character class cannot save this case: the URL below is a LEGITIMATE loopback URL by every
// rule valid_base_url() enforces — real host, real port, path ending in `anthropic` — and it still
// carries `;` in its PATH, which valid_base_url() does not police and should not have to. The only
// thing standing between that and execution is that route_confirm_command quotes what it interpolates.
//
// Asserted on the marker file rather than the exit code on purpose: the install is expected to fail
// (there is nothing at that URL). What must not happen is the payload running, and it ran BEFORE any
// consent answer in the original, because it rides the string `--plan` prints.
func TestRouteConfirmCommandCannotCarryShellSyntax(t *testing.T) {
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	marker := filepath.Join(t.TempDir(), "PWNED")
	port := freePort(t)
	writePluginOptions(t, home, map[string]any{"port": port})
	env := routeEnv(t, home, state, "")

	hostile := "http://127.0.0.1:" + port + "/;touch " + marker + ";/anthropic"

	// It really is accepted by the validator — otherwise this test would be proving nothing about the
	// quoting, which is the thing it exists to pin.
	if facts, code := settingsIn(t, state, home, "check-url", "--url", hostile); code != 0 {
		t.Skipf("check-url now refuses %q (%v); this case no longer isolates the quoting layer",
			hostile, facts)
	}

	plan, code := runRoute(t, proj, env, "--plan", "--mode", "attach", "--base-url", hostile)
	if code != 0 {
		t.Fatalf("plan: exit %d %v", code, plan)
	}
	cmdline := plan["confirm_command"]
	if cmdline == "" {
		t.Fatal("no confirm_command to test")
	}
	cmd := exec.Command("bash", "-c", cmdline)
	cmd.Dir = proj
	cmd.Env = env
	out, _ := cmd.CombinedOutput()
	t.Logf("confirm_command run verbatim:\n%s\n%s", cmdline, out)

	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("the command printed by --plan executed an injected payload (%s exists). It fires "+
			"whenever that string reaches a shell, which is upstream of the consent gate entirely",
			marker)
	}
}

// TestRouteDecidesProvenanceFromTheRecordNotTheURLShape. `route_inspect` answered "is this already
// ours?" by matching `127.0.0.1:<port>` against the current value — the one inference
// valid_base_url()'s own docstring forbids, because two local proxies are indistinguishable by URL.
// The consequence was not cosmetic: somebody else's proxy on our port was reported as ours with
// `existing_base_url=` emptied, so the single question this design says can never be defaulted was
// never asked, and the skill narrated it as "a re-run or a repair".
func TestRouteDecidesProvenanceFromTheRecordNotTheURLShape(t *testing.T) {
	port := freePort(t)

	t.Run("a foreign URL on our own port is a conflict", func(t *testing.T) {
		home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
		writePluginOptions(t, home, map[string]any{"port": port})
		// No `$context-guru` record: this file was written by something else. Same URL we would use.
		foreign := "http://127.0.0.1:" + port + "/anthropic"
		if err := os.MkdirAll(filepath.Join(proj, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeJSON(t, filepath.Join(proj, ".claude", "settings.local.json"),
			map[string]any{"env": map[string]any{"ANTHROPIC_BASE_URL": foreign}})

		facts, code := runRoute(t, proj, routeEnv(t, home, state, ""), "--plan", "--scope", "project")
		if code != 0 {
			t.Fatalf("plan: exit %d %v", code, facts)
		}
		if facts["already_routed"] == "true" {
			t.Errorf("claimed somebody else's endpoint as ours. With port 4000 this is litellm's own "+
				"default, so this is how an install would silently take over a working gateway: %v", facts)
		}
		if facts["existing_base_url"] == "" {
			t.Errorf("existing_base_url was emptied, so the conflict is never reported and the one "+
				"question that cannot be defaulted is never asked: %v", facts)
		}
	})

	t.Run("a different port is not our port", func(t *testing.T) {
		home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
		writePluginOptions(t, home, map[string]any{"port": port})
		// The old match was an unanchored substring, so 8787 matched an endpoint on 87870.
		if err := os.MkdirAll(filepath.Join(proj, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeJSON(t, filepath.Join(proj, ".claude", "settings.local.json"), map[string]any{
			"env": map[string]any{"ANTHROPIC_BASE_URL": "http://127.0.0.1:" + port + "0/anthropic"},
		})
		facts, code := runRoute(t, proj, routeEnv(t, home, state, ""), "--plan", "--scope", "project")
		if code != 0 {
			t.Fatalf("plan: exit %d %v", code, facts)
		}
		if facts["already_routed"] == "true" {
			t.Errorf("port %s0 was matched as port %s: %v", port, port, facts)
		}
	})

	// The other direction, which is what makes the fix a fix rather than a refusal to answer: OUR OWN
	// install must still be recognised, from the record this time, so a re-run is not narrated as fresh.
	t.Run("our own install is still recognised", func(t *testing.T) {
		home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
		p := freePort(t)
		writePluginOptions(t, home, map[string]any{"port": p})
		env := routeEnv(t, home, state, fakeProxyDir(t, p, true))
		t.Cleanup(func() { stopFakeProxy(t, state, p) })

		if facts, code := runRoute(t, proj, env, consentOK()...); code != 0 {
			t.Fatalf("first install: exit %d %v", code, facts)
		}
		facts, code := runRoute(t, proj, env, "--plan", "--scope", "project")
		if code != 0 {
			t.Fatalf("plan: exit %d %v", code, facts)
		}
		if facts["already_routed"] != "true" {
			t.Errorf("our own routing was not recognised, so a re-run gets narrated as a fresh "+
				"install: %v", facts)
		}
	})
}

// TestRouteRefusesAnUnknownStrategyBeforeTouchingAnything. `settings.py strategy set` refuses a typo
// with exit 2, and install.sh turned that refusal into `strategy_warning=` with `|| true`. No config
// file written means `split`, so `--cache-strategy 5-minute-ping` installed a DIFFERENT mechanism from
// the one named and reported `result=routed`. That is the same shape as the unknown flag this script
// already refuses: a dropped decision reporting success.
func TestRouteRefusesAnUnknownStrategyBeforeTouchingAnything(t *testing.T) {
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	port := freePort(t)
	writePluginOptions(t, home, map[string]any{"port": port})
	env := routeEnv(t, home, state, fakeProxyDir(t, port, true))

	facts, code := runRoute(t, proj, env, consentOK("--cache-strategy", "5-minute-ping")...)
	if code != 2 || facts["reason"] != "unknown_strategy" {
		t.Fatalf("exit %d reason=%q, want 2/unknown_strategy: %v", code, facts["reason"], facts)
	}
	// Before the binary, before the proxy, before the settings file.
	for _, p := range []string{
		filepath.Join(proj, ".claude", "settings.local.json"),
		filepath.Join(state, "context-guru", "proxy-"+port+".pid"),
	} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("refused the strategy name but created %s anyway", p)
		}
	}
	// The names have to be in the refusal, or the model guesses — and the names differ in whether they
	// spend the user's money, which is not a thing to guess at.
	if !strings.Contains(facts["note"], "5-min-ping") {
		t.Errorf("the refusal does not list the real strategy names: note=%q", facts["note"])
	}
}

// TestRouteReportsAProxyItLeftRunning. "SETTINGS WERE NOT TOUCHED" is true, and "nothing happened" is
// what a caller infers from it — the skill was instructing the model to say "nothing was written; the
// project is unrouted, which is a working project", and the first clause was false. This matters most
// when the health check fails TRANSIENTLY: the user is told nothing happened, and their retry meets a
// stale pidfile on an occupied port.
func TestRouteReportsAProxyItLeftRunning(t *testing.T) {
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	port := freePort(t)
	writePluginOptions(t, home, map[string]any{"port": port})
	// listen=false: it starts, it stays up, it never answers /healthz.
	env := routeEnv(t, home, state, fakeProxyDir(t, port, false))
	t.Cleanup(func() { stopFakeProxy(t, state, port) })

	facts, code := runRoute(t, proj, env, consentOK()...)
	if facts["reason"] != "health_check_failed" {
		t.Fatalf("exit %d, want a failed health check: %v", code, facts)
	}
	pidfile := filepath.Join(state, "context-guru", "proxy-"+port+".pid")
	b, err := os.ReadFile(pidfile)
	if err != nil {
		t.Skipf("no proxy was started, so there is no side effect to report: %v", err)
	}
	// Ground truth first: this test is only meaningful if something really is still running.
	if err := exec.Command("kill", "-0", strings.TrimSpace(string(b))).Run(); err != nil {
		t.Skipf("the fake proxy is not running, so there is nothing to report: %v", err)
	}
	if facts["proxy_started"] != "true" {
		t.Errorf("a proxy IS running on port %s and the report does not say so. The caller is told "+
			"only that settings were untouched, and will report that nothing happened: %v", port, facts)
	}
	if facts["pidfile"] == "" || facts["stop_command"] == "" {
		t.Errorf("no pidfile= or stop_command=, so the user is left with a listener and no handle "+
			"on it: %v", facts)
	}
}

// TestRoutePlanCarriesTheConsentQuestionIncludingTheSpend.
//
// The gate guards the ACT and has to name the TERMS. `--i-consent-to-traffic-interception` is spelled
// to gate "traffic gets intercepted", while what the user is asked also covers scope and which cache
// strategy — and the default one spends their own quota while nobody is at the keyboard. A question
// narrower than the command it authorises is not consent to that command, so the question is generated
// from the same resolved facts as the command rather than from the skill's example paragraph.
func TestRoutePlanCarriesTheConsentQuestionIncludingTheSpend(t *testing.T) {
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	writePluginOptions(t, home, map[string]any{"port": freePort(t)})
	env := routeEnv(t, home, state, "")

	facts, code := runRoute(t, proj, env, "--plan", "--scope", "project")
	if code != 0 {
		t.Fatalf("plan: exit %d %v", code, facts)
	}
	q := facts["consent_question"]
	if q == "" {
		t.Fatal("the plan prints no consent_question=, so the question a model asks is composed from " +
			"prose and can drift from the command it authorises")
	}
	if !strings.Contains(q, "SPENDS") {
		t.Errorf("the default strategy spends the user's own quota and the question does not say so. "+
			"That is the half of the proposition a user would most want to have been asked: %q", q)
	}

	// And with a strategy that does not spend, it must not claim otherwise.
	facts, code = runRoute(t, proj, env, "--plan", "--scope", "project", "--cache-strategy", "none")
	if code != 0 {
		t.Fatalf("plan: exit %d %v", code, facts)
	}
	if strings.Contains(facts["consent_question"], "SPENDS") {
		t.Errorf("`none` spends nothing, so warning about spending is a false statement in the one "+
			"sentence the user is asked to agree to: %q", facts["consent_question"])
	}
}

// TestRouteHonoursTheStrategyThroughThePrintedCommand is the end-to-end of the money finding: not just
// that the flag appears in `confirm_command=`, but that running that line actually leaves `split`
// armed. The regression it guards is a user asking not to spend and being charged anyway.
func TestRouteHonoursTheStrategyThroughThePrintedCommand(t *testing.T) {
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	port := freePort(t)
	writePluginOptions(t, home, map[string]any{"port": port})
	env := routeEnv(t, home, state, fakeProxyDir(t, port, true))
	t.Cleanup(func() { stopFakeProxy(t, state, port) })

	plan, code := runRoute(t, proj, env, "--plan", "--scope", "project", "--cache-strategy", "none")
	if code != 0 {
		t.Fatalf("plan: exit %d %v", code, plan)
	}
	cmd := exec.Command("bash", "-c", plan["confirm_command"])
	cmd.Dir = proj
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	t.Logf("verbatim -> %v\n%s", err, out)
	if !strings.Contains(string(out), "result=routed") {
		t.Fatalf("the printed command did not complete the install:\n%s", out)
	}
	// `split` IS the absence of a keepalive config. Its presence means the spending strategy was armed.
	if _, err := os.Stat(filepath.Join(state, "context-guru", "keepalive-"+port+".yaml")); err == nil {
		t.Error("the user asked for `split` and a keepalive config was written anyway, so the install " +
			"spends their quota while reporting success")
	}
}

// stopFakeProxy kills whatever the run under test left listening. Pidfile-first and best-effort: a
// `pkill` pattern is out of the question here, because these eval boxes are shared with another
// engineer and with other sessions running as the same unix user.
// healthzServerSrc is the smallest thing that answers /healthz on a port: a real process, so a test
// can seed its pid and let the script under test stop it.
func healthzServerSrc(port string) string {
	return "import http.server\n" +
		"class H(http.server.BaseHTTPRequestHandler):\n" +
		"    def do_GET(self):\n" +
		"        self.send_response(200); self.end_headers(); self.wfile.write(b\"ok\")\n" +
		"    def log_message(self, *a): pass\n" +
		"http.server.HTTPServer((\"127.0.0.1\", " + port + "), H).serve_forever()\n"
}

// shellQuote wraps s for a single-quoted shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// waitForHealthz fails the test if nothing answers within a few seconds: a test that proceeds
// against a port nothing is serving is testing the cold-start path by accident.
func waitForHealthz(t *testing.T, port string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://127.0.0.1:" + port + "/healthz")
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("nothing answered /healthz on port %s", port)
}

func stopFakeProxy(t *testing.T, state, port string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(state, "context-guru", "proxy-"+port+".pid"))
	if err != nil {
		return
	}
	if pid := strings.TrimSpace(string(b)); pid != "" {
		exec.Command("kill", pid).Run() //nolint:errcheck
	}
}

// TestInstallSkillRunsThePrintedCommandRatherThanAnExample. The step-2 block used to be a hand-written
// command, and it contradicted the bullet directly under it telling the model to run `confirm_command=`
// as printed. Two concrete defects in one block: it baked in `--on-conflict chain`, wrong whenever the
// plan came back with nothing already set, and it spelled the path with `${CLAUDE_PLUGIN_ROOT}` — a
// variable install.sh's own comment records as substituted into a `!`-block's command STRING but NOT
// exported to a child process, so on the one gated command it can expand to `/scripts/install.sh`.
func TestInstallSkillRunsThePrintedCommandRatherThanAnExample(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("skills", "install", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	step, _, found := strings.Cut(body, "## 3. Read the result")
	if !found {
		t.Fatal("SKILL.md no longer has a `## 3. Read the result` section; this test reads the step " +
			"above it and needs re-anchoring")
	}
	_, step, found = strings.Cut(step, "### Then run one command")
	if !found {
		t.Fatal("SKILL.md no longer has a `### Then run one command` section")
	}
	// The defect is a COPYABLE command, so that is what this looks for: a fenced block in this step
	// spelling out an install.sh invocation. Prose may name `--on-conflict chain` and
	// `${CLAUDE_PLUGIN_ROOT}` freely — the section explains why neither belongs in a command here, and
	// an earlier version of this assertion failed on that explanation, which is a test measuring the
	// wrong thing rather than a defect in the file.
	for _, fence := range strings.Split(step, "```")[1:] {
		if !strings.Contains(fence, "install.sh") {
			continue
		}
		t.Errorf("this step shows a copyable install.sh command:\n%s\nA model copies the block, not "+
			"the bullet beside it. Every hand-written spelling of this command has been wrong in the "+
			"same two ways: a baked-in --on-conflict that is wrong whenever the plan found nothing "+
			"already set, and ${CLAUDE_PLUGIN_ROOT}, which is not exported to a Bash tool call. "+
			"confirm_command= carries the absolute path resolved from $0", fence)
		break
	}
	if !strings.Contains(step, "verbatim") {
		t.Error("the step no longer tells the model to run the printed line verbatim")
	}
	// And the ask has to be driven by the script's own resolved question, not a paragraph that can
	// drift from what the command does.
	if !strings.Contains(body, "consent_question") {
		t.Error("the skill does not use consent_question=, so the question asked and the command " +
			"authorised are composed independently and can disagree about the money")
	}
}

// TestStrategySetSplitAnswersTheOperationTheCallerAsked. `split` is implemented as a REMOVAL, because
// --config replaces --preset rather than layering, so the absence of a file is the only way to express
// "the preset and nothing else". That is the right mechanism and it leaked into the report: a caller
// that asked to `set` was answered `cleared`, so success had three words and every future caller
// inherited the obligation to know that. install.sh's `set|cleared|unchanged` case was the tell.
func TestStrategySetSplitAnswersTheOperationTheCallerAsked(t *testing.T) {
	state, home := t.TempDir(), t.TempDir()
	const port = "9391"

	if facts, code := settingsIn(t, state, home, "strategy", "set", "--name", "5-min-ping",
		"--port", port, "--preset", "cache"); code != 0 || facts["result"] != "set" {
		t.Fatalf("arming a real strategy: exit %d %v", code, facts)
	}

	facts, code := settingsIn(t, state, home, "strategy", "set", "--name", "none",
		"--port", port, "--preset", "cache")
	if code != 0 || facts["result"] != "set" {
		t.Errorf("`strategy set --name none` -> exit %d result=%q; a caller that asked to set is "+
			"answered with the word for what it asked: %v", code, facts["result"], facts)
	}
	if facts["strategy"] != "none" {
		t.Errorf("strategy=%q, want none: %v", facts["strategy"], facts)
	}
	// The path deliberately does not exist now, so naming it would be a confidently wrong detail.
	if facts["file"] != "(none)" {
		t.Errorf("file=%q; `none` IS the absence of a config, so the report should not name a path "+
			"that does not exist: %v", facts["file"], facts)
	}
	// Idempotent, and still a `set`.
	if facts, code := settingsIn(t, state, home, "strategy", "set", "--name", "none",
		"--port", port, "--preset", "cache"); code != 0 || facts["result"] != "set" {
		t.Errorf("re-setting none: exit %d %v", code, facts)
	}

	// And `clear`, invoked as itself, keeps its own vocabulary — the fix must not have flattened the
	// distinction, only stopped one op borrowing the other's word.
	if facts, code := settingsIn(t, state, home, "strategy", "set", "--name", "5-min-ping",
		"--port", port, "--preset", "cache"); code != 0 {
		t.Fatalf("re-arming: exit %d %v", code, facts)
	}
	if facts, code := settingsIn(t, state, home, "strategy", "clear", "--port", port); code != 0 ||
		facts["result"] != "cleared" {
		t.Errorf("`strategy clear` -> exit %d result=%q, want cleared: %v", code, facts["result"], facts)
	}
	if facts, code := settingsIn(t, state, home, "strategy", "clear", "--port", port); code != 0 ||
		facts["result"] != "unchanged" {
		t.Errorf("`strategy clear` with nothing there -> exit %d result=%q, want unchanged: %v",
			code, facts["result"], facts)
	}
}

// TestOneHourHeadDisclosesItsSizeGateEverywhereItIsDescribed.
//
// `1-hour-head` ships `head_ttl_min_tokens: 50000`, and that number is the difference between the
// strategy doing something and doing nothing. It was disclosed in none of the three places a user
// reads before choosing it.
//
// What makes it more than an omission: the evidence all three places cite is config/config.go's live
// measurement — GRANTED on Haiku 4.5 at 36,251 of 36,574 tokens written, downgraded on Sonnet 5 at
// 48,212. BOTH of those prefixes are BELOW the 50,000 gate the strategy ships with. So a user who arms
// this on the one model where the tier was granted, and checks a request the size of the one that was
// measured, sees no 1h label — and the advice to "verify with Usage.CacheWrite1h" reads zero for a
// second, undisclosed reason. The threshold is defensible (it is what makes the strategy pay at all);
// advertising a measurement while hiding the gate that measurement would not have passed is not.
func TestOneHourHeadDisclosesItsSizeGateEverywhereItIsDescribed(t *testing.T) {
	// The gate as actually shipped. If this changes, every description below has to change with it,
	// which is the coupling this test exists to enforce.
	gate, err := os.ReadFile(filepath.Join("scripts", "settings.py"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gate), `head_ttl_min_tokens": 50000`) {
		t.Skip("the 1-hour-head size gate is no longer 50000; re-anchor this test on the new value")
	}

	for _, f := range []struct{ what, path string }{
		{"the strategy's own description, printed by `strategy list`",
			filepath.Join("scripts", "settings.py")},
		{"plugin.json, where the option is actually chosen",
			filepath.Join(".claude-plugin", "plugin.json")},
		{"the picker skill, which is what a user reads to decide",
			filepath.Join("skills", "cache-strategy-picker", "SKILL.md")},
	} {
		b, err := os.ReadFile(f.path)
		if err != nil {
			t.Fatal(err)
		}
		body := string(b)
		// Only the text that talks about this strategy matters, but every one of these files mentions it
		// exactly once in prose, so a whole-file check is honest and stays readable.
		if !strings.Contains(body, "1-hour-head") && !strings.Contains(body, "1-hour tier") {
			t.Errorf("%s (%s) does not describe 1-hour-head at all", f.what, f.path)
			continue
		}
		if !strings.Contains(body, "50k") && !strings.Contains(body, "50,000") &&
			!strings.Contains(body, "50000") {
			t.Errorf("%s (%s) describes 1-hour-head without stating the >=50k-token gate. Below that "+
				"the strategy does nothing at all, and both measurements this file cites as evidence "+
				"(36,574 and 48,212 tokens) are on the inactive side of it", f.what, f.path)
		}
	}
}

// withEnv replaces a variable in a prepared environment rather than appending a second copy of it:
// duplicate keys in an exec environment are resolved by the child's libc, not by us, so appending is
// not a reliable way to override one.
func withEnv(env []string, key, val string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	return append(out, key+"="+val)
}

// TestRouteReportsAProxyLeftInTheTMPDIRFallback.
//
// start-proxy.sh derives its state dir and then, if it cannot create it, falls back:
//
//	STATE="${CONTEXT_GURU_STATE:-...}"
//	mkdir -p "$STATE" 2>/dev/null || STATE="${TMPDIR:-/tmp}"
//
// install.sh re-derived the first line and not the second, so on a machine whose state dir cannot be
// created it looked for the pidfile in the state dir while the proxy's pidfile was in $TMPDIR — and the
// honest "a proxy is still running" report printed NOTHING. The defect that report exists to fix,
// intact in the fallback path.
//
// Not a corner: the fallback fires only when the machine is already broken, which is when a health
// check is likeliest to fail and an accurate side-effect report matters most.
//
// The fix is that the script which WRITES the pidfile prints the path it used (`--emit-facts`) and this
// one reads it, so there is no second derivation left to drift.
func TestRouteReportsAProxyLeftInTheTMPDIRFallback(t *testing.T) {
	home, proj := t.TempDir(), t.TempDir()
	tmp := t.TempDir()
	port := freePort(t)
	writePluginOptions(t, home, map[string]any{"port": port})

	// A state dir that cannot be created: a path underneath a regular file.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	unmakeable := filepath.Join(blocker, "state")

	// listen=false: it starts and stays up, but never answers /healthz.
	env := routeEnv(t, home, unmakeable, fakeProxyDir(t, port, false))
	env = withEnv(env, "TMPDIR", tmp)
	t.Cleanup(func() { stopFakeProxy2(t, filepath.Join(tmp, "proxy-"+port+".pid")) })

	facts, code := runRoute(t, proj, env, consentOK()...)
	if facts["reason"] != "health_check_failed" {
		t.Fatalf("exit %d, want a failed health check: %v", code, facts)
	}

	// Ground truth: is a proxy really running, and is its pidfile really in TMPDIR? Without this the
	// assertions below could pass vacuously on a run where nothing started at all.
	pidfile := filepath.Join(tmp, "proxy-"+port+".pid")
	b, err := os.ReadFile(pidfile)
	if err != nil {
		t.Skipf("no pidfile in the TMPDIR fallback, so there is no drift to test here: %v", err)
	}
	if err := exec.Command("kill", "-0", strings.TrimSpace(string(b))).Run(); err != nil {
		t.Skipf("the fake proxy is not running: %v", err)
	}

	if facts["proxy_started"] != "true" {
		t.Errorf("a proxy IS running with its pidfile at %s and the report does not mention it. This "+
			"is the same defect the side-effect report fixed, reachable through the fallback: %v",
			pidfile, facts)
	}
	if facts["pidfile"] != pidfile {
		t.Errorf("pidfile=%q, want %q — the path has to come from the script that wrote it, not from "+
			"a second copy of its derivation", facts["pidfile"], pidfile)
	}
}

// stopFakeProxy2 kills a proxy by an explicit pidfile path. Pidfile-first and best-effort; never a
// pkill pattern, because these boxes are shared with another engineer as the same unix user.
func stopFakeProxy2(t *testing.T, pidfile string) {
	t.Helper()
	b, err := os.ReadFile(pidfile)
	if err != nil {
		return
	}
	if pid := strings.TrimSpace(string(b)); pid != "" {
		exec.Command("kill", pid).Run() //nolint:errcheck
	}
}

// TestRouteDistinguishesAnUnreadableStrategyListFromABadName. An empty strategy list is not an unknown
// name, and conflating them made the script state a false thing about the caller's input: with
// settings.py unavailable, `--cache-strategy 5-min-ping` — the default, and obviously a strategy — came
// back as "is not a strategy. Known: unavailable", burying the one fault the user could act on inside a
// sentence about a typo. The skill's next move is to ask which name they meant, about a correct name.
func TestRouteDistinguishesAnUnreadableStrategyListFromABadName(t *testing.T) {
	requireTool(t, "bash")
	home, proj := t.TempDir(), t.TempDir()
	writePluginOptions(t, home, map[string]any{"port": freePort(t)})

	// A scripts directory where settings.py cannot run. install.sh self-locates from $0, so copying it
	// beside a broken sibling is enough — no need to break the real one.
	broken := t.TempDir()
	for _, name := range []string{"install.sh", "start-proxy.sh"} {
		b, err := os.ReadFile(filepath.Join(scriptsDir(t), name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(broken, name), b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(broken, "settings.py"),
		[]byte("#!/bin/sh\nexit 127\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", filepath.Join(broken, "install.sh"), "--route", "--plan",
		"--cache-strategy", "5-min-ping")
	cmd.Dir = proj
	cmd.Env = routeEnv(t, home, t.TempDir(), "")
	out, _ := cmd.CombinedOutput()
	t.Logf("install.sh with an unusable settings.py:\n%s", out)
	body := string(out)

	if strings.Contains(body, "is not a strategy") {
		t.Errorf("`5-min-ping` is the default strategy and was reported as not being one, because the "+
			"LIST could not be read. A false statement about the caller's input:\n%s", body)
	}
	if !strings.Contains(body, "strategy_list_unavailable") {
		t.Errorf("the real fault is not named as the reason, so the user cannot act on it:\n%s", body)
	}
}

// TestConsentQuestionReadsWhetherAStrategySpendsFromTheStrategyList.
//
// `route_consent_question` hardcoded which name spends. That was correct for all three strategies that
// exist, and its catch-all asserted "(no spend)" about every name it had not been told about — so a
// fourth strategy that spends, or `1-hour-head` gaining a ping, would make it state something false in
// the one sentence a human is asked to agree to.
//
// The expectation here is derived from `settings.py strategy list` rather than written out, so this test
// cannot go stale the way the function did: add a spending strategy and it checks the new one too.
func TestConsentQuestionReadsWhetherAStrategySpendsFromTheStrategyList(t *testing.T) {
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	writePluginOptions(t, home, map[string]any{"port": freePort(t)})
	env := routeEnv(t, home, state, "")

	list, code := settingsIn(t, state, home, "strategy", "list")
	if code != 0 || list["names"] == "" {
		t.Fatalf("could not read the strategy list this test derives its expectations from: %v", list)
	}

	for _, name := range strings.Split(list["names"], ",") {
		t.Run(name, func(t *testing.T) {
			spends := list["spends_"+strings.ReplaceAll(name, "-", "_")]
			if spends == "" {
				t.Fatalf("strategy list reports no spends_ fact for %q, so the consent question has "+
					"nothing to read: %v", name, list)
			}
			facts, code := runRoute(t, proj, env, "--plan", "--scope", "project",
				"--cache-strategy", name)
			if code != 0 {
				t.Fatalf("plan: exit %d %v", code, facts)
			}
			q := facts["consent_question"]
			warns := strings.Contains(q, "SPENDS")
			if warns != (spends == "true") {
				t.Errorf("strategy list says spends=%s for %q, and the consent question %s warn about "+
					"spending. The sentence a user agrees to has to agree with STRATEGIES:\n  %s",
					spends, name, map[bool]string{true: "does", false: "does not"}[warns], q)
			}
		})
	}
}

// TestRouteChainOverwritesAConflictFoundInTheFile is the shape no test covered, and the reason the
// defect it guards shipped through three review rounds.
//
// `--force` was added for `replace` only. But BOTH decisions overwrite the routing key — that is what
// routing through a proxy means; `chain` differs only in additionally recording the old value as the
// upstream. So `chain` — the answer the design calls usually right, and the reason the plan asks its one
// question at all — died at step 7 whenever the existing endpoint was in a settings FILE:
// `result=error reason=settings_write_failed` with an empty `detail=`, for a user who had just answered
// the only question they were asked.
//
// Every other chain test supplies the conflict through `ANTHROPIC_BASE_URL`, where there is nothing in
// the file to overwrite, and that path genuinely worked. A file-sourced conflict is what a PREVIOUS
// context-guru install or a checked-in team settings file leaves behind, which is not an exotic state.
func TestRouteChainOverwritesAConflictFoundInTheFile(t *testing.T) {
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	port := freePort(t)
	writePluginOptions(t, home, map[string]any{"port": port})
	env := routeEnv(t, home, state, fakeProxyDir(t, port, true)) // routeEnv strips ANTHROPIC_BASE_URL
	t.Cleanup(func() { stopFakeProxy(t, state, port) })

	const gateway = "https://gw.corp.example/v1"
	settings := filepath.Join(proj, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, settings, map[string]any{"env": map[string]any{"ANTHROPIC_BASE_URL": gateway}})

	facts, code := runRoute(t, proj, env, consentOK("--on-conflict", "chain")...)
	if code != 0 || facts["result"] != "routed" {
		t.Fatalf("chain with a file-sourced conflict: exit %d result=%q detail=%q — the user answered "+
			"the one question the plan asks and the install refused: %v",
			code, facts["result"], facts["detail"], facts)
	}
	if facts["upstream"] != gateway {
		t.Errorf("upstream=%q, want the gateway we chained behind: %v", facts["upstream"], facts)
	}

	// The record is what makes overwriting their key safe rather than a bypass, so it is asserted
	// directly rather than trusted: uninstall reads `previous_base_url` to put the gateway back.
	got := readJSON(t, settings)
	envBlock, _ := got["env"].(map[string]any)
	meta, _ := got["$context-guru"].(map[string]any)
	if envBlock["ANTHROPIC_UPSTREAM"] != gateway {
		t.Errorf("ANTHROPIC_UPSTREAM=%v, want %q — without it the proxy has nothing to forward to and "+
			"the user's auth breaks", envBlock["ANTHROPIC_UPSTREAM"], gateway)
	}
	if meta["previous_base_url"] != gateway {
		t.Fatalf("previous_base_url=%v, want %q. This is the record uninstall restores from; without "+
			"it, --force here WOULD be a one-way door", meta["previous_base_url"], gateway)
	}

	// And the round trip, because the record only matters if it is actually honoured.
	if facts, code := settingsIn(t, state, home, "remove", "--file", settings); code != 0 {
		t.Fatalf("remove: exit %d %v", code, facts)
	}
	after := readJSON(t, settings)
	afterEnv, _ := after["env"].(map[string]any)
	if afterEnv["ANTHROPIC_BASE_URL"] != gateway {
		t.Errorf("after uninstall ANTHROPIC_BASE_URL=%v, want their gateway %q restored",
			afterEnv["ANTHROPIC_BASE_URL"], gateway)
	}
}

// TestAConflictRefusalNamesItsReason. install.sh reports `detail=$(kv "$aout" reason)` on a failed
// write, and this script's conflict paths emitted `existing=`/`proposed=` but no `reason=` — so the one
// line that would explain a refusal came back EMPTY, and a refusal was indistinguishable from a crash.
//
// Fixed in the emit rather than in the reader: every caller keys on `reason=`, so a refusal that does
// not carry one is the defect, and naming it once fixes it for all of them.
func TestAConflictRefusalNamesItsReason(t *testing.T) {
	state, home := t.TempDir(), t.TempDir()

	t.Run("add, base URL already set", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "settings.json")
		writeJSON(t, f, map[string]any{
			"env": map[string]any{"ANTHROPIC_BASE_URL": "https://gw.corp.example/v1"}})
		facts, code := settingsIn(t, state, home, "add", "--file", f,
			"--url", "http://127.0.0.1:8787/anthropic")
		if code != 2 || facts["result"] != "conflict" {
			t.Fatalf("exit %d result=%q, want a conflict: %v", code, facts["result"], facts)
		}
		if facts["reason"] != "base_url_already_set" {
			t.Errorf("reason=%q; install.sh reports this as `detail=`, so an empty one leaves the "+
				"failure unexplained on the line meant to explain it: %v", facts["reason"], facts)
		}
	})

	t.Run("remove, a URL we did not install", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "settings.json")
		writeJSON(t, f, map[string]any{
			"env": map[string]any{"ANTHROPIC_BASE_URL": "https://someone.else/v1"}})
		facts, code := settingsIn(t, state, home, "remove", "--file", f,
			"--url", "http://127.0.0.1:8787/anthropic")
		if code != 2 || facts["result"] != "conflict" {
			t.Fatalf("exit %d result=%q, want a conflict: %v", code, facts["result"], facts)
		}
		if facts["reason"] == "" {
			t.Errorf("no reason= on the remove mismatch either: %v", facts)
		}
	})
}

// TestRouteRefusesAnUnknownScopeOutLoud. `route_scope_file` called `route_refuse` — which emits and
// exits — from inside `R_FILE=$(route_scope_file)`. Command substitution captured every line it
// printed, assigned them to R_FILE and threw them away, so `--scope bogus` produced NO OUTPUT AT ALL
// and exit 2.
//
// That is the same defect class as this design's first recorded one: a non-zero exit with nothing on
// stdout leaves the model with nothing to act on, and it happens inside the `!` block that renders the
// skill. `--mode` and `--on-conflict` were validated at parse time and did print; `--scope` was not,
// and it is the likelier mistake of the three — the flag this skill documents to users is `--global`
// while the value this script accepts is `user`.
func TestRouteRefusesAnUnknownScopeOutLoud(t *testing.T) {
	home, proj := t.TempDir(), t.TempDir()
	facts, code := runRoute(t, proj, routeEnv(t, home, t.TempDir(), ""), "--plan", "--scope", "global")
	if code == 0 {
		t.Fatalf("an unknown scope was accepted: %v", facts)
	}
	if len(facts) == 0 {
		t.Fatal("exited non-zero with NO facts at all. A model gets an empty result and no reason, " +
			"which is the failure this script's plan contract exists to prevent")
	}
	if facts["reason"] != "unknown_scope" {
		t.Errorf("reason=%q, want unknown_scope: %v", facts["reason"], facts)
	}
	// The mistake this catches is `--global` -> `--scope global`, so the refusal should say what the
	// real value is rather than only that this one is wrong.
	if !strings.Contains(facts["note"], "user") {
		t.Errorf("the refusal does not point at the accepted value, so a model has to guess "+
			"again: note=%q", facts["note"])
	}
}

// TestConflictPlanPrintsARunnableCommandPerAnswer.
//
// The conflict branch is the ONE place this design asks a question, and it printed a
// `confirm_command=` that could not perform any of the answers: `R_ONCONFLICT` is empty precisely
// because that is what is being asked, and `route_confirm_command` only appends `--on-conflict` when it
// is set. So SKILL.md's "it carries every decision, the only thing you add is nothing" was false on
// exactly the path where a decision was outstanding, and running the printed line verbatim re-hit the
// same refusal. The way out for a model would have been to compose the flag itself — the
// model-composed-syntax risk this whole design exists to remove.
//
// So the script prints one runnable line per answer and the model picks.
func TestConflictPlanPrintsARunnableCommandPerAnswer(t *testing.T) {
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	port := freePort(t)
	writePluginOptions(t, home, map[string]any{"port": port})
	const gateway = "https://gateway.corp.example/v1"
	env := withEnv(routeEnv(t, home, state, fakeProxyDir(t, port, true)),
		"ANTHROPIC_BASE_URL", gateway)
	t.Cleanup(func() { stopFakeProxy(t, state, port) })

	plan, code := runRoute(t, proj, env, "--plan", "--scope", "project")
	if code != 0 || plan["reason"] != "base_url_already_set" {
		t.Fatalf("expected the conflict question: exit %d %v", code, plan)
	}
	for _, k := range []string{"confirm_command_chain", "confirm_command_replace"} {
		if plan[k] == "" {
			t.Fatalf("%s= is missing, so the only way to act on the answer is to compose the flag: %v",
				k, plan)
		}
	}
	if !strings.Contains(plan["confirm_command_chain"], "--on-conflict chain") {
		t.Errorf("confirm_command_chain does not carry the decision: %s", plan["confirm_command_chain"])
	}

	// The property that matters: the printed line, run exactly as printed, performs the answer.
	cmd := exec.Command("bash", "-c", plan["confirm_command_chain"])
	cmd.Dir = proj
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	t.Logf("confirm_command_chain run verbatim -> %v\n%s", err, out)
	if !strings.Contains(string(out), "result=routed") {
		t.Fatalf("the chain line did not complete the install when run verbatim:\n%s", out)
	}
	if !strings.Contains(string(out), "upstream="+gateway) {
		t.Errorf("chain ran but did not keep their gateway as upstream:\n%s", out)
	}
}

// TestStrategyClearRefusesToDeleteAFileItCannotRead. `clear` swallowed the OSError from reading the
// config, which left `text` empty — and `text and not _strategy_is_ours(text)` is then false, so the
// ownership check was skipped and execution fell through to os.unlink(), reporting `result=cleared` for
// a file it never verified it wrote. A permission-restricted foreign config is exactly what produces
// that, and exactly the case the ownership check exists for, so the invariant this module advertises
// (and TestStrategyNeverTouchesAFileItDidNotWrite asserts) was false in the one case that matters most.
func TestStrategyClearRefusesToDeleteAFileItCannotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode 000 is still readable, so this test cannot create the condition " +
			"it asserts on and would pass without testing anything")
	}
	state, home := t.TempDir(), t.TempDir()
	cfg := strategyCfg(state)
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("# not ours at all\npreset: whatever\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfg, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(cfg, 0o644) }) //nolint:errcheck

	facts, code := settingsIn(t, state, home, "strategy", "clear", "--port", keepalivePort)
	if _, err := os.Stat(cfg); err != nil {
		t.Fatalf("a config that could not be read, and therefore could not be shown to be ours, was "+
			"DELETED: %v (facts %v)", err, facts)
	}
	if code == 0 {
		t.Errorf("reported success for a file it did not touch and could not verify: %v", facts)
	}
	if facts["reason"] != "unreadable" {
		t.Errorf("reason=%q, want unreadable — `set` already names this situation that way, and the "+
			"two ops disagreeing about it is what let this through: %v", facts["reason"], facts)
	}
}

// TestStrategySetSplitNeedsNoPreset. The empty-preset guard ran before the `split` short-circuit, so
// `strategy set --name split` — with no `--preset`, which the parser does not require — was refused as
// `empty_preset`, whose note warns about silently disabling compaction in a file that would never be
// written. `split` IS the absence of a config: it recurses into `clear` and never reads the preset. It
// also regressed the old keep-alive-off path, which was `rm -f "$CFG"` and needed no preset at all.
func TestStrategySetSplitNeedsNoPreset(t *testing.T) {
	state, home := t.TempDir(), t.TempDir()
	facts, code := settingsIn(t, state, home, "strategy", "set", "--name", "none",
		"--port", keepalivePort)
	if code != 0 {
		t.Fatalf("`strategy set --name none` needs no preset and was refused: exit %d %v", code, facts)
	}
	if facts["reason"] == "empty_preset" {
		t.Errorf("still refused for an empty preset on the one strategy that writes no file: %v", facts)
	}
	if facts["result"] != "set" || facts["strategy"] != "none" {
		t.Errorf("result=%q strategy=%q, want set/none: %v", facts["result"], facts["strategy"], facts)
	}

	// The control, because the guard is right where it applies: a strategy that DOES write a file must
	// still refuse an empty preset, or compaction is silently off while pings keep being paid for.
	facts, code = settingsIn(t, state, home, "strategy", "set", "--name", "5-min-ping",
		"--port", keepalivePort, "--preset", "")
	if code == 0 || facts["reason"] != "empty_preset" {
		t.Errorf("the empty-preset guard was lost where it matters: exit %d %v", code, facts)
	}
}

// TestConflictPlanAsksAboutTheEndpointItWouldDisplace.
//
// `consent_question=` named an existing endpoint only through R_UPSTREAM, which the `chain` branch
// populates — and that branch has by definition not run on the path that ASKS about the conflict. So on
// the one path where what happens to the user's gateway *is* the question, the generated question
// described a local proxy and never mentioned the gateway, while SKILL.md says of that line "say all of
// it, do not compose your own shorter version".
//
// This is the round-2 finding (the gate protects the act, not the terms) surviving on the single path
// where the terms are the whole question — and untested for the same reason the --force bug was:
// TestRoutePlanCarriesTheConsentQuestionIncludingTheSpend runs with no ANTHROPIC_BASE_URL, so it never
// reaches this branch.
//
// Questions are now paired one-to-one with the commands that perform them, so the thing a user answers
// and the thing that answer runs are generated together and cannot describe different states.
func TestConflictPlanAsksAboutTheEndpointItWouldDisplace(t *testing.T) {
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	port := freePort(t)
	writePluginOptions(t, home, map[string]any{"port": port})
	const gateway = "https://gw.corp.example/v1"
	env := withEnv(routeEnv(t, home, state, ""), "ANTHROPIC_BASE_URL", gateway)

	facts, code := runRoute(t, proj, env, "--plan", "--scope", "project")
	if code != 0 || facts["reason"] != "base_url_already_set" {
		t.Fatalf("expected the conflict question: exit %d %v", code, facts)
	}

	chain, replace := facts["consent_question_chain"], facts["consent_question_replace"]
	for name, q := range map[string]string{"chain": chain, "replace": replace} {
		if q == "" {
			t.Fatalf("consent_question_%s= is missing, so a model asking per SKILL.md has no generated "+
				"question for the answer it is asking about: %v", name, facts)
		}
		if !strings.Contains(q, gateway) {
			t.Errorf("consent_question_%s does not name the endpoint being displaced, which is the "+
				"whole of what the user is deciding: %q", name, q)
		}
		// The money clause from round 2 has to survive into both, or the paired questions reintroduce
		// the very gap they were split to close.
		if !strings.Contains(q, "SPENDS") {
			t.Errorf("consent_question_%s lost the spend warning: %q", name, q)
		}
	}
	// The two answers are different propositions, and the difference is the decision.
	if !strings.Contains(chain, "upstream") {
		t.Errorf("the chain question does not say the gateway is KEPT: %q", chain)
	}
	if !strings.Contains(replace, "REPLACING") {
		t.Errorf("the replace question does not say the gateway is replaced: %q", replace)
	}

	// One state, one shape: neither bare key appears here, because neither could be complete and both
	// mean something exact everywhere else in this script.
	for _, k := range []string{"consent_question", "confirm_command"} {
		if v, ok := facts[k]; ok {
			t.Errorf("%s=%q is printed on the path where the answer is what is being asked for. A key "+
				"whose contract is 'the whole proposition' / 'a line that runs' should be absent rather "+
				"than hold a placeholder, which is the value most likely to be re-read as the real thing",
				k, v)
		}
	}

	// And the control, because the risk in this fix is losing the single keys where they are correct.
	clean := t.TempDir()
	facts, code = runRoute(t, clean, routeEnv(t, home, t.TempDir(), ""), "--plan", "--scope", "project")
	if code != 0 {
		t.Fatalf("plan with no conflict: exit %d %v", code, facts)
	}
	if facts["consent_question"] == "" || facts["confirm_command"] == "" {
		t.Errorf("with nothing to ask about, the single keys must still be present and complete: %v",
			facts)
	}
}

// TestMissingFlagValueRefusesOnStdout. Every value-taking flag used `${2:?...}`, which writes BASH's
// diagnostic to stderr and exits 1 — no `result=`, nothing on stdout. That is the third recorded
// instance of one shape in this design: `--plan` exiting 2 with no output, `--scope bogus` swallowed by
// a command substitution, and this. Closed as a class rather than one flag at a time, which is why this
// test is a table over every flag rather than an assertion about one.
//
// Reachability is honest rather than dramatic: the skill's `!` block passes fixed arguments and a
// printed `confirm_command_*` always carries its values. What makes it matter is that a re-typed or
// truncated command is usually cut at the END — so a dropped VALUE is at least as likely as a dropped
// flag, and a dropped flag already reports `unknown_flag` on stdout.
func TestMissingFlagValueRefusesOnStdout(t *testing.T) {
	home, proj := t.TempDir(), t.TempDir()
	env := routeEnv(t, home, t.TempDir(), "")

	for _, flag := range []string{
		"--scope", "--mode", "--on-conflict", "--base-url",
		"--cache-strategy", "--upstream", "--health-url",
	} {
		t.Run(flag, func(t *testing.T) {
			facts, code := runRoute(t, proj, env, "--plan", flag)
			if code == 0 {
				t.Fatalf("%s with no value was accepted: %v", flag, facts)
			}
			// The point of the finding: something has to arrive on STDOUT as key=value.
			if len(facts) == 0 {
				t.Fatalf("%s exited %d with nothing on stdout. A caller keying on `result=` sees an "+
					"empty response and no reason, which is this design's oldest defect shape", flag, code)
			}
			if facts["reason"] != "missing_value" {
				t.Errorf("reason=%q, want missing_value: %v", facts["reason"], facts)
			}
			if facts["flag"] != flag {
				t.Errorf("flag=%q, want %q — the refusal has to name which one: %v",
					facts["flag"], flag, facts)
			}
		})
	}

	// Control: the flags still take values. Uses a strategy that EXISTS, deliberately — the point of
	// this control is that a flag carrying a value is accepted, so a value that is itself refused
	// (`split` was removed, not aliased) would make the control fail for the wrong reason and say
	// nothing about the parsing this test is about.
	facts, code := runRoute(t, proj, env, "--plan", "--scope", "project", "--cache-strategy", "none")
	if code != 0 || facts["result"] != "planned" {
		t.Errorf("a flag WITH a value must still be accepted: exit %d %v", code, facts)
	}
}

// TestConsentQuestionAgreesWithTheUpstreamThatGetsWritten covers two defects that were only visible
// together, and only by comparing the question against the file the install writes.
//
//  1. One rule, two encodings. `route_main` DEFAULTED R_UPSTREAM to the displaced endpoint only when it
//     was empty; the question generator OVERRODE it unconditionally. With a configured upstream those
//     disagreed: the chain question named the endpoint being displaced as the one being kept, never
//     named the gateway that actually ends up behind the proxy, and the replace question lost its
//     REPLACING clause entirely because a non-empty R_UPSTREAM made that branch unreachable.
//
//  2. `confirm_command` dropped `--upstream`. Same shape as the `--cache-strategy` drop: a decision
//     travelling in argv, absent from the printed line, silently re-resolved by the run that performs
//     it — so the file named an upstream the caller never asked for. Found by running the check
//     prescribed for (1); the question was right by then and the FILE was wrong, which is the opposite
//     of the defect being looked for.
//
// The assertion is therefore against the settings file rather than against a string: the question has
// to describe what actually happens, and only the file knows that.
func TestConsentQuestionAgreesWithTheUpstreamThatGetsWritten(t *testing.T) {
	const gateway = "https://gw.corp.example/v1"    // what the user already has
	const configured = "https://real.up.example/v1" // an explicitly configured upstream

	t.Run("a configured upstream alongside a conflicting endpoint", func(t *testing.T) {
		home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
		port := freePort(t)
		writePluginOptions(t, home, map[string]any{"port": port})
		env := withEnv(routeEnv(t, home, state, fakeProxyDir(t, port, true)),
			"ANTHROPIC_BASE_URL", gateway)
		t.Cleanup(func() { stopFakeProxy(t, state, port) })

		plan, code := runRoute(t, proj, env, "--plan", "--scope", "project", "--upstream", configured)
		if code != 0 || plan["reason"] != "base_url_already_set" {
			t.Fatalf("expected the conflict question: exit %d %v", code, plan)
		}

		chain, replace := plan["consent_question_chain"], plan["consent_question_replace"]
		if strings.Contains(chain, "keeping "+gateway) {
			t.Errorf("the chain question calls the DISPLACED endpoint the one being kept. With an "+
				"explicit --upstream the proxy forwards there instead, so their endpoint is replaced "+
				"however the conflict decision was spelled:\n  %s", chain)
		}
		if !strings.Contains(chain, configured) {
			t.Errorf("the chain question never names the gateway that ends up behind the proxy:\n  %s",
				chain)
		}
		if !strings.Contains(replace, "REPLACING "+gateway) {
			t.Errorf("the replace question lost its REPLACING clause — a non-empty upstream made that "+
				"branch unreachable, so it stopped saying the endpoint is displaced:\n  %s", replace)
		}
		// Splitting one question into two is an easy way to lose what round 2 added.
		for name, q := range map[string]string{"chain": chain, "replace": replace} {
			if !strings.Contains(q, "SPENDS") {
				t.Errorf("consent_question_%s lost the spend warning: %q", name, q)
			}
		}

		// The check that matters, and the one that found the second defect: run the printed line and
		// compare the question against what was actually written.
		cmd := exec.Command("bash", "-c", plan["confirm_command_chain"])
		cmd.Dir = proj
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		t.Logf("confirm_command_chain -> %v\n%s", err, out)

		got := readJSON(t, filepath.Join(proj, ".claude", "settings.local.json"))
		envBlock, _ := got["env"].(map[string]any)
		written, _ := envBlock["ANTHROPIC_UPSTREAM"].(string)
		if written != configured {
			t.Fatalf("ANTHROPIC_UPSTREAM=%q, want the %q that was asked for. The printed command "+
				"dropped --upstream, so the run that performed the install re-resolved it to the "+
				"endpoint being displaced", written, configured)
		}
		if !strings.Contains(chain, written) {
			t.Errorf("the question and the file disagree: file says %q, question says:\n  %s",
				written, chain)
		}
	})

	// Control: the common case must be unchanged — chain with nothing configured really does keep
	// their gateway, and the question should say so rather than claim a replacement.
	t.Run("chain with no configured upstream still keeps their gateway", func(t *testing.T) {
		home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
		port := freePort(t)
		writePluginOptions(t, home, map[string]any{"port": port})
		env := withEnv(routeEnv(t, home, state, fakeProxyDir(t, port, true)),
			"ANTHROPIC_BASE_URL", gateway)
		t.Cleanup(func() { stopFakeProxy(t, state, port) })

		plan, code := runRoute(t, proj, env, "--plan", "--scope", "project")
		if code != 0 {
			t.Fatalf("plan: exit %d %v", code, plan)
		}
		chain := plan["consent_question_chain"]
		if !strings.Contains(chain, "keeping "+gateway) {
			t.Errorf("the ordinary chain case should say their gateway is kept:\n  %s", chain)
		}
		if strings.Contains(chain, "REPLACING") {
			t.Errorf("chain with nothing configured keeps their endpoint; saying REPLACING would be "+
				"the same defect in the other direction:\n  %s", chain)
		}
		cmd := exec.Command("bash", "-c", plan["confirm_command_chain"])
		cmd.Dir, cmd.Env = proj, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("confirm: %v\n%s", err, out)
		}
		got := readJSON(t, filepath.Join(proj, ".claude", "settings.local.json"))
		envBlock, _ := got["env"].(map[string]any)
		if envBlock["ANTHROPIC_UPSTREAM"] != gateway {
			t.Errorf("ANTHROPIC_UPSTREAM=%v, want %q: the question and the file must agree here too",
				envBlock["ANTHROPIC_UPSTREAM"], gateway)
		}
	})
}

// TestStartProxyDoesNotStopAnotherProjectsProxy is the regression the per-project port exists to
// remove, asserted at the place it actually happened.
//
// Before per-project ports, every project's proxy was on 8787. start-proxy.sh compares the running
// proxy's fingerprint against the configuration THIS session asks for, and on a mismatch stops it
// and starts a replacement — correct when there is one project, catastrophic when there are two:
// each project's SessionStart flipped the shared proxy back to its own preset, and every flip wiped
// the in-memory store. That is the cache-write regression store/store_test.go's idle-exit floor
// already refuses to permit for the same reason.
//
// So a `proxy-<port>.owner` file naming a DIFFERENT project vetoes the restart outright, ahead of
// the fingerprint comparison — against a foreign proxy a mismatch is expected and proves nothing.
//
// The second subtest is the positive control, and it is load-bearing: the veto's assertions are all
// absences, so without a case where the SAME state DOES reach the restart path, gutting section (2)
// to a bare `exit 0` would leave the veto subtest passing.
func TestStartProxyDoesNotStopAnotherProjectsProxy(t *testing.T) {
	requireTool(t, "bash")
	requireTool(t, "python3")

	for _, c := range []struct {
		name       string
		ownerIsUs  bool
		wantNote   string
		unwantNote string
	}{
		{name: "another project's proxy is left alone", ownerIsUs: false,
			wantNote: "is serving another project", unwantNote: "configuration changed"},
		{name: "our own proxy still gets the restart decision", ownerIsUs: true,
			wantNote: "different configuration", unwantNote: "is serving another project"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			mux := http.NewServeMux()
			mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
				w.Write([]byte("ok")) //nolint:errcheck // test stub
			})
			srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
			go srv.Serve(ln) //nolint:errcheck // returns ErrServerClosed on Close
			defer srv.Close()
			port := fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)

			dir := t.TempDir()
			state := filepath.Join(dir, "state")
			proj := filepath.Join(dir, "proj")
			for _, d := range []string{state, proj} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			projReal, err := filepath.EvalSymlinks(proj)
			if err != nil {
				t.Fatal(err)
			}

			// A fingerprint that deliberately does NOT match what this session will compute, so the
			// restart path is what the script would take if nothing vetoed it.
			if err := os.WriteFile(filepath.Join(state, "proxy-"+port+".fingerprint"),
				[]byte("preset=somebody-elses strategy=x idle=0 upstream= port="+port+" bin=/nope\n"),
				0o600); err != nil {
				t.Fatal(err)
			}
			owner := projReal
			if !c.ownerIsUs {
				owner = filepath.Join(dir, "some-other-project")
			}
			if err := os.WriteFile(filepath.Join(state, "proxy-"+port+".owner"),
				[]byte(owner+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			sentinel := filepath.Join(dir, "started")
			fake := filepath.Join(dir, "fake-proxy")
			if err := os.WriteFile(fake,
				[]byte("#!/usr/bin/env bash\ntouch \""+sentinel+"\"\nsleep 30\n"), 0o755); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "start-proxy.sh"))
			cmd.Dir = proj
			cmd.Env = append(sandboxEnv(t),
				"CONTEXT_GURU_STATE="+state,
				"CONTEXT_GURU_BIN="+fake,
				"TMPDIR="+dir,
				"CLAUDE_PLUGIN_OPTION_PORT="+port,
				"ANTHROPIC_BASE_URL=http://127.0.0.1:"+port+"/anthropic",
			)
			b, err := cmd.CombinedOutput()
			code := 0
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			out := string(b)
			t.Logf("start-proxy.sh (owner=%s) -> exit %d, output:\n%s", owner, code, out)

			if code != 0 {
				t.Errorf("exit %d — this hook must never fail a session", code)
			}
			if !strings.Contains(out, c.wantNote) {
				t.Errorf("output does not say %q:\n%s", c.wantNote, out)
			}
			if strings.Contains(out, c.unwantNote) {
				t.Errorf("output must not say %q:\n%s", c.unwantNote, out)
			}
			if _, err := os.Stat(sentinel); err == nil {
				t.Error("a replacement proxy was started even though the port is answering")
			}
			// The foreign proxy's own bookkeeping must survive untouched: rewriting it would hand
			// the port to whichever project ran last, which is the bug in a quieter form.
			if !c.ownerIsUs {
				got, err := os.ReadFile(filepath.Join(state, "proxy-"+port+".owner"))
				if err != nil || strings.TrimSpace(string(got)) != owner {
					t.Errorf("the other project's owner file was disturbed: %q, %v", got, err)
				}
			}
			// And it is still serving — the whole point is that nothing killed it.
			if resp, err := http.Get("http://127.0.0.1:" + port + "/healthz"); err != nil {
				t.Errorf("the proxy stopped answering: %v", err)
			} else {
				resp.Body.Close()
			}
		})
	}
}

// TestStartProxyRestartsTheSharedProxyForANonOwningProject: the veto above must not fire under a
// MACHINE-WIDE install, and this is the regression that says so.
//
// `--scope user` is one settings file, therefore one `port` option, therefore ONE proxy shared by
// every project on the machine — that being what machine-wide means. But every project still has its
// own project_key(), so keying ownership on the project made exactly one project the owner and every
// other one a stranger to the proxy it is supposed to be using. The veto sits AHEAD of the
// fingerprint comparison, so a preset change made from any non-owning project never restarted the
// proxy and never took effect: it kept serving the old preset until the owning project happened to
// start a session, or 24h --idle-exit reaped it. That worked before per-project ports — one file,
// one preset, one proxy, fingerprints agreeing — so it is a regression, not a gap. The advice
// printed with it ("this project should have its own port") is also the precise opposite of the
// install the user asked for, in every session of every project but one.
//
// So ownership is keyed on the ROUTING SCOPE, which is the thing that actually decides whether the
// port is shared. The second subtest is the control that keeps the fix honest: a project that
// installed ITSELF on that port is NOT covered by the machine-wide route (its own settings file is
// more specific and keeps winning), so its proxy is still not ours to stop.
func TestStartProxyRestartsTheSharedProxyForANonOwningProject(t *testing.T) {
	requireTool(t, "bash")
	requireTool(t, "python3")

	for _, c := range []struct {
		name string
		// Does the project named in the legacy owner file route ITSELF?
		ownerRoutesItself bool
		wantNote          string
		unwantNote        string
	}{
		{name: "a legacy owner file does not veto the shared proxy", ownerRoutesItself: false,
			wantNote: "configuration changed", unwantNote: "is serving another project"},
		{name: "a project that installed itself still owns its proxy", ownerRoutesItself: true,
			wantNote: "is serving another project", unwantNote: "configuration changed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			// A REAL, killable proxy holding the port, not an in-process listener. The restart is the
			// behaviour under test: with a listener this script CANNOT stop, every subtest ends in
			// "could not be stopped automatically" and the test degrades to checking the wording of a
			// note — which is exactly the shape of vacuous evidence, since the note would read the
			// same if the veto had fired for the wrong reason.
			py := requireTool(t, "python3")
			dir := t.TempDir()
			port := freePort(t)
			running := exec.Command(py, "-c", healthzServerSrc(port))
			if err := running.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = running.Process.Kill()
				_, _ = running.Process.Wait()
			})
			waitForHealthz(t, port)

			state := filepath.Join(dir, "state")
			home := filepath.Join(dir, "home")
			p1 := filepath.Join(dir, "p1") // owns the running proxy, under the OLD owner scheme
			p2 := filepath.Join(dir, "p2") // this session: same machine-wide route, different project
			for _, d := range []string{state, filepath.Join(home, ".claude"),
				filepath.Join(p1, ".claude"), filepath.Join(p2, ".claude")} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			p1Real, err := filepath.EvalSymlinks(p1)
			if err != nil {
				t.Fatal(err)
			}
			p2Real, err := filepath.EvalSymlinks(p2)
			if err != nil {
				t.Fatal(err)
			}
			userFile := filepath.Join(home, ".claude", "settings.json")
			writeJSON(t, userFile, map[string]any{})

			// Both projects routed by the ONE user-scope file. Recorded rather than inferred, so the
			// test states the scope it is testing instead of depending on is_ours's own rules.
			p1rec := map[string]any{"scope": "user", "file": userFile,
				"recorded_at": "2020-01-01T00:00:00Z"}
			if c.ownerRoutesItself {
				p1rec = map[string]any{"scope": "project-local",
					"file":        filepath.Join(p1, ".claude", "settings.local.json"),
					"recorded_at": "2020-01-01T00:00:00Z"}
			}
			writeJSON(t, filepath.Join(state, "install-scope.json"), map[string]any{
				"version": 1,
				"projects": map[string]any{
					p1Real: p1rec,
					p2Real: map[string]any{"scope": "user", "file": userFile,
						"recorded_at": "2020-01-01T00:00:00Z"},
				},
			})

			// A fingerprint that deliberately does NOT match what this session computes: the restart
			// decision is the thing being tested, so it has to be the decision on the table.
			if err := os.WriteFile(filepath.Join(state, "proxy-"+port+".fingerprint"),
				[]byte("preset=somebody-elses strategy=x idle=0 upstream= port="+port+" bin=/nope\n"),
				0o600); err != nil {
				t.Fatal(err)
			}
			// A BARE PATH, which is what every owner file written before this token existed holds.
			if err := os.WriteFile(filepath.Join(state, "proxy-"+port+".owner"),
				[]byte(p1Real+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			// The pid of the proxy that is really running, which is what stop_running_proxy acts on.
			if err := os.WriteFile(filepath.Join(state, "proxy-"+port+".pid"),
				[]byte(fmt.Sprint(running.Process.Pid)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			// The replacement binds the port too, so the post-launch health poll is answered by the
			// process that was actually started rather than by whatever happened to survive.
			sentinel := filepath.Join(dir, "started")
			fake := filepath.Join(dir, "fake-proxy")
			if err := os.WriteFile(fake, []byte("#!/usr/bin/env bash\n"+
				"if [ \"$1\" = --version ]; then echo 'context-guru-proxy vfake (commit none)'; exit 0; fi\n"+
				"touch \""+sentinel+"\"\n"+
				"exec "+py+" -c "+shellQuote(healthzServerSrc(port))+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "start-proxy.sh"))
			cmd.Dir = p2
			cmd.Env = append(sandboxEnv(t),
				"CONTEXT_GURU_STATE="+state,
				"HOME="+home,
				"CONTEXT_GURU_BIN="+fake,
				"TMPDIR="+dir,
				"CLAUDE_PLUGIN_OPTION_PORT="+port,
				"ANTHROPIC_BASE_URL=http://127.0.0.1:"+port+"/anthropic",
			)
			b, err := cmd.CombinedOutput()
			code := 0
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			out := string(b)
			t.Logf("start-proxy.sh (owner=%s, routes itself=%v) -> exit %d:\n%s",
				p1Real, c.ownerRoutesItself, code, out)

			if code != 0 {
				t.Errorf("exit %d — this hook must never fail a session", code)
			}
			if !strings.Contains(out, c.wantNote) {
				t.Errorf("output does not say %q:\n%s", c.wantNote, out)
			}
			if strings.Contains(out, c.unwantNote) {
				t.Errorf("output must not say %q:\n%s", c.unwantNote, out)
			}
			// Ground truth, not the wording: was a replacement actually started? The shared proxy is
			// the user's only proxy, so a preset change from ANY project it routes has to reach it;
			// and a proxy a project installed for itself must survive another project's session.
			_, started := os.Stat(sentinel)
			if c.ownerRoutesItself {
				if started == nil {
					t.Error("a project's own proxy was stopped and replaced by another project's session")
				}
				if resp, err := http.Get("http://127.0.0.1:" + port + "/healthz"); err != nil {
					t.Errorf("the other project's proxy stopped answering: %v", err)
				} else {
					resp.Body.Close()
				}
			} else if started != nil {
				t.Error("the machine-wide proxy was NOT restarted for a non-owning project, so a " +
					"preset change made from that project never takes effect")
			}
		})
	}
}

// TestStartProxyRecordsTheOwnerOfAProxyItStarts: the veto above is only as good as the file it
// reads, and nothing else writes it. Written next to the fingerprint and under the same ownership
// condition, so a port held by something we did not start is never claimed.
func TestStartProxyRecordsTheOwnerOfAProxyItStarts(t *testing.T) {
	requireTool(t, "bash")
	py := requireTool(t, "python3")

	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	proj := filepath.Join(dir, "proj")
	for _, d := range []string{state, proj} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	projReal, err := filepath.EvalSymlinks(proj)
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
	ln.Close() // take the port only to learn a free number, then hand it to the fake proxy

	// A fake proxy that actually binds and answers /healthz, so the script reaches its success path
	// (and its ownership check) rather than the binary-missing branch.
	fake := filepath.Join(dir, "fake-proxy")
	script := "#!/usr/bin/env bash\nexec " + py + " -c '\n" +
		"import http.server, sys\n" +
		"class H(http.server.BaseHTTPRequestHandler):\n" +
		"    def do_GET(self):\n" +
		"        self.send_response(200); self.end_headers(); self.wfile.write(b\"ok\")\n" +
		"    def log_message(self, *a): pass\n" +
		"http.server.HTTPServer((\"127.0.0.1\", " + port + "), H).serve_forever()\n' \"$@\"\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "start-proxy.sh"))
	cmd.Dir = proj
	cmd.Env = append(sandboxEnv(t),
		"CONTEXT_GURU_STATE="+state,
		"CONTEXT_GURU_BIN="+fake,
		"TMPDIR="+dir,
		"CLAUDE_PLUGIN_OPTION_PORT="+port,
		"ANTHROPIC_BASE_URL=http://127.0.0.1:"+port+"/anthropic",
	)
	b, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		t.Fatalf("exit %d: %s", ee.ExitCode(), b)
	} else if err != nil {
		t.Fatal(err)
	}
	t.Logf("start-proxy.sh output:\n%s", b)
	t.Cleanup(func() {
		if pid, err := os.ReadFile(filepath.Join(state, "proxy-"+port+".pid")); err == nil {
			// Kill by PID, from the pidfile, never a pattern: these tests run on a box shared with
			// other people's live proxies as the same unix user.
			if n, err := strconv.Atoi(strings.TrimSpace(string(pid))); err == nil && n > 1 {
				_ = syscall.Kill(n, syscall.SIGTERM)
			}
		}
	})

	got, err := os.ReadFile(filepath.Join(state, "proxy-"+port+".owner"))
	if err != nil {
		t.Fatalf("no owner file was written for a proxy we started: %v", err)
	}
	if strings.TrimSpace(string(got)) != projReal {
		t.Errorf("owner file says %q, want this project's key %q", strings.TrimSpace(string(got)), projReal)
	}
}

// --- a user-scope install meets the projects that route themselves ----------------------------
//
// The second of the two scenarios this feature exists for: someone installs into one project, likes
// it, and installs at the user level. The machine-wide route they just asked for does NOT reach the
// project that installed itself — a project's own settings file is more specific, so it keeps
// winning — and nothing used to say so. They asked for "everywhere" and got
// everywhere-except-this-one, silently.

// runRouteRaw is runRoute's output without the map. Needed because the gate below emits one
// `existing_project=` line PER project, and a map keyed on the fact name keeps only the last: a
// test reading facts["existing_project"] would pass just as happily if the script listed one
// project out of five.
func runRouteRaw(t *testing.T, dir string, env []string, args ...string) (string, int) {
	t.Helper()
	requireTool(t, "bash")
	argv := append([]string{filepath.Join(scriptsDir(t), "install.sh"), "--route"}, args...)
	cmd := exec.Command("bash", argv...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running install.sh --route %v: %v (%s)", args, err, out)
	}
	t.Logf("install.sh --route %v -> exit %d\n%s", args, code, out)
	return string(out), code
}

// seedProjectRecord writes an install-scope.json record for `proj` as if that project had installed
// itself at project scope, at `port`, and makes its settings file actually route there — both
// halves, because `adopt` only removes routing it can prove is ours.
func seedProjectRecord(t *testing.T, state, proj, port string) string {
	t.Helper()
	sd := filepath.Join(state, "context-guru")
	if err := os.MkdirAll(sd, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(proj, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	url := "http://127.0.0.1:" + port + "/anthropic"
	writeJSON(t, file, map[string]any{
		"env":           map[string]any{"ANTHROPIC_BASE_URL": url},
		"$context-guru": map[string]any{"installed_base_url": url},
		"pluginConfigs": map[string]any{
			"context-guru@context-guru": map[string]any{"options": map[string]any{"port": port}},
		},
	})
	projReal, err := filepath.EvalSymlinks(proj)
	if err != nil {
		t.Fatal(err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(sd, "install-scope.json"), map[string]any{
		"version": 1,
		"projects": map[string]any{
			projReal: map[string]any{
				"scope": "project-local", "file": file, "port": p,
				"recorded_at": "2020-01-01T00:00:00Z",
			},
		},
	})
	return file
}

func TestUserScopeInstallAsksAboutProjectsThatRouteThemselves(t *testing.T) {
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	other := t.TempDir()
	seedProjectRecord(t, state, other, freePort(t))
	env := routeEnv(t, home, state, "")

	out, code := runRouteRaw(t, proj, env, "--plan", "--scope", "user", "--i-understand-machine-wide")
	if code != 0 {
		t.Fatalf("a PLAN must report rather than fail: exit %d\n%s", code, out)
	}
	for _, want := range []string{
		"result=needs_decision",
		"reason=project_installs_exist",
		"existing_project=",
		"--on-existing-projects leave",
		"--on-existing-projects adopt",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the plan omits %q, so the caller cannot ask or answer this:\n%s", want, out)
		}
	}
	otherReal, err := filepath.EvalSymlinks(other)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, otherReal) {
		t.Errorf("the gate does not name the project it is about (%s):\n%s", otherReal, out)
	}
	// Nothing may have been written by a plan — the same property TestRoutePlanWritesNothing
	// asserts for the rest of the script, restated because this gate runs before it does.
	if _, err := os.Stat(filepath.Join(proj, ".claude")); err == nil {
		t.Error("the plan created a settings directory")
	}

	// NEGATIVE CONTROL. With no project records the same command must sail past this gate — without
	// it, a script that refused every user-scope install would pass every assertion above.
	home2, state2 := t.TempDir(), t.TempDir()
	facts, code := runRoute(t, t.TempDir(), routeEnv(t, home2, state2, ""),
		"--plan", "--scope", "user", "--i-understand-machine-wide")
	if code != 0 || facts["result"] != "planned" {
		t.Errorf("with no project installs the plan must proceed, got exit %d: %v", code, facts)
	}
}

func TestOnExistingProjectsRefusesAnUnknownAnswer(t *testing.T) {
	home, state := t.TempDir(), t.TempDir()
	facts, code := runRoute(t, t.TempDir(), routeEnv(t, home, state, ""),
		"--plan", "--scope", "user", "--i-understand-machine-wide",
		"--on-existing-projects", "sideways")
	// Validated where --scope is, not where it is used: a value that reached the gate unchecked
	// would read as "no answer given" and re-ask a question the caller already answered, with the
	// typo invisible.
	if code != 3 || facts["reason"] != "unknown_on_existing_projects" {
		t.Errorf("a bogus answer must be refused by name, got exit %d: %v", code, facts)
	}
	if facts["value"] != "sideways" {
		t.Errorf("the refusal does not quote the value back: %v", facts)
	}
}

// TestRouteRefusesAPortAnotherProjectOwns: allocation skips every port another project RECORDED, so
// this state is only reachable for a port that was explicitly configured (or recorded before the
// owner file existed) — and that is exactly the case that must not be resolved silently.
// start-proxy.sh will not kill a proxy it does not own, so an install that proceeded here would
// route this project at a proxy running somebody else's configuration and report success.
func TestRouteRefusesAPortAnotherProjectOwns(t *testing.T) {
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	port := freePort(t)
	writePluginOptions(t, home, map[string]any{"port": port})
	sd := filepath.Join(state, "context-guru")
	if err := os.MkdirAll(sd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sd, "proxy-"+port+".owner"),
		[]byte("/somewhere/else\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	facts, code := runRoute(t, proj, routeEnv(t, home, state, ""), "--plan", "--scope", "project")
	if code != 0 {
		t.Fatalf("a PLAN reports rather than fails: exit %d %v", code, facts)
	}
	if facts["reason"] != "port_owned_by_another_project" {
		t.Fatalf("the plan does not refuse a port another project owns: %v", facts)
	}
	if facts["owner_project"] != "/somewhere/else" {
		t.Errorf("the refusal does not name the owner, so the user cannot act on it: %v", facts)
	}

	// CONTROL: the same owner file naming THIS project is not a conflict at all.
	projReal, err := filepath.EvalSymlinks(proj)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sd, "proxy-"+port+".owner"),
		[]byte(projReal+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	facts, code = runRoute(t, proj, routeEnv(t, home, state, ""), "--plan", "--scope", "project")
	if code != 0 || facts["result"] != "planned" {
		t.Errorf("our own proxy must not be reported as a conflict, got exit %d: %v", code, facts)
	}
}

// TestUserScopeAdoptUnroutesTheProjectsItAdopts is the `adopt` answer end to end, and the ordering
// is the point: the adopted project loses its own routing only AFTER the machine-wide route it will
// fall back on is written and health-checked. Doing it earlier — and then failing the install —
// would leave that project with no route at all.
func TestUserScopeAdoptUnroutesTheProjectsItAdopts(t *testing.T) {
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	other := t.TempDir()
	otherPort := freePort(t)
	otherFile := seedProjectRecord(t, state, other, otherPort)
	// The state a running proxy of that project's own leaves behind. Nothing is listening on
	// otherPort, so adopt's stop step finds it already down and is expected to clear both files;
	// seeding them is what gives that assertion something to prove.
	otherOwner := filepath.Join(state, "context-guru", "proxy-"+otherPort+".owner")
	otherReal0, err := filepath.EvalSymlinks(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherOwner, []byte(otherReal0+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	port := freePort(t)
	writePluginOptions(t, home, map[string]any{"port": port})
	env := routeEnv(t, home, state, fakeProxyDir(t, port, true))
	t.Cleanup(func() { stopFakeProxy(t, state, port) })

	facts, code := runRoute(t, proj, env, "--scope", "user", "--i-consent-to-traffic-interception",
		"--i-understand-machine-wide", "--on-existing-projects", "adopt")
	if code != 0 || facts["result"] != "routed" {
		t.Fatalf("the install itself failed, so adoption proves nothing: exit %d %v", code, facts)
	}

	data := readJSON(t, otherFile)
	if env, _ := data["env"].(map[string]any); env["ANTHROPIC_BASE_URL"] != nil {
		t.Errorf("the adopted project still routes itself, so it still overrides the machine-wide "+
			"route it was adopted into: %v", data)
	}
	// A leftover per-project port would aim that project's hooks at a port nothing serves —
	// configured-looking and broken, which is worse than the state before adoption.
	pc, _ := data["pluginConfigs"].(map[string]any)
	pe, _ := pc["context-guru@context-guru"].(map[string]any)
	if opts, _ := pe["options"].(map[string]any); opts != nil && opts["port"] != nil {
		t.Errorf("the adopted project kept its own port option: %v", opts)
	}
	// Every file touched goes through the same backup machinery as an uninstall; without it this is
	// an install silently editing a project it was not run in.
	//
	// Asserted through `remove`'s own report and the recovery folder, NOT by globbing for a backup
	// FILE. That is what this test used to do, and #305/#306 made it vacuous: backups moved into the
	// recovery folder and a clean removal now deletes them again (forget_backups), so on the success
	// path the glob finds nothing — and "no backup file" is indistinguishable from "no backup was
	// ever taken", which is the failure it was written to catch. It kept passing only because a
	// fallback glob matched the recovery folder's own contents, i.e. it proved a directory existed.
	//
	// What actually survives a successful adopt is `result=removed` (only reachable through the
	// branch that calls backup() first) and the recovery folder plus its README, which nothing but
	// backup() and the hatch ever create.
	if ap := facts["adopted_project"]; !strings.Contains(ap, "unrouted=removed") {
		t.Errorf("the adopted project did not go through remove's success path, so nothing was backed "+
			"up and nothing was restored: adopted_project=%q", ap)
	} else if !strings.Contains(ap, "recovery_dir="+filepath.Join(other, ".claude",
		"context-guru-settings-json")) {
		// Deliberately a real, existing path and not `remove`'s own backup= — that field stopped
		// being a path once a clean removal began deleting the backup it had just taken, and a
		// reported path that does not exist is worse than no report.
		t.Errorf("adopt does not point at the adopted project's recovery folder: adopted_project=%q", ap)
	}
	if _, err := os.Stat(filepath.Join(other, ".claude", "context-guru-settings-json",
		"README.md")); err != nil {
		t.Errorf("no recovery folder beside the adopted project's settings file, so backup() never "+
			"ran on it: %v", err)
	}
	// And its record is gone, so nothing reports it as still having its own routing.
	scopes := readJSON(t, filepath.Join(state, "context-guru", "install-scope.json"))
	projects, _ := scopes["projects"].(map[string]any)
	otherReal := otherReal0
	if _, still := projects[otherReal]; still {
		t.Errorf("the adopted project still has its own record: %v", projects)
	}
	// AND the proxy it was using, plus the owner file that outlives it. Uninstall does both halves
	// for the same reason: a released record beside a live proxy is the one state nothing else in the
	// plugin expects, and a surviving `proxy-<port>.owner` gets a LATER install that explicitly
	// configures that port refused over a project that has not routed itself since. Adopt was doing
	// neither, so every adopted project left a proxy serving its old configuration until --idle-exit
	// reaped it.
	if facts["adopted_proxy_stopped"] == "" && facts["adopted_proxy_left_running"] == "" {
		t.Errorf("adopt says nothing about the proxy the adopted project was using — the port was "+
			"released while that proxy was left to keep serving the old configuration: %v", facts)
	}
	if _, err := os.Stat(otherOwner); err == nil {
		t.Errorf("the adopted project's owner file (%s) survived, so a later install that configures "+
			"that port is refused over a project that no longer routes itself", otherOwner)
	}
}

// TestUserScopeAdoptReportsAProjectThatStillOverrides: adopt's gate promises the adopted projects
// "fall back to the machine-wide one". For a project that had a base URL BEFORE context-guru, that
// is false — `cmd_remove` puts that URL back, deliberately and rightly (deleting a URL we did not
// set is the overreach that branch exists to avoid), and the restored URL is more specific than the
// machine-wide route, so it keeps overriding it. Which is the same silent override the gate exists
// to surface. The user asked for everywhere, was told which projects would be folded in, and still
// got everywhere-except-those — with `unrouted=removed` reported for them exactly as for a project
// that really did fall back.
//
// A reporting fix, not a behavior change: the restore stays.
func TestUserScopeAdoptReportsAProjectThatStillOverrides(t *testing.T) {
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	other := t.TempDir()
	otherFile := seedProjectRecord(t, state, other, freePort(t))

	// Their own gateway, from before we were installed — exactly what `add --on-conflict chain`
	// records so uninstall can put it back.
	theirs := "https://gateway.example.invalid/v1"
	data := readJSON(t, otherFile)
	meta, _ := data["$context-guru"].(map[string]any)
	meta["previous_base_url"] = theirs
	writeJSON(t, otherFile, data)

	port := freePort(t)
	writePluginOptions(t, home, map[string]any{"port": port})
	env := routeEnv(t, home, state, fakeProxyDir(t, port, true))
	t.Cleanup(func() { stopFakeProxy(t, state, port) })

	facts, code := runRoute(t, proj, env, "--scope", "user", "--i-consent-to-traffic-interception",
		"--i-understand-machine-wide", "--on-existing-projects", "adopt")
	if code != 0 || facts["result"] != "routed" {
		t.Fatalf("the install itself failed, so adoption proves nothing: exit %d %v", code, facts)
	}

	// Ground truth first: the restore really happened, so the report below is about a real state and
	// not about a project that was fully adopted after all.
	after := readJSON(t, otherFile)
	aenv, _ := after["env"].(map[string]any)
	if aenv["ANTHROPIC_BASE_URL"] != theirs {
		t.Fatalf("remove did not restore the pre-existing base URL, so there is nothing to report "+
			"here and the restore behaviour itself has changed: %v", after)
	}
	if got := facts["adopted_project_still_overriding"]; !strings.Contains(got, other) ||
		!strings.Contains(got, theirs) {
		t.Errorf("adopt reports this project as folded into the machine-wide route while its own "+
			"pre-existing URL (%s) keeps overriding it: adopted_project_still_overriding=%q, all: %v",
			theirs, got, facts)
	}
}

// TestUserScopeAdoptHandlesAProjectPathWithASpace: the adopt loop used to take the project key as
// "everything up to the first space", so a project at `~/My Projects/thing` yielded a truncated
// path — `[ -n "$ap" ]` still passed, the `cd` failed into `|| true`, the record release silently did
// not run, and the truncated path was reported as if it had worked. Space-bearing paths are ordinary
// on macOS, and `existing_project=` and `file=` are both paths.
func TestUserScopeAdoptHandlesAProjectPathWithASpace(t *testing.T) {
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	other := filepath.Join(t.TempDir(), "My Projects", "thing")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	seedProjectRecord(t, state, other, freePort(t))
	otherReal, err := filepath.EvalSymlinks(other)
	if err != nil {
		t.Fatal(err)
	}
	// FIRST, so this test can never pass on a resolved path that lost the space it is about.
	if !strings.Contains(otherReal, " ") {
		t.Fatalf("this test needs a path containing a space to mean anything: %q", otherReal)
	}

	port := freePort(t)
	writePluginOptions(t, home, map[string]any{"port": port})
	env := routeEnv(t, home, state, fakeProxyDir(t, port, true))
	t.Cleanup(func() { stopFakeProxy(t, state, port) })

	facts, code := runRoute(t, proj, env, "--scope", "user", "--i-consent-to-traffic-interception",
		"--i-understand-machine-wide", "--on-existing-projects", "adopt")
	if code != 0 || facts["result"] != "routed" {
		t.Fatalf("exit %d, want a routed user-scope install: %v", code, facts)
	}
	// The whole path, not the part before the space.
	if ap := facts["adopted_project"]; !strings.HasPrefix(ap, otherReal+" ") {
		t.Errorf("adopt reported a truncated project path: adopted_project=%q, want it to start "+
			"with %q", ap, otherReal)
	}
	// And the record release — the half that used to be lost silently, because it was the only step
	// that went through the truncated path rather than through file=.
	scopes := readJSON(t, filepath.Join(state, "context-guru", "install-scope.json"))
	projects, _ := scopes["projects"].(map[string]any)
	if _, still := projects[otherReal]; still {
		t.Errorf("the adopted project's record survived, so a path with a space in it is still "+
			"reported as a project that routes itself: %v", projects)
	}
}

// TestRouteTakesBackThePortRecordWhenTheInstallFails: step 0 commits the port BEFORE the binary
// step, the settings write and the health check — it has to, since the URL the settings write needs
// names that port. What was missing is the undo. Every failure path after it reported that nothing
// was written while the install-scope.json record, an `options.port`, and in a fresh project the
// settings file and its recovery folder all survived.
//
// The record is the one that does harm rather than just litter: a record with a `port` and no
// `scope`/`file` was reported as a project that routes itself, so the next `--scope user` install was
// refused with `project_installs_exist` naming a project that had never been installed.
func TestRouteTakesBackThePortRecordWhenTheInstallFails(t *testing.T) {
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	port := freePort(t)
	writePluginOptions(t, home, map[string]any{"port": port})
	// listen=false: the proxy starts and stays up but never answers /healthz, so step 6 fails —
	// the earliest failure path that is reached AFTER step 0's write.
	env := routeEnv(t, home, state, fakeProxyDir(t, port, false))
	t.Cleanup(func() { stopFakeProxy(t, state, port) })

	facts, code := runRoute(t, proj, env, consentOK()...)
	if code == 0 || facts["reason"] != "health_check_failed" {
		t.Fatalf("exit %d reason=%q, want a failed health check: %v", code, facts["reason"], facts)
	}
	if facts["port_unwound"] != "true" {
		t.Errorf("the failure does not say the port bookkeeping was taken back, so a caller cannot "+
			"tell it from an install that never got that far: %v", facts)
	}

	projReal, err := filepath.EvalSymlinks(proj)
	if err != nil {
		t.Fatal(err)
	}
	scopesPath := filepath.Join(state, "context-guru", "install-scope.json")
	if _, err := os.Stat(scopesPath); err == nil {
		scopes := readJSON(t, scopesPath)
		projects, _ := scopes["projects"].(map[string]any)
		if rec, still := projects[projReal]; still {
			t.Errorf("a failed install left a port record behind (%v), which `scopes` then reports "+
				"and the next --scope user install refuses over: %v", rec, projects)
		}
	}
	// And the option it wrote into the settings file, which would otherwise aim this project's hooks
	// at a port nothing serves while nothing routes there.
	file := filepath.Join(proj, ".claude", "settings.local.json")
	if _, err := os.Stat(file); err == nil {
		data := readJSON(t, file)
		pc, _ := data["pluginConfigs"].(map[string]any)
		pe, _ := pc["context-guru@context-guru"].(map[string]any)
		if opts, _ := pe["options"].(map[string]any); opts != nil && opts["port"] != nil {
			t.Errorf("a failed install left its port option behind: %v", opts)
		}
	}
}

// TestUserScopeLeaveKeepsTheProjectsItWasToldToLeave: the safe answer, and the one a user picks when
// the per-project config was deliberate. `leave` must be a no-op ON THOSE PROJECTS while the
// user-scope install proceeds for everything else.
func TestUserScopeLeaveKeepsTheProjectsItWasToldToLeave(t *testing.T) {
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	other := t.TempDir()
	otherPort := freePort(t)
	otherFile := seedProjectRecord(t, state, other, otherPort)
	beforeRaw, err := os.ReadFile(otherFile)
	if err != nil {
		t.Fatal(err)
	}

	port := freePort(t)
	writePluginOptions(t, home, map[string]any{"port": port})
	env := routeEnv(t, home, state, fakeProxyDir(t, port, true))
	t.Cleanup(func() { stopFakeProxy(t, state, port) })

	facts, code := runRoute(t, proj, env, "--scope", "user", "--i-consent-to-traffic-interception",
		"--i-understand-machine-wide", "--on-existing-projects", "leave")
	if code != 0 || facts["result"] != "routed" {
		t.Fatalf("exit %d, want a routed user-scope install: %v", code, facts)
	}
	afterRaw, err := os.ReadFile(otherFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(beforeRaw) != string(afterRaw) {
		t.Errorf("`leave` changed the project it was told to leave:\nbefore %s\nafter  %s",
			beforeRaw, afterRaw)
	}
	scopes := readJSON(t, filepath.Join(state, "context-guru", "install-scope.json"))
	projects, _ := scopes["projects"].(map[string]any)
	otherReal, serr := filepath.EvalSymlinks(other)
	if serr != nil {
		t.Fatal(serr)
	}
	if _, still := projects[otherReal]; !still {
		t.Errorf("`leave` dropped the project's record: %v", projects)
	}
}

// pathWithoutTheProxyBinary returns $PATH with every directory that actually holds a
// `context-guru-proxy` removed.
//
// A test about the DOWNLOAD path has to be able to reach it, and it can only be reached when no
// proxy is already on $PATH. The helpers here prepend a fixture directory to the inherited $PATH
// rather than replacing it, so on any machine with a real proxy installed (every eval box) the
// installer finds that one, reports `result=present`, and a test written to exercise a failing
// download instead exercises nothing and says so in the shape of a pass. Filtered by what is on
// disk rather than by naming /usr/local/bin, because the point is the binary, not the directory.
func pathWithoutTheProxyBinary(t *testing.T) string {
	t.Helper()
	keep := []string{}
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if d == "" {
			continue
		}
		if st, err := os.Stat(filepath.Join(d, "context-guru-proxy")); err == nil && !st.IsDir() {
			continue
		}
		keep = append(keep, d)
	}
	return strings.Join(keep, string(os.PathListSeparator))
}

// checksumlessCurlDir returns a directory holding a stub `curl` that serves a release tarball and
// 404s its checksums.txt — the shape of a release whose checksums are missing, which install.sh must
// refuse. Same stub shape as TestInstallRefusesAnUnverifiedDownload, in a helper because a second
// test now needs the refusal as a means rather than as the thing under test.
func checksumlessCurlDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(dir, "context-guru-proxy")
	if err := os.WriteFile(payload, []byte("#!/bin/sh\necho NEVER VERIFIED\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tarball := filepath.Join(dir, "payload.tar.gz")
	if out, err := exec.Command("tar", "czf", tarball, "-C", dir, "context-guru-proxy").
		CombinedOutput(); err != nil {
		t.Fatalf("tar: %v (%s)", err, out)
	}
	stub := "#!/usr/bin/env bash\n" +
		"dest=\"\"; url=\"\"\n" +
		"while [ $# -gt 0 ]; do case \"$1\" in -o) dest=$2; shift 2;; -*) shift;; *) url=$1; shift;; " +
		"esac; done\n" +
		"case \"$url\" in\n" +
		"  *checksums.txt) exit 22;;\n" +
		"  *api.github.com*) printf '{\"tag_name\": \"v9.9.9\"}' ${dest:+> \"$dest\"}; exit 0;;\n" +
		"  *.tar.gz) cp " + tarball + " \"$dest\"; exit 0;;\n" +
		"esac\nexit 22\n"
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// fakeProxyDirAnyPort is fakeProxyDir for a test that cannot know the port in advance: it serves
// whatever `--listen host:port` start-proxy.sh puts in its argv. That is the only way to reach the
// case where the port was ALLOCATED rather than pinned, and the allocated case is the one where
// `port alloc` writes an `options.port` of its own — so it is the case a re-install's unwind has to
// leave alone. A test that pins the port cannot get there.
func fakeProxyDirAnyPort(t *testing.T) string {
	t.Helper()
	py := requireTool(t, "python3")
	dir := t.TempDir()
	script := "#!/usr/bin/env bash\n" +
		"if [ \"$1\" = --version ]; then echo 'context-guru-proxy vfake (commit none)'; exit 0; fi\n" +
		"addr=\"\"\n" +
		"while [ $# -gt 0 ]; do case \"$1\" in --listen) addr=$2; shift 2;; *) shift;; esac; done\n" +
		"port=${addr##*:}\n" +
		"exec " + py + " -c '\n" +
		"import http.server, sys\n" +
		"class H(http.server.BaseHTTPRequestHandler):\n" +
		"    def do_GET(self):\n" +
		"        self.send_response(200); self.end_headers(); self.wfile.write(b\"ok\")\n" +
		"    def log_message(self, *a): pass\n" +
		"http.server.HTTPServer((\"127.0.0.1\", int(sys.argv[1])), H).serve_forever()\n' \"$port\"\n"
	if err := os.WriteFile(filepath.Join(dir, "context-guru-proxy"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestAFailedReinstallKeepsTheWorkingInstallsPortRecord is the one the first version of the unwind
// got backwards, and it is the more expensive of the two states.
//
// `route_unwind_port` fired on `result=ok` from `port alloc`, and `result=ok` also covers
// source=recorded and source=configured — the cases where step 0 wrote nothing new. So a failed
// RE-install of a healthy install deleted that project's whole record (scope, file and port) and the
// user's pinned `options.port`, while `env.ANTHROPIC_BASE_URL` in the same file still named the old
// port. Nothing had changed about the routing, so the failure looked recoverable; the next session
// computed the default port instead of the recorded one and was routed at a port nothing serves.
//
// A re-install is the common case (a repair, a version bump, a strategy change), and the failure it
// is most likely to hit is the health check — so this is the path that had to be safe.
func TestAFailedReinstallKeepsTheWorkingInstallsPortRecord(t *testing.T) {
	// Both halves of "undo what step 0 MADE, never what it found", because which half step 0 makes
	// depends on where the port came from — and the two cases report different things.
	//
	// pinned: the user's `options.port` is already in their own settings file, so `port alloc` writes
	// no option there (source=configured) and the one it does write into the project file on a
	// re-install IS its own to take back. allocated: the option in the project file is the working
	// install's, and taking it back is the damage.
	for _, tc := range []struct {
		name      string
		pinned    bool
		wantParts string // "" = nothing was step 0's to undo
	}{
		{"a pinned port", true, "option"},
		{"an allocated port", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
			var env []string
			port := ""
			if tc.pinned {
				port = freePort(t)
				writePluginOptions(t, home, map[string]any{"port": port})
				env = routeEnv(t, home, state, fakeProxyDir(t, port, true))
			} else {
				env = routeEnv(t, home, state, fakeProxyDirAnyPort(t))
			}

			// A real, working project-scope install first. Without this the record under test is one
			// step 0 created, which is the case the unwind SHOULD clean up — the test would prove the
			// opposite thing.
			facts, code := runRoute(t, proj, env, consentOK()...)
			if code != 0 || facts["result"] != "routed" {
				t.Fatalf("the first install failed, so there is no working install to damage: exit %d "+
					"%v", code, facts)
			}
			if port == "" {
				if port = facts["port"]; port == "" {
					t.Fatalf("the install did not say which port it allocated: %v", facts)
				}
			}
			t.Cleanup(func() { stopFakeProxy(t, state, port) })
			projReal, err := filepath.EvalSymlinks(proj)
			if err != nil {
				t.Fatal(err)
			}
			scopesPath := filepath.Join(state, "context-guru", "install-scope.json")
			beforeProjects, _ := readJSON(t, scopesPath)["projects"].(map[string]any)
			beforeRec, _ := beforeProjects[projReal].(map[string]any)
			if beforeRec == nil {
				t.Fatalf("the first install recorded nothing for %s: %v", projReal, beforeProjects)
			}
			file := filepath.Join(proj, ".claude", "settings.local.json")
			beforeOpt := projectPortOption(t, file)

			// Now break it the way a real re-install breaks: the working proxy is gone and the binary
			// on PATH comes up but never answers /healthz, so the re-install fails at step 6 — the same
			// post-step-0 failure route_unwind_port was written for, this time in a project whose record
			// it did not make.
			stopFakeProxy(t, state, port)
			env2 := routeEnv(t, home, state, fakeProxyDir(t, port, false))
			t.Cleanup(func() { stopFakeProxy(t, state, port) })
			facts, code = runRoute(t, proj, env2, consentOK()...)
			if code == 0 || facts["reason"] != "health_check_failed" {
				t.Fatalf("exit %d reason=%q, want a failed re-install: %v", code, facts["reason"], facts)
			}
			// Said out loud, not merely skipped: a caller has to be able to tell "your record is
			// deliberately still there" from "nothing was cleaned up".
			if tc.wantParts == "" {
				if facts["port_unwound"] != "nothing_to_undo" {
					t.Errorf("port_unwound=%q parts=%q; step 0 created neither half here, so the only "+
						"honest report is nothing_to_undo: %v", facts["port_unwound"],
						facts["port_unwound_parts"], facts)
				}
			} else {
				if facts["port_unwound"] != "true" {
					t.Errorf("port_unwound=%q, want true: %v", facts["port_unwound"], facts)
				}
				if facts["port_unwound_parts"] != tc.wantParts {
					t.Errorf("port_unwound_parts=%q, want %q — the record was NOT step 0's to take "+
						"back: %v", facts["port_unwound_parts"], tc.wantParts, facts)
				}
			}

			// The record, unchanged. This is the expensive half: `ANTHROPIC_BASE_URL` still names the
			// old port, and without the record nothing can compute it again.
			afterProjects, _ := readJSON(t, scopesPath)["projects"].(map[string]any)
			afterRec, _ := afterProjects[projReal].(map[string]any)
			if afterRec == nil {
				t.Fatalf("a failed re-install deleted the working install's record: %v", afterProjects)
			}
			for _, k := range []string{"scope", "file", "port"} {
				if fmt.Sprint(afterRec[k]) != fmt.Sprint(beforeRec[k]) {
					t.Errorf("a failed re-install changed the record's %s: %v -> %v", k, beforeRec[k],
						afterRec[k])
				}
			}
			// And the option the working install is using, in whichever file holds it.
			if tc.pinned {
				pinned := readJSON(t, filepath.Join(home, ".claude", "settings.json"))
				ppc, _ := pinned["pluginConfigs"].(map[string]any)
				ppe, _ := ppc["context-guru@context-guru"].(map[string]any)
				popts, _ := ppe["options"].(map[string]any)
				if popts == nil || fmt.Sprint(popts["port"]) != port {
					t.Errorf("a failed re-install took away the port the user pinned (want %s): %v",
						port, popts)
				}
			} else if got := projectPortOption(t, file); got != beforeOpt || got != port {
				t.Errorf("a failed re-install took away the working install's own port option: "+
					"%q -> %q (want %s)", beforeOpt, got, port)
			}
			// The two agreeing is the whole property: a surviving URL beside a deleted port is the
			// broken state the next session cannot compute its way out of.
			data := readJSON(t, file)
			fenv, _ := data["env"].(map[string]any)
			if url := fmt.Sprint(fenv["ANTHROPIC_BASE_URL"]); !strings.Contains(url, ":"+port) {
				t.Errorf("the routing no longer names the recorded port %s, so the two disagree: %q",
					port, url)
			}
		})
	}
}

// projectPortOption returns pluginConfigs[...].options.port from a settings file as a string, or ""
// when the file, the entry or the key is absent.
func projectPortOption(t *testing.T, file string) string {
	t.Helper()
	if _, err := os.Stat(file); err != nil {
		return ""
	}
	pc, _ := readJSON(t, file)["pluginConfigs"].(map[string]any)
	pe, _ := pc["context-guru@context-guru"].(map[string]any)
	opts, _ := pe["options"].(map[string]any)
	if opts == nil || opts["port"] == nil {
		return ""
	}
	return fmt.Sprint(opts["port"])
}

// TestUserScopeAdoptKeepsTheProxyAndRecordOfTheProjectItIsRunFrom.
//
// `adopt` un-routes every project that routes itself, releases its port record and stops the proxy it
// was using. That is right for a STRANGER project. But the likeliest way anyone reaches adopt at all
// is by converting their own project-local install to machine-wide, FROM that project — and then the
// converting project is in the adopt list too: its record still says scope=project-local, which is
// exactly what the gate lists.
//
// So adopt ran on the install's own project and, with nothing excluding it, killed the proxy step 8
// had just started and health-checked, deleted its pid/owner/fingerprint and released the record step
// 0 had just written — while the install still reported `result=routed`. The user is told they are
// now routed machine-wide; the port they are routed at has nothing on it.
//
// What still applies to that project is removing its project-local routing: that file is more
// specific than the machine-wide route and would keep overriding it, which is the point of adopting.
func TestUserScopeAdoptKeepsTheProxyAndRecordOfTheProjectItIsRunFrom(t *testing.T) {
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	port := freePort(t)
	// This project's own project-local install, at `port`, routing itself — the state a user has
	// immediately before they decide they want context-guru everywhere.
	projFile := seedProjectRecord(t, state, proj, port)

	env := routeEnv(t, home, state, fakeProxyDir(t, port, true))
	t.Cleanup(func() { stopFakeProxy(t, state, port) })
	facts, code := runRoute(t, proj, env, "--scope", "user", "--i-consent-to-traffic-interception",
		"--i-understand-machine-wide", "--on-existing-projects", "adopt")
	if code != 0 || facts["result"] != "routed" {
		t.Fatalf("the conversion itself failed, so adoption proves nothing: exit %d %v", code, facts)
	}
	// This is only the case under test if the machine-wide install really did reuse this project's
	// recorded port; if it picked a different one, everything below passes for the wrong reason.
	if facts["port"] != port || facts["port_source"] != "recorded" {
		t.Fatalf("the install is not on the project's recorded port %s (port=%s source=%s), so the "+
			"case this test exists for was never reached: %v", port, facts["port"],
			facts["port_source"], facts)
	}
	// The self-skip in the adopt loop is what stops the damage: it has to skip the record release too,
	// so it returns before the proxy-stop step is reached at all. (`route_stop_adopted_proxy`'s own
	// $R_PORT guard is the same invariant restated at the point of the dangerous action, and on this
	// path it is deliberately unreachable — so `adopted_proxy_kept=` is NOT what this asserts on.)
	if facts["adopted_project_is_this_project"] == "" {
		t.Errorf("adopt says nothing about having skipped the install's own project, so a reader "+
			"cannot tell a deliberate skip from a release that silently failed: %v", facts)
	}

	// Ground truth, not wording: something still answers on that port.
	waitForHealthz(t, port)
	sd := filepath.Join(state, "context-guru")
	for _, f := range []string{"proxy-" + port + ".pid", "proxy-" + port + ".owner"} {
		if _, err := os.Stat(filepath.Join(sd, f)); err != nil {
			t.Errorf("adopt removed %s for the proxy this install is using: %v", f, err)
		}
	}
	projReal, err := filepath.EvalSymlinks(proj)
	if err != nil {
		t.Fatal(err)
	}
	projects, _ := readJSON(t, filepath.Join(sd, "install-scope.json"))["projects"].(map[string]any)
	rec, _ := projects[projReal].(map[string]any)
	if rec == nil {
		t.Fatalf("adopt released the record step 0 wrote for this install: %v", projects)
	}
	if fmt.Sprint(rec["port"]) != port {
		t.Errorf("the surviving record no longer names the port being served (want %s): %v", port, rec)
	}
	if rec["scope"] != "user" {
		t.Errorf("the record still calls this a %v install after converting to machine-wide: %v",
			rec["scope"], rec)
	}
	// And the half of adoption that DOES apply: the project-local file no longer overrides the
	// machine-wide route it was adopted into.
	data := readJSON(t, projFile)
	if fenv, _ := data["env"].(map[string]any); fenv["ANTHROPIC_BASE_URL"] != nil {
		t.Errorf("the install's own project still routes itself, so the machine-wide route it just "+
			"wrote is overridden in the very project it was run from: %v", data)
	}
}

// TestAFailedBinaryInstallTakesBackItsPortBookkeeping covers the most common early failure there is.
//
// A missing release asset or an unverifiable checksum is refused — rightly, and that refusal is not
// negotiable. But it happens AFTER step 0 has allocated the port, and the exit printed "nothing else
// was touched" over a record, an `options.port`, and in a fresh project the settings file and its
// recovery folder that had just been created. The note a user acts on was false exactly where they
// were most likely to read it.
func TestAFailedBinaryInstallTakesBackItsPortBookkeeping(t *testing.T) {
	requireTool(t, "bash")
	requireTool(t, "tar")
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	bin := checksumlessCurlDir(t)
	dest := filepath.Join(t.TempDir(), "dest")

	// PATH explicitly, rather than routeEnv's prepend: this test NEEDS the download path, and it is
	// unreachable on any machine with a proxy already installed.
	env := withEnv(routeEnv(t, home, state, ""), "PATH",
		bin+string(os.PathListSeparator)+pathWithoutTheProxyBinary(t))
	env = append(env, "CONTEXT_GURU_DEST="+dest)

	facts, code := runRoute(t, proj, env, consentOK()...)
	if code != 3 || facts["reason"] != "binary_install_failed" {
		t.Fatalf("exit %d reason=%q, want the binary step to have failed: %v", code, facts["reason"],
			facts)
	}
	if _, err := os.Stat(filepath.Join(dest, "context-guru-proxy")); err == nil {
		t.Fatal("an unverified binary was installed")
	}
	if facts["port_unwound"] != "true" {
		t.Errorf("port_unwound=%q: step 0 created both halves in this fresh project, so both had to "+
			"come back: %v", facts["port_unwound"], facts)
	}
	for _, part := range []string{"option", "record"} {
		if !strings.Contains(facts["port_unwound_parts"], part) {
			t.Errorf("port_unwound_parts=%q does not name %s", facts["port_unwound_parts"], part)
		}
	}
	// The note has to point at that report rather than contradict it.
	if !strings.Contains(facts["note"], "port_unwound") {
		t.Errorf("the note does not point at what was unwound: note=%q", facts["note"])
	}
	if !strings.Contains(facts["note"], "checksum failure must never be worked around") {
		t.Errorf("the refusal stopped saying the checksum failure is not to be worked around: "+
			"note=%q", facts["note"])
	}

	// And it is really gone, not merely reported.
	projReal, err := filepath.EvalSymlinks(proj)
	if err != nil {
		t.Fatal(err)
	}
	scopesPath := filepath.Join(state, "context-guru", "install-scope.json")
	if _, err := os.Stat(scopesPath); err == nil {
		projects, _ := readJSON(t, scopesPath)["projects"].(map[string]any)
		if rec, still := projects[projReal]; still {
			t.Errorf("the failed install left a port record behind (%v), which the next --scope user "+
				"install is then refused over: %v", rec, projects)
		}
	}
	file := filepath.Join(proj, ".claude", "settings.local.json")
	if _, err := os.Stat(file); err == nil {
		data := readJSON(t, file)
		pc, _ := data["pluginConfigs"].(map[string]any)
		pe, _ := pc["context-guru@context-guru"].(map[string]any)
		if opts, _ := pe["options"].(map[string]any); opts != nil && opts["port"] != nil {
			t.Errorf("the failed install left its port option behind: %v", opts)
		}
	}
}
