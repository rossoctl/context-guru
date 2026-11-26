// Tests for scripts/install-beta.sh: install a beta (pre-release) proxy, and restart it on request.
//
// Nothing here touches the developer's machine. Every run gets its own HOME, destination, state
// directory and PATH. `curl` is a stub that serves a fake release list and a fake source archive;
// the proxy is a small Python server. The archive holds a FAKE install.sh and start-proxy.sh (so no
// download and no real proxy), and the REAL settings.py (so the update answer is the real store).
package plugin

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/rossoctl/context-guru/internal/testutil"
)

const betaTag = "v9.9.9-beta.1"

// fakeProxyBody is a stand-in for context-guru-proxy: --version prints a version, --listen serves
// /healthz. Its command line carries "context-guru-proxy --listen 127.0.0.1:<port>", as the real one's does.
const fakeProxyBody = `#!/usr/bin/env python3
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer
if "--version" in sys.argv:
    print("context-guru-proxy %s (commit none)" % __import__("os").environ.get("FAKE_PROXY_VERSION", "` + betaTag + `"))
    sys.exit(0)
addr = sys.argv[sys.argv.index("--listen") + 1]
host, port = addr.rsplit(":", 1)
class H(BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200); self.end_headers(); self.wfile.write(b"ok")
    def log_message(self, *a): pass
HTTPServer((host, int(port)), H).serve_forever()
`

// fakeInstallSh is the beta tag's install.sh. It writes a proxy that reports FAKE_PROXY_VERSION
// (default: the tag) into CONTEXT_GURU_DEST and records which tag it was asked for.
const fakeInstallSh = `#!/usr/bin/env bash
echo "installer-from-tag $CONTEXT_GURU_VERSION" > "$CG_TEST_MARK"
if [ -n "${FAKE_INSTALL_FAIL:-}" ]; then echo "result=error"; echo "reason=checksum_mismatch: x"; exit 1; fi
mkdir -p "$CONTEXT_GURU_DEST"
cp "$CG_TEST_PROXY" "$CONTEXT_GURU_DEST/context-guru-proxy"
echo "result=installed"
echo "checksum=verified"
`

// fakeStartProxy records its arguments and starts the fake proxy detached on --port.
const fakeStartProxy = `#!/usr/bin/env bash
echo "$@" > "$CG_TEST_ARGS"
port=""
while [ $# -gt 0 ]; do case "$1" in --port) port=$2; shift 2;; *) shift;; esac; done
nohup "$CONTEXT_GURU_BIN" --listen "127.0.0.1:$port" >/dev/null 2>&1 </dev/null &
echo $! > "$CONTEXT_GURU_STATE/proxy-$port.pid"
for _ in $(seq 1 50); do curl -fsS --max-time 1 "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && break; sleep 0.1; done
echo "context-guru: started on $port"
`

type betaEnv struct {
	t           *testing.T
	home, dest  string
	state, stub string
	tmp         string
	mark, args  string
	proxy       string
	apiBody     string // body the release-list stub serves
	apiCode     string
	env         []string
}

// newBetaEnv builds the fixture: a source archive for betaTag, a curl stub, and a sandbox.
func newBetaEnv(t *testing.T) *betaEnv {
	t.Helper()
	requireTool(t, "bash")
	py := requireTool(t, "python3")
	root := t.TempDir()
	e := &betaEnv{t: t, home: filepath.Join(root, "home"), dest: filepath.Join(root, "dest"),
		state: filepath.Join(root, "state"), stub: filepath.Join(root, "bin"),
		mark: filepath.Join(root, "mark"), args: filepath.Join(root, "args"),
		proxy: filepath.Join(root, "fakebin", "context-guru-proxy"), apiCode: "200",
		apiBody: `[{"tag_name":"v9.9.8","prerelease":false},` +
			`{"tag_name":"` + betaTag + `","prerelease":true},{"tag_name":"v9.9.0-beta.1","prerelease":true}]`}
	e.tmp = filepath.Join(root, "tmp") // the script's work dir lands here, and goes with the test
	for _, d := range []string{e.home, e.dest, e.state, e.stub, e.tmp} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, body string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(e.proxy, fakeProxyBody, 0o755)

	// The source archive, laid out as GitHub lays it out: one top directory.
	top := "context-guru-" + strings.TrimPrefix(betaTag, "v")
	src := filepath.Join(root, "src")
	sc := filepath.Join(src, top, "context-guru-plugin", "scripts")
	write(filepath.Join(sc, "install.sh"), fakeInstallSh, 0o755)
	write(filepath.Join(sc, "start-proxy.sh"), fakeStartProxy, 0o755)
	realSettings, err := os.ReadFile(filepath.Join(scriptsDir(t), "settings.py"))
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(sc, "settings.py"), string(realSettings), 0o755)
	write(filepath.Join(src, top, "context-guru-plugin", ".claude-plugin", "plugin.json"), `{"name":"context-guru","version":"9.9.9"}`, 0o644)
	// Files the script must NOT extract: one beside the plugin, one in a plugin folder it does not use.
	write(filepath.Join(src, top, "README.md"), "not extracted\n", 0o644)
	write(filepath.Join(src, top, "context-guru-plugin", "skills", "x", "SKILL.md"), "not extracted\n", 0o644)
	archive := filepath.Join(root, "src.tar.gz")
	if out, err := exec.Command("tar", "czf", archive, "-C", src, top).CombinedOutput(); err != nil {
		t.Fatalf("tar: %v (%s)", err, out)
	}

	realCurl, err := exec.LookPath("curl")
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(e.stub, "curl"), "#!/usr/bin/env bash\n"+
		"url=\"\"; dest=\"\"; args=(\"$@\")\n"+
		"while [ $# -gt 0 ]; do case \"$1\" in -o) dest=$2; shift 2;; -w|--max-time) shift 2;; -*) shift;; *) url=$1; shift;; esac; done\n"+
		"case \"$url\" in\n"+
		"  https://api.github.com/*/releases\\?*) printf '%s' \"$CG_TEST_API_BODY\"; printf '\\n%s' \"$CG_TEST_API_CODE\"; exit 0;;\n"+
		"  https://github.com/*/archive/refs/tags/"+betaTag+".tar.gz) cp \""+archive+"\" \"$dest\"; exit 0;;\n"+
		"  https://*) exit 22;;\n"+
		"esac\n"+
		"exec \""+realCurl+"\" \"${args[@]}\"\n", 0o755)

	e.env = append(sandboxEnv(t),
		"HOME="+e.home,
		"PATH="+e.stub+":"+filepath.Dir(py)+":/usr/bin:/bin",
		"CONTEXT_GURU_DEST="+e.dest,
		"CONTEXT_GURU_STATE="+e.state,
		"TMPDIR="+e.tmp,
		"CONTEXT_GURU_BIN=", // the script must not need this from the caller
		"CG_TEST_MARK="+e.mark, "CG_TEST_ARGS="+e.args, "CG_TEST_PROXY="+e.proxy,
	)
	return e
}

// run executes install-beta.sh and returns its key=value facts, the raw output and the exit code.
func (e *betaEnv) run(extraEnv []string, args ...string) (map[string]string, string, int) {
	e.t.Helper()
	cmd := exec.Command("bash", append([]string{filepath.Join(scriptsDir(e.t), "install-beta.sh")}, args...)...)
	cmd.Env = append(append([]string{}, e.env...), "CG_TEST_API_BODY="+e.apiBody, "CG_TEST_API_CODE="+e.apiCode)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		e.t.Fatalf("running install-beta.sh: %v", err)
	}
	facts := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			facts[k] = v
		}
	}
	e.t.Logf("install-beta.sh %v -> exit %d\n%s", args, code, out)
	return facts, string(out), code
}

// startFakeProxy starts the fake proxy on a free port, writes its pidfile, and stops it at cleanup.
func (e *betaEnv) startFakeProxy(port string) int {
	e.t.Helper()
	cmd := exec.Command(e.proxy, "--listen", "127.0.0.1:"+port)
	cmd.Env = append(e.env, "FAKE_PROXY_VERSION=v0.0.1-old")
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	pid := cmd.Process.Pid
	go cmd.Wait() //nolint:errcheck // reaps it, so Kill(pid, 0) sees it gone
	e.stopAtCleanup(pid)
	e.waitHealthy(port)
	if err := os.WriteFile(filepath.Join(e.state, "proxy-"+port+".pid"), []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		e.t.Fatal(err)
	}
	return pid
}

func (e *betaEnv) stopAtCleanup(pid int) {
	e.t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) }) //nolint:errcheck
}

func (e *betaEnv) waitHealthy(port string) {
	e.t.Helper()
	testutil.WaitFor(e.t, "the fake proxy on "+port+" to answer /healthz", func() bool {
		return exec.Command("curl", "-fsS", "--max-time", "1", "http://127.0.0.1:"+port+"/healthz").Run() == nil
	})
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func (e *betaEnv) updateAnswer() string {
	e.t.Helper()
	cmd := exec.Command("python3", filepath.Join(scriptsDir(e.t), "settings.py"), "update-check", "show")
	cmd.Env = e.env
	out, _ := cmd.CombinedOutput()
	for _, l := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(l, "answer="); ok {
			return v
		}
	}
	return "(none): " + string(out)
}

func TestInstallBetaPicksTheNewestPrereleaseAndUsesItsOwnInstaller(t *testing.T) {
	e := newBetaEnv(t)
	facts, _, code := e.run(nil)
	if code != 0 || facts["result"] != "installed" {
		t.Fatalf("want result=installed, exit 0; got exit %d, %v", code, facts)
	}
	if facts["tag"] != betaTag {
		t.Errorf("tag = %q, want the first pre-release in the list, %s (a stable release must be skipped)", facts["tag"], betaTag)
	}
	if got := readFileString(t, e.mark); !strings.Contains(got, "installer-from-tag "+betaTag) {
		t.Errorf("the installer that ran did not come from the tag, or got another version: %q", got)
	}
	if facts["installed_version"] != betaTag {
		t.Errorf("installed_version = %q", facts["installed_version"])
	}
	if facts["install_result"] != "installed" {
		t.Errorf("the tag installer's own output is not passed on with the install_ prefix: %v", facts)
	}
	if _, err := os.Stat(filepath.Join(e.dest, "context-guru-proxy")); err != nil {
		t.Errorf("no binary in the temp destination: %v", err)
	}
	scripts := facts["beta_scripts"]
	for _, f := range []string{"install.sh", "settings.py", "start-proxy.sh"} {
		if _, err := os.Stat(filepath.Join(scripts, f)); err != nil {
			t.Errorf("beta_scripts misses %s: %v", f, err)
		}
	}
	// Walk the whole work area (TMPDIR is private to this test). Both files sit in the archive. An
	// extraction or a copy that is too broad would put one of them anywhere under here.
	filepath.Walk(e.tmp, func(path string, info os.FileInfo, err error) error { //nolint:errcheck
		if err == nil && !info.IsDir() && (info.Name() == "README.md" || info.Name() == "SKILL.md") {
			t.Errorf("the script extracted more than the plugin scripts: %s", path)
		}
		return nil
	})
	if facts["restart"] != "not_requested" {
		t.Errorf("restart = %q: a restart must be opt-in", facts["restart"])
	}
	// Assumes the repo's plugin.json version is below 9.9.9, the version in the fixture archive.
	if facts["plugin_older"] != "true" || !strings.Contains(facts["plugin_update_command"], "/plugin marketplace update") {
		t.Errorf("the installed plugin is older than 9.9.9 and the output must say so: %v", facts)
	}
	if entries, _ := os.ReadDir(e.home); len(entries) != 0 {
		t.Errorf("the run wrote into HOME: %v", entries)
	}
}

func TestInstallBetaTakesAnExplicitTagAndRefusesAnOddOne(t *testing.T) {
	e := newBetaEnv(t)
	e.apiCode = "500" // an explicit tag must not call the API at all
	facts, _, code := e.run(nil, betaTag)
	if code != 0 || facts["tag"] != betaTag {
		t.Fatalf("explicit tag: exit %d, %v", code, facts)
	}
	for _, bad := range []string{"main", "v1.2", "v1.2.3/../x", "v1.2.3;rm"} {
		facts, _, code := e.run(nil, bad)
		if code == 0 || !strings.HasPrefix(facts["reason"], "bad_tag") {
			t.Errorf("tag %q: want reason=bad_tag, exit != 0; got exit %d, %v", bad, code, facts)
		}
	}
}

func TestInstallBetaExplainsARateLimit(t *testing.T) {
	e := newBetaEnv(t)
	e.apiCode = "403"
	e.apiBody = `{"message":"API rate limit exceeded"}`
	facts, _, code := e.run(nil)
	if code == 0 || !strings.HasPrefix(facts["reason"], "github_rate_limited") {
		t.Fatalf("want reason=github_rate_limited; got exit %d, %v", code, facts)
	}
	if !strings.Contains(facts["reason"], "pass the tag") && !strings.Contains(facts["reason"], "Wait for the window") {
		t.Errorf("the message must say how to go on: %q", facts["reason"])
	}
	if _, err := os.Stat(e.mark); err == nil {
		t.Error("an installer ran after a failed tag lookup")
	}
}

func TestInstallBetaSetsTheUpdateAnswerToNeverAgainstIssue1254(t *testing.T) {
	e := newBetaEnv(t)
	set := exec.Command("python3", filepath.Join(scriptsDir(t), "settings.py"), "update-check", "answer", "--answer", "always")
	set.Env = e.env
	if out, err := set.CombinedOutput(); err != nil {
		t.Fatalf("seeding the update answer: %v (%s)", err, out)
	}
	if got := e.updateAnswer(); got != "auto" {
		t.Fatalf("seed: answer = %q, want auto", got)
	}
	facts, out, code := e.run(nil, betaTag)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if facts["update_answer_was"] != "auto" || facts["update_answer_now"] != "never" {
		t.Errorf("the change must be reported: %v", facts)
	}
	for _, want := range []string{"--answer always", "#1254"} {
		if !strings.Contains(out, want) {
			t.Errorf("the output must tell how to turn it back on and name #1254: missing %q", want)
		}
	}
	if got := e.updateAnswer(); got != "never" {
		t.Errorf("stored answer = %q, want never", got)
	}
}

func TestInstallBetaLeavesAnOtherUpdateAnswerAlone(t *testing.T) {
	e := newBetaEnv(t)
	// No update-check file: the answer is "ask". Nothing to guard, and nothing may be written.
	facts, _, code := e.run(nil, betaTag)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if _, set := facts["update_answer_was"]; set {
		t.Errorf("changed an answer that did not need it: %v", facts)
	}
	if _, err := os.Stat(filepath.Join(e.state, "update-check.yaml")); err == nil {
		t.Error("the script created update-check.yaml")
	}
}

func TestInstallBetaFailsClosedWhenTheInstallerFails(t *testing.T) {
	e := newBetaEnv(t)
	set := exec.Command("python3", filepath.Join(scriptsDir(t), "settings.py"), "update-check", "answer", "--answer", "always")
	set.Env = e.env
	if out, err := set.CombinedOutput(); err != nil {
		t.Fatalf("seeding the update answer: %v (%s)", err, out)
	}
	facts, _, code := e.run([]string{"FAKE_INSTALL_FAIL=1"}, betaTag)
	if code == 0 || !strings.HasPrefix(facts["reason"], "install_failed") || !strings.Contains(facts["reason"], "checksum_mismatch") {
		t.Fatalf("want reason=install_failed naming the checksum; got exit %d, %v", code, facts)
	}
	if facts["result"] != "error" {
		t.Errorf("result = %q", facts["result"])
	}
	if _, set := facts["beta_scripts"]; set {
		t.Error("the output names beta_scripts, but the folder is deleted after a failure")
	}
	if facts["update_answer_restored"] != "auto" || e.updateAnswer() != "auto" {
		t.Errorf("a failed install must put the update answer back and say so; stored=%q, %v", e.updateAnswer(), facts)
	}
	if _, err := os.Stat(filepath.Join(e.dest, "context-guru-proxy")); err == nil {
		t.Error("a binary was installed")
	}
}

func TestInstallBetaChecksTheInstalledVersion(t *testing.T) {
	e := newBetaEnv(t)
	facts, _, code := e.run([]string{"FAKE_PROXY_VERSION=v0.6.0"}, betaTag)
	// The fake installer copies the proxy, and the proxy reads FAKE_PROXY_VERSION when it runs.
	if code == 0 || !strings.HasPrefix(facts["reason"], "version_mismatch") {
		t.Fatalf("want reason=version_mismatch; got exit %d, %v", code, facts)
	}
	if _, set := facts["beta_scripts"]; set {
		t.Error("the output names beta_scripts, but the folder is deleted after a failure")
	}
}

func TestInstallBetaDoesNotTouchARunningProxyWithoutRestart(t *testing.T) {
	e := newBetaEnv(t)
	port := freePort(t)
	pid := e.startFakeProxy(port)
	_, _, code := e.run(nil, betaTag)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !alive(pid) {
		t.Fatal("a proxy was stopped although --restart was not given")
	}
	if _, err := os.Stat(e.args); err == nil {
		t.Error("start-proxy.sh ran although --restart was not given")
	}
}

func TestInstallBetaRestartsWithThePresetAndNeverGuessesTheProcess(t *testing.T) {
	e := newBetaEnv(t)
	port := freePort(t)
	oldPid := e.startFakeProxy(port)
	// The running proxy was started with the medium preset; no preset is configured in a settings file.
	fp := "preset=medium strategy=auto idle=24h upstream= port=" + port + " bin=v0.0.1-old\n"
	if err := os.WriteFile(filepath.Join(e.state, "proxy-"+port+".fingerprint"), []byte(fp), 0o644); err != nil {
		t.Fatal(err)
	}
	facts, _, code := e.run(nil, betaTag, "--restart", "--port", port)
	t.Cleanup(func() {
		if b, err := os.ReadFile(filepath.Join(e.state, "proxy-"+port+".pid")); err == nil {
			if p, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				syscall.Kill(p, syscall.SIGKILL) //nolint:errcheck
			}
		}
	})
	if code != 0 || facts["restart"] != "done" || facts["healthz"] != "ok" {
		t.Fatalf("want restart=done healthz=ok; got exit %d, %v", code, facts)
	}
	if alive(oldPid) {
		t.Error("the old proxy is still running")
	}
	b, err := os.ReadFile(filepath.Join(e.state, "proxy-"+port+".pid"))
	if err != nil {
		t.Fatal(err)
	}
	if newPid, _ := strconv.Atoi(strings.TrimSpace(string(b))); newPid == oldPid || !alive(newPid) {
		t.Errorf("no new proxy is running (old %d, pidfile %q)", oldPid, b)
	}
	args := readFileString(t, e.args)
	for _, want := range []string{"--unrouted", "--port " + port, "--preset medium"} {
		if !strings.Contains(args, want) {
			t.Errorf("start-proxy.sh args %q miss %q (a restart without --preset resets the preset, #1244)", args, want)
		}
	}
}

func TestInstallBetaRefusesToStopAPidThatIsNotOurProxy(t *testing.T) {
	e := newBetaEnv(t)
	port := freePort(t)
	// A stranger's process named in the pidfile.
	sleeper := exec.Command("sleep", "60")
	sleeper.Env = e.env
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	go sleeper.Wait() //nolint:errcheck
	e.stopAtCleanup(sleeper.Process.Pid)
	if err := os.WriteFile(filepath.Join(e.state, "proxy-"+port+".pid"), []byte(fmt.Sprint(sleeper.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	facts, _, code := e.run(nil, betaTag, "--restart", "--port", port)
	if code == 0 || !strings.HasPrefix(facts["reason"], "pid_not_ours") {
		t.Fatalf("want reason=pid_not_ours; got exit %d, %v", code, facts)
	}
	if !alive(sleeper.Process.Pid) {
		t.Fatal("the script signalled a process that is not our proxy")
	}
	if _, err := os.Stat(e.args); err == nil {
		t.Error("start-proxy.sh ran after the refusal")
	}
}

func TestInstallBetaRestartNeedsAPort(t *testing.T) {
	e := newBetaEnv(t)
	facts, _, code := e.run(nil, betaTag, "--restart")
	if code == 0 || !strings.HasPrefix(facts["reason"], "port_unknown") {
		t.Fatalf("an unrouted sandbox has no port: want reason=port_unknown; got exit %d, %v", code, facts)
	}
}

func TestInstallBetaRestartWithNothingRunningJustStarts(t *testing.T) {
	e := newBetaEnv(t)
	port := freePort(t)
	facts, _, code := e.run(nil, betaTag, "--restart", "--port", port)
	t.Cleanup(func() {
		if b, err := os.ReadFile(filepath.Join(e.state, "proxy-"+port+".pid")); err == nil {
			if p, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				syscall.Kill(p, syscall.SIGKILL) //nolint:errcheck
			}
		}
	})
	if code != 0 || facts["stopped_pid"] != "none" || facts["restart"] != "done" || facts["healthz"] != "ok" {
		t.Fatalf("want stopped_pid=none restart=done healthz=ok; got exit %d, %v", code, facts)
	}
	if facts["preset"] != "off" {
		t.Errorf("preset = %q, want off when nothing is configured and nothing runs", facts["preset"])
	}
}

func TestInstallBetaDoesNotStopAProxyItHasNoPidfileFor(t *testing.T) {
	e := newBetaEnv(t)
	port := freePort(t)
	pid := e.startFakeProxy(port)
	if err := os.Remove(filepath.Join(e.state, "proxy-"+port+".pid")); err != nil {
		t.Fatal(err)
	}
	facts, _, code := e.run(nil, betaTag, "--restart", "--port", port)
	if code == 0 || !strings.HasPrefix(facts["reason"], "port_in_use_not_ours") {
		t.Fatalf("want reason=port_in_use_not_ours; got exit %d, %v", code, facts)
	}
	if !alive(pid) {
		t.Fatal("a proxy without a pidfile was stopped")
	}
}

func TestInstallBetaRefusesBadArguments(t *testing.T) {
	e := newBetaEnv(t)
	for _, c := range []struct {
		reason string
		args   []string
	}{
		{"bad_port", []string{"--port", "abc"}},
		{"bad_port", []string{"--port"}},
		{"unknown_flag", []string{"--nope"}},
		{"too_many_arguments", []string{betaTag, "v1.0.0"}},
	} {
		facts, _, code := e.run(nil, c.args...)
		if code == 0 || !strings.HasPrefix(facts["reason"], c.reason) {
			t.Errorf("%v: want reason=%s; got exit %d, %v", c.args, c.reason, code, facts)
		}
	}
	if _, err := os.Stat(e.mark); err == nil {
		t.Error("an installer ran after a bad argument")
	}
}

func TestInstallBetaReportsAnUnreadableUpdateAnswer(t *testing.T) {
	e := newBetaEnv(t)
	// A file we did not write: settings.py refuses to read it as ours.
	if err := os.WriteFile(filepath.Join(e.state, "update-check.yaml"), []byte("answer: auto\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	facts, _, code := e.run(nil, betaTag)
	if code != 0 {
		t.Fatalf("exit %d, %v", code, facts)
	}
	if facts["update_answer"] != "unreadable" {
		t.Errorf("update_answer = %q, want unreadable: the guard did not run and the output must say so", facts["update_answer"])
	}
}
