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
	"runtime"
	"strconv"
	"strings"
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
	py := requireTool(t, "python3")
	cmd := exec.Command(py, append([]string{filepath.Join(scriptsDir(t), "settings.py")}, args...)...)
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
	t.Logf("settings.py %v -> exit %d, %v", args, code, facts)
	return facts, code
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
	cmd.Env = append(os.Environ(), "CONTEXT_GURU_BIN="+fake, "TMPDIR="+dir)
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
	cmd.Env = append(os.Environ(),
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
	cmd.Env = append(os.Environ(),
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

	// Back to back, deliberately: the bug needed only that both land in the same second.
	add, code := settings(t, "add", "--file", path, "--url", ourURL, "--force")
	if code != 0 {
		t.Fatalf("add: exit %d, %v", code, add)
	}
	rm, code := settings(t, "remove", "--file", path, "--url", ourURL)
	if code != 0 {
		t.Fatalf("remove: exit %d, %v", code, rm)
	}
	if add["backup"] == rm["backup"] {
		t.Fatalf("both operations reported the same backup path %q, so one overwrote the other",
			add["backup"])
	}
	// The install backup must still hold what was there BEFORE we touched it.
	b, err := os.ReadFile(add["backup"])
	if err != nil {
		t.Fatalf("the install backup is gone: %v", err)
	}
	if !strings.Contains(string(b), theirs) {
		t.Errorf("the install backup does not contain the value it was meant to preserve:\n%s", b)
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
func TestSettingsPreservesFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes only")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	writeJSON(t, path, map[string]any{"env": map[string]any{"ANTHROPIC_AUTH_TOKEN": "secret"}})
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, code := settings(t, "add", "--file", path, "--url", ourURL); code != 0 {
		t.Fatal("add failed")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("mode after add = %o, want 600: this file holds a credential", got)
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
	cmd.Env = append(os.Environ(),
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
	cmd.Env = append(os.Environ(),
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
	cmd.Env = append(os.Environ(),
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
		"--dashboard", // the printed recovery command must not recreate the 404 dashboard
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
			cmd.Env = append(os.Environ(),
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
	cmd.Env = append(os.Environ(),
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
		cmd.Env = append(os.Environ(), "PATH="+path, "CONTEXT_GURU_DEST="+dest, "HOME="+dir)
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
// the backups KEEP_BACKUPS exists to bound then grow without limit in the user's ~/.claude.
func TestBackupPruningSurvivesAGlobbyPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proj[1]", ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "settings.json")
	writeJSON(t, path, map[string]any{"env": map[string]any{"KEEP": "yes"}})

	// Each add/remove pair takes a backup, so this comfortably exceeds KEEP_BACKUPS (10).
	for i := 0; i < 8; i++ {
		if _, code := settings(t, "add", "--file", path, "--url", ourURL); code != 0 {
			t.Fatalf("add %d failed", i)
		}
		if _, code := settings(t, "remove", "--file", path, "--url", ourURL); code != 0 {
			t.Fatalf("remove %d failed", i)
		}
	}
	entries, err := os.ReadDir(dir)
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
			cmd.Env = append(os.Environ(),
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
