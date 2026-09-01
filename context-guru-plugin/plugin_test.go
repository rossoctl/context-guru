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

func requireTool(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not available: %v", name, err)
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
	for _, c := range []struct{ name, baseURL string }{
		{"unset", ""},
		{"another local proxy on a different port (e.g. litellm)", "http://localhost:4000/anthropic"},
		{"a remote gateway", "https://gateway.corp.example/anthropic"},
		{"our port number appearing in a REMOTE host", "https://8787.example.com/anthropic"},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := map[string]string{"CLAUDE_PLUGIN_OPTION_PORT": "8787"}
			if c.baseURL != "" {
				env["ANTHROPIC_BASE_URL"] = c.baseURL
			} else {
				env["ANTHROPIC_BASE_URL"] = ""
			}
			out, code, sentinel := runStart(t, env)
			if code != 0 {
				t.Errorf("exit %d; the hook must never fail a session", code)
			}
			if strings.TrimSpace(out) != "" {
				t.Errorf("the hook printed output in an unrouted project: %q", out)
			}
			if _, err := os.Stat(sentinel); err == nil {
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
