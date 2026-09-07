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
			cmd.Env = append(os.Environ(),
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
	cmd.Env = append(os.Environ(),
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
	cmd.Env = append(os.Environ(), "TMPDIR="+t.TempDir())
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
	port := statsStub(t, `{"total_saved_usd": 0.03, "saved_unique": 12000, "keepalive_pings": 4}`)
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
	port := statsStub(t, `{"total_saved_usd": 0.03, "saved_unique": 12000, "keepalive_pings": 4}`)
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
	if !strings.Contains(out, "ka 4p") {
		t.Errorf("got %q, want %q with --keepalive passed", out, "ka 4p")
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
		cmd.Env = append(os.Environ(), "TMPDIR="+tmp,
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
		cmd.Env = append(os.Environ(), "TMPDIR="+tmp,
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

// keepalivePort is deliberately not 8787. These blocks name the config file after the port, so a
// regression to `${CLAUDE_PLUGIN_OPTION_PORT:-8787}` writes a filename nothing reads — and at a
// fixture port of 8787 that regression passes by coincidence, which is exactly how it shipped.
const keepalivePort = "4041"

// emptyPreset asks runKeepaliveBlock to substitute an EMPTY preset value, which is not the same as
// passing "" — that means "this block has no <preset> line at all". Only the first models a model that
// looked for `option_preset=` and found nothing to use.
const emptyPreset = "\x00"

// unfilledPlaceholder matches a `<lowercase>` placeholder left in an extracted block. Lower-case only
// on purpose, so a heredoc delimiter (`<<EOF`) is not mistaken for one.
var unfilledPlaceholder = regexp.MustCompile(`<[a-z][a-z_]*>`)

// fillSkillPlaceholder substitutes one placeholder in an extracted skill block, failing loudly when
// it is absent: an unsubstituted `PORT="<port>"` makes every path below look inert for the wrong
// reason, which is what happened when the placeholder was introduced in the uninstall skill.
func fillSkillPlaceholder(t *testing.T, skill, block, placeholder, value string) string {
	t.Helper()
	if !strings.Contains(block, placeholder) {
		t.Fatalf("the %s block no longer carries %s; if that value is obtained differently now, this "+
			"test needs to follow suit rather than execute a stale template:\n%s", skill, placeholder, block)
	}
	return strings.Replace(block, placeholder, value, 1)
}

// runKeepaliveBlock executes one bash block from skills/keepalive/SKILL.md with a controlled
// environment, the same way runCheck/skillBlock drive the other skills' destructive snippets.
//
// The blocks are TEMPLATES: the skill tells the model to discover the port and preset with
// `settings.py config` and substitute them, because CLAUDE_PLUGIN_OPTION_* never reaches a Bash tool
// call. So fill the placeholders the way the model is instructed to, and run with those variables
// EMPTY — that is the environment the blocks actually execute in.
func runKeepaliveBlock(t *testing.T, needle, preset string, env map[string]string) (out string, code int) {
	t.Helper()
	requireTool(t, "bash")
	block := skillBlock(t, "keepalive", needle)
	block = fillSkillPlaceholder(t, "keepalive", block, `PORT="<port>"`, `PORT="`+keepalivePort+`"`)
	if preset != "" {
		v := preset
		if v == emptyPreset {
			v = ""
		}
		block = fillSkillPlaceholder(t, "keepalive", block, `PRESET="<preset>"`, `PRESET="`+v+`"`)
	}
	// Nothing below may execute a template. The per-call substitutions above are opt-in, so this is
	// the check that does not have to be remembered: the day a block gains a placeholder no call site
	// fills, it fails here instead of running literally and asserting on nothing.
	if m := unfilledPlaceholder.FindString(block); m != "" {
		t.Fatalf("the keepalive %q block still carries the placeholder %s after substitution; it would "+
			"execute as a literal template and every assertion below would pass on nothing:\n%s",
			needle, m, block)
	}
	cmd := exec.Command("bash", "-c", block)
	cmd.Env = append(os.Environ(), "CLAUDE_PLUGIN_OPTION_PORT=", "CLAUDE_PLUGIN_OPTION_PRESET=")
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	b, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running keepalive block: %v (%s)", err, b)
	}
	t.Logf("keepalive block (needle %q) env=%v -> exit %d, output:\n%s", needle, env, code, b)
	return string(b), code
}

// TestKeepaliveEnableWritesAPresetPreservingConfig is the fix for the defect the config toggle
// would otherwise reintroduce: --config REPLACES --preset entirely (loadConfig in
// cmd/context-guru-proxy/main.go only reads --preset when --config is ABSENT), so a keep-alive
// config that omitted its own `preset:` line would silently turn compaction off the moment
// keep-alive turned on — the opposite of what enabling it is supposed to do.
func TestKeepaliveEnableWritesAPresetPreservingConfig(t *testing.T) {
	state := t.TempDir()
	out, code := runKeepaliveBlock(t, `cat > "$CFG" <<EOF`, "codesmart", map[string]string{
		"XDG_STATE_HOME": state,
	})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	cfg := filepath.Join(state, "context-guru", "keepalive-"+keepalivePort+".yaml")
	b, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("config was not written at %s: %v", cfg, err)
	}
	if !strings.Contains(string(b), "preset: codesmart") {
		t.Errorf("the configured PRESET did not survive into the config file, which would "+
			"silently drop compaction the moment --config is passed:\n%s", b)
	}
	if !strings.Contains(string(b), "keepalive: true") {
		t.Errorf("cache.keepalive: true is missing from the written config:\n%s", b)
	}
}

// TestKeepaliveEnableRefusesToClobberAForeignFile: a file at the same path that this skill did
// not write (no marker) might be something else entirely — refuse rather than overwrite it.
func TestKeepaliveEnableRefusesToClobberAForeignFile(t *testing.T) {
	state := t.TempDir()
	stateDir := filepath.Join(state, "context-guru")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(stateDir, "keepalive-"+keepalivePort+".yaml")
	foreign := "# hand-written, not ours\npreset: cache\n"
	if err := os.WriteFile(cfg, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := runKeepaliveBlock(t, `cat > "$CFG" <<EOF`, "cache", map[string]string{
		"XDG_STATE_HOME": state,
	})
	if code != 0 {
		t.Fatalf("must not fail outright, just refuse: exit %d: %s", code, out)
	}
	if !strings.Contains(out, "REFUSING") {
		t.Errorf("no refusal reported for a foreign file: %s", out)
	}
	got, err := os.ReadFile(cfg)
	if err != nil || string(got) != foreign {
		t.Errorf("the foreign file was modified: %v, %q", err, got)
	}
}

// TestKeepaliveDisableOnlyRemovesOurOwnFile mirrors the same ownership discipline
// settings.py enforces for everything else this plugin writes.
func TestKeepaliveDisableOnlyRemovesOurOwnFile(t *testing.T) {
	t.Run("removes what we wrote", func(t *testing.T) {
		state := t.TempDir()
		stateDir := filepath.Join(state, "context-guru")
		if err := os.MkdirAll(stateDir, 0o755); err != nil {
			t.Fatal(err)
		}
		cfg := filepath.Join(stateDir, "keepalive-"+keepalivePort+".yaml")
		ours := "# context-guru: written by /context-guru:keepalive\npreset: cache\ncache:\n  keepalive: true\n"
		if err := os.WriteFile(cfg, []byte(ours), 0o644); err != nil {
			t.Fatal(err)
		}
		out, code := runKeepaliveBlock(t, `rm -f "$CFG"`, "", map[string]string{
			"XDG_STATE_HOME": state,
		})
		if code != 0 {
			t.Fatalf("exit %d: %s", code, out)
		}
		if _, err := os.Stat(cfg); err == nil {
			t.Error("our own config survived the disable block")
		}
	})

	t.Run("leaves a foreign file alone", func(t *testing.T) {
		state := t.TempDir()
		stateDir := filepath.Join(state, "context-guru")
		if err := os.MkdirAll(stateDir, 0o755); err != nil {
			t.Fatal(err)
		}
		cfg := filepath.Join(stateDir, "keepalive-"+keepalivePort+".yaml")
		foreign := "# hand-written\npreset: cache\n"
		if err := os.WriteFile(cfg, []byte(foreign), 0o644); err != nil {
			t.Fatal(err)
		}
		out, code := runKeepaliveBlock(t, `rm -f "$CFG"`, "", map[string]string{
			"XDG_STATE_HOME": state,
		})
		if code != 0 {
			t.Fatalf("exit %d: %s", code, out)
		}
		got, err := os.ReadFile(cfg)
		if err != nil || string(got) != foreign {
			t.Errorf("a config this skill never wrote was removed: %v, %q, output: %s", err, got, out)
		}
	})
}

// TestKeepaliveEnableRefusesAnEmptyPreset closes the hole the placeholder flow itself opens.
//
// `settings.py config` prints `option_preset=` only when that key is actually configured, so a user who
// set the port and never touched the preset leaves the model with nothing to substitute. Writing the
// resulting `preset:` line empty is not an error anywhere downstream: applyPreset returns early on
// `Preset == ""` (config/config.go), so Load reports success, no pipeline is filled, and compaction is
// entirely OFF while the keep-alive keeps spending the caller's credential on idle pings.
//
// That is strictly worse than the defaulted-`cache` bug this branch fixes, since `cache` at least ran
// the split. The block already refuses to write over a file that is not its own; an unresolved preset
// earns the same conservatism, and must leave no file behind.
func TestKeepaliveEnableRefusesAnEmptyPreset(t *testing.T) {
	state := t.TempDir()
	out, code := runKeepaliveBlock(t, `cat > "$CFG" <<EOF`, emptyPreset, map[string]string{
		"XDG_STATE_HOME": state,
	})
	if code == 0 {
		t.Errorf("an unresolved preset was accepted; that writes `preset:` empty, which turns "+
			"compaction off while keep-alive keeps paying for pings:\n%s", out)
	}
	if !strings.Contains(out, "REFUSING") {
		t.Errorf("the refusal was not reported as such:\n%s", out)
	}
	cfg := filepath.Join(state, "context-guru", "keepalive-"+keepalivePort+".yaml")
	if _, err := os.Stat(cfg); err == nil {
		t.Errorf("a config was written anyway at %s, so the proxy would start with compaction off", cfg)
	}
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
			cmd.Env = append(os.Environ(),
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
		cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+cfg)
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
		cmd.Env = append(os.Environ(),
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

	t.Run("a non-numeric port is reported, not used", func(t *testing.T) {
		requireTool(t, "bash")
		dir := t.TempDir()
		cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "start-proxy.sh"),
			"--unrouted", "--port", "not-a-port")
		cmd.Env = append(os.Environ(), "ANTHROPIC_BASE_URL=", "CONTEXT_GURU_BIN=/nonexistent/x",
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
