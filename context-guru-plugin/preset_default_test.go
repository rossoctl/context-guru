// Tests for #264: the default preset is `off`, and a `/plugin configure` preset change actually
// takes effect. Kept in their own file because they are one coherent change and the review of a
// 7,000-line test file is worse than the review of a 300-line one.
package plugin

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestThePresetDefaultIsEncodedOnce.
//
// The default preset was written out in FIVE places: plugin.json's userConfig, install.sh's
// per-option fallback, start-proxy.sh's and check-proxy.sh's CLAUDE_PLUGIN_OPTION_PRESET reads, and
// the note settings.py prints when a caller passes no preset. That is the defect shape this branch
// exists to remove and the one #263 records: one rule, N encodings, and a change lands in whichever
// subset somebody thought to grep.
//
// settings.py's DEFAULT_PRESET is the owner. This test does not check that the others READ it — two
// of them are hooks on the SessionStart path and must not pay for a python invocation to learn a
// constant — it checks that they cannot silently disagree with it.
func TestThePresetDefaultIsEncodedOnce(t *testing.T) {
	scripts := scriptsDir(t)

	owner := readFileString(t, filepath.Join(scripts, "settings.py"))
	want := betweenQuotes(t, owner, "DEFAULT_PRESET = ")
	if want == "" {
		t.Fatal("settings.py no longer defines DEFAULT_PRESET; this test reads it as the single " +
			"owner of the default and needs re-anchoring")
	}
	if want != "off" {
		t.Errorf("DEFAULT_PRESET is %q. The branch's whole claim is that the default runs NO "+
			"components, which only `off` does; a non-empty pipeline by default is a promise about "+
			"somebody's request body that this repo cannot make on their behalf", want)
	}

	// plugin.json is what the config UI shows, so a disagreement here is the one a user sees.
	var manifest struct {
		UserConfig map[string]struct {
			Default any `json:"default"`
		} `json:"userConfig"`
	}
	raw := readFileString(t, filepath.Join(".claude-plugin", "plugin.json"))
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		t.Fatalf("plugin.json does not parse: %v", err)
	}
	if got := fmt.Sprint(manifest.UserConfig["preset"].Default); got != want {
		t.Errorf("plugin.json's preset default is %q, settings.py says %q. The config UI would "+
			"offer one default while every script assumed the other", got, want)
	}

	// NOT included, deliberately: cmd/context-guru-proxy/main.go defaults --preset to `house`. That
	// is a SIXTH encoding of a default and it is meant to differ, because it is a default for a
	// different product - the standalone proxy binary, whose service default is `house`
	// (tenant.DefaultConfigYAML is that pipeline). The plugin never relies on it: every path that
	// launches a proxy passes --preset explicitly, so the binary's default is unreachable from here.
	// Pinned in prose rather than in an assertion so nobody "fixes" the two into agreement and
	// silently changes what the bare binary does. Raised in review as worth stating.
	for _, c := range []struct{ file, marker string }{
		{"install.sh", `: "${R_PRESET:=`},
		{"start-proxy.sh", `PRESET="${CLAUDE_PLUGIN_OPTION_PRESET:-`},
		{"check-proxy.sh", `PRESET="${CLAUDE_PLUGIN_OPTION_PRESET:-`},
	} {
		body := readFileString(t, filepath.Join(scripts, c.file))
		i := strings.Index(body, c.marker)
		if i < 0 {
			t.Errorf("%s no longer contains %q, so this drift test is not reading its default any "+
				"more and would pass while the file disagreed", c.file, c.marker)
			continue
		}
		rest := body[i+len(c.marker):]
		got := rest[:strings.IndexAny(rest, `"}`)]
		if got != want {
			t.Errorf("%s defaults the preset to %q, settings.py says %q", c.file, got, want)
		}
	}
}

// TestStrategySyncMakesTheArmedFileAgreeWithTheOption is the fix for the third of the three
// mechanisms that made a preset change inert.
//
// --config REPLACES --preset rather than layering, so the preset recorded inside an armed strategy
// file was in force forever. Measured before the fix: set the plugin option to `housellm`, and the
// proxy went on running whatever was recorded when keep-alive was first armed, with nothing
// anywhere reporting the divergence — /healthz answers the literal string "ok", so the running
// configuration could not even be interrogated.
func TestStrategySyncMakesTheArmedFileAgreeWithTheOption(t *testing.T) {
	state, home := t.TempDir(), t.TempDir()
	port := "8787"

	if facts, code := settingsIn(t, state, home,
		"strategy", "set", "--name", "5-min-ping", "--port", port, "--preset", "off"); code != 0 {
		t.Fatalf("arming failed: %v", facts)
	}
	cfg := filepath.Join(state, "keepalive-"+port+".yaml")
	if got := presetLine(t, cfg); got != "off" {
		t.Fatalf("armed file records preset %q, want off", got)
	}

	facts, code := settingsIn(t, state, home,
		"strategy", "sync", "--port", port, "--preset", "housellm")
	if code != 0 {
		t.Fatalf("sync exit %d: %v", code, facts)
	}
	if facts["result"] != "synced" {
		t.Errorf("result=%q, want synced: %v", facts["result"], facts)
	}
	if facts["strategy"] != "5-min-ping" {
		t.Errorf("sync changed the STRATEGY to %q. It may only change the preset — the strategy is "+
			"the user's durable choice and re-rendering must preserve it", facts["strategy"])
	}
	if got := presetLine(t, cfg); got != "housellm" {
		t.Errorf("after sync the file records preset %q, want housellm. The plugin option is the "+
			"source of truth for the preset; the file is only the source for the strategy", got)
	}

	// Idempotent, because this runs before EVERY proxy start. A sync that rewrote the file each time
	// would churn the state directory and make "did the configuration change?" unanswerable.
	facts, code = settingsIn(t, state, home,
		"strategy", "sync", "--port", port, "--preset", "housellm")
	if code != 0 || facts["result"] != "unchanged" {
		t.Errorf("a second sync with the same preset reported %q (exit %d), want unchanged: %v",
			facts["result"], code, facts)
	}
}

// TestASyncedStrategyFileIsIdenticalToAFreshlySetOne is the invariant that keeps sync from becoming
// a second encoding of how these files are written.
//
// sync re-renders from the NAME in the marker plus the preset it is given, rather than editing the
// `preset:` line in place. An in-place edit is the obvious implementation and the wrong one: it
// would be a second piece of code that knows the file format, and the two would drift the first
// time the format gained a line.
func TestASyncedStrategyFileIsIdenticalToAFreshlySetOne(t *testing.T) {
	home := t.TempDir()
	port := "8787"

	// Path A: armed with `off`, then synced to `housellm`.
	synced := t.TempDir()
	settingsIn(t, synced, home, "strategy", "set", "--name", "5-min-ping", "--port", port, "--preset", "off")
	settingsIn(t, synced, home, "strategy", "sync", "--port", port, "--preset", "housellm")

	// Path B: armed with `housellm` directly.
	fresh := t.TempDir()
	settingsIn(t, fresh, home, "strategy", "set", "--name", "5-min-ping", "--port", port, "--preset", "housellm")

	a := readFileString(t, filepath.Join(synced, "keepalive-"+port+".yaml"))
	b := readFileString(t, filepath.Join(fresh, "keepalive-"+port+".yaml"))
	if a != b {
		t.Errorf("a synced file differs from a freshly-set one, so two code paths now know this "+
			"file format and will drift.\nsynced:\n%s\nfresh:\n%s", a, b)
	}
}

// TestStrategySyncLeavesAConfigWeDidNotWriteAlone.
//
// sync runs unprompted on every SessionStart, which makes it the most dangerous writer in the
// plugin: it rewrites a file nobody asked it to touch, at a moment nobody is watching. The
// ownership marker is the whole of its licence to do that.
func TestStrategySyncLeavesAConfigWeDidNotWriteAlone(t *testing.T) {
	state, home := t.TempDir(), t.TempDir()
	port := "8787"
	cfg := filepath.Join(state, "keepalive-"+port+".yaml")
	foreign := "# somebody else's config\npreset: codesmart\ncache:\n  keepalive: false\n"
	if err := os.WriteFile(cfg, []byte(foreign), 0o600); err != nil {
		t.Fatal(err)
	}

	facts, code := settingsIn(t, state, home, "strategy", "sync", "--port", port, "--preset", "housellm")
	if code != 0 {
		t.Errorf("exit %d — a foreign config must not fail the SessionStart path: %v", code, facts)
	}
	if facts["result"] != "skipped" || facts["reason"] != "not_ours" {
		t.Errorf("result=%q reason=%q, want skipped/not_ours: %v", facts["result"], facts["reason"], facts)
	}
	if got := readFileString(t, cfg); got != foreign {
		t.Errorf("rewrote a config it does not own.\nwas:\n%s\nnow:\n%s", foreign, got)
	}
}

// TestStrategySyncOnAnAbsentConfigIsNotAFailure. `none` IS the absence of a file, so there is
// nothing to sync and the preset reaches the proxy directly as --preset. Reporting that as an error
// would put a warning in every session of every user who turned keep-alive off.
func TestStrategySyncOnAnAbsentConfigIsNotAFailure(t *testing.T) {
	facts, code := settings(t, "strategy", "sync", "--port", "8787", "--preset", "off")
	if code != 0 {
		t.Errorf("exit %d, want 0: %v", code, facts)
	}
	if facts["result"] != "unchanged" || facts["strategy"] != "none" {
		t.Errorf("result=%q strategy=%q, want unchanged/none: %v",
			facts["result"], facts["strategy"], facts)
	}
}

// TestTheRemovedStrategyNameIsRefused. `split` was renamed to `none`, not aliased to it: the plugin
// is unreleased, so no install in the wild needs the old word, and an alias would be a second name
// for one thing living in the code forever to serve nobody. The name is therefore simply unknown —
// asserted here so a REAPPEARANCE of it is a failure rather than a silent revival, and so the
// refusal names what is actually available instead of only saying no.
func TestTheRemovedStrategyNameIsRefused(t *testing.T) {
	facts, code := settings(t, "strategy", "set", "--name", "split", "--port", "8787")
	if code == 0 {
		t.Fatalf("`split` was accepted; it was removed, not aliased: %v", facts)
	}
	if facts["reason"] != "unknown_strategy" {
		t.Errorf("reason=%q, want unknown_strategy: %v", facts["reason"], facts)
	}
	// The refusal has to carry the alternatives, or a caller who typed the old name is told only
	// that they are wrong. `known=` is that list, and it comes from STRATEGIES rather than prose.
	if !strings.Contains(facts["known"], "none") {
		t.Errorf("known=%q does not offer `none`, so the refusal does not say what to type instead",
			facts["known"])
	}
}

// --- the fingerprint, and restarting on a change ------------------------------------------------

// servingProxy writes a fake proxy that actually ANSWERS /healthz, which the existing runStart
// stand-in does not. The fingerprint is written only once the proxy answers, so a fake that never
// does cannot reach the code under test.
func servingProxy(t *testing.T, dir, port, sentinel string) string {
	t.Helper()
	requireTool(t, "python3")
	path := filepath.Join(dir, "fake-serving-proxy")
	body := "#!/usr/bin/env bash\ntouch \"" + sentinel + "\"\nexec python3 -c '\n" +
		"import sys, http.server\n" +
		"class H(http.server.BaseHTTPRequestHandler):\n" +
		"    def do_GET(self):\n" +
		"        self.send_response(200); self.end_headers(); self.wfile.write(b\"ok\")\n" +
		"    def log_message(self, *a): pass\n" +
		"http.server.HTTPServer((\"127.0.0.1\", int(sys.argv[1])), H).serve_forever()\n" +
		"' " + port + "\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// startProxyIn runs the hook with an explicit state directory, so the test can inspect and forge the
// pidfile and fingerprint that live in it.
func startProxyIn(t *testing.T, state, bin, port string, extra ...string) (string, int) {
	t.Helper()
	requireTool(t, "bash")
	cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "start-proxy.sh"))
	cmd.Env = append(sandboxEnv(t),
		"CONTEXT_GURU_STATE="+state,
		"CONTEXT_GURU_BIN="+bin,
		"CLAUDE_PLUGIN_OPTION_PORT="+port,
		"ANTHROPIC_BASE_URL=http://127.0.0.1:"+port+"/anthropic",
		"CONTEXT_GURU_HEALTH_BUDGET=10")
	cmd.Env = append(cmd.Env, extra...)
	b, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running start-proxy.sh: %v (%s)", err, b)
	}
	t.Logf("start-proxy.sh -> exit %d:\n%s", code, b)
	return string(b), code
}

// TestStartProxyRecordsTheConfigurationItStartedWith. Nothing can ask a running proxy what it was
// started with, so the starter has to write it down. Without this record the hook cannot tell a
// proxy that matches this session's configuration from one that does not, which is the whole of the
// restart decision.
func TestStartProxyRecordsTheConfigurationItStartedWith(t *testing.T) {
	dir, state := t.TempDir(), t.TempDir()
	port := freePort(t)
	sentinel := filepath.Join(dir, "started")
	bin := servingProxy(t, dir, port, sentinel)

	out, code := startProxyIn(t, state, bin, port, "TMPDIR="+dir)
	t.Cleanup(func() { stopByPidfile(filepath.Join(state, "proxy-"+port+".pid")) })
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("the proxy was never started, so there is nothing to record: %s", out)
	}

	fp := readFileString(t, filepath.Join(state, "proxy-"+port+".fingerprint"))
	for _, field := range []string{"preset=", "strategy=", "idle=", "upstream=", "port=" + port} {
		if !strings.Contains(fp, field) {
			t.Errorf("the fingerprint is missing %q, so a change to it could not be detected: %q",
				field, fp)
		}
	}
	if !strings.Contains(fp, "preset=off") {
		t.Errorf("fingerprint records %q; with no option set the default is off", fp)
	}
}

// TestStartProxyDoesNotRestartWhenTheConfigurationMatches. SessionStart also fires on clear,
// compact, resume and fork, so this runs repeatedly inside one working day. Restarting a healthy
// proxy that already matches would interrupt every other project routed at that port for no reason.
func TestStartProxyDoesNotRestartWhenTheConfigurationMatches(t *testing.T) {
	dir, state := t.TempDir(), t.TempDir()
	port := freePort(t)
	bin := servingProxy(t, dir, port, filepath.Join(dir, "started"))

	if out, code := startProxyIn(t, state, bin, port, "TMPDIR="+dir); code != 0 {
		t.Fatalf("first start failed: exit %d %s", code, out)
	}
	pidfile := filepath.Join(state, "proxy-"+port+".pid")
	t.Cleanup(func() { stopByPidfile(pidfile) })
	first := readFileString(t, pidfile)

	out, code := startProxyIn(t, state, bin, port, "TMPDIR="+dir)
	if code != 0 {
		t.Fatalf("second start failed: exit %d %s", code, out)
	}
	if got := readFileString(t, pidfile); got != first {
		t.Errorf("the proxy was replaced (%s -> %s) although nothing changed", first, got)
	}
	if strings.Contains(out, "restarting") {
		t.Errorf("announced a restart with an identical configuration:\n%s", out)
	}
}

// TestStartProxyReplacesAProxyStartedWithADifferentConfiguration is the behaviour the branch exists
// for, at the level of the hook.
func TestStartProxyReplacesAProxyStartedWithADifferentConfiguration(t *testing.T) {
	dir, state := t.TempDir(), t.TempDir()
	port := freePort(t)
	bin := servingProxy(t, dir, port, filepath.Join(dir, "started"))

	if out, code := startProxyIn(t, state, bin, port, "TMPDIR="+dir); code != 0 {
		t.Fatalf("first start failed: exit %d %s", code, out)
	}
	pidfile := filepath.Join(state, "proxy-"+port+".pid")
	t.Cleanup(func() { stopByPidfile(pidfile) })
	first := strings.TrimSpace(readFileString(t, pidfile))

	// Same as changing the option in /plugin configure: the desired preset no longer matches the
	// one the running proxy was started with.
	out, code := startProxyIn(t, state, bin, port, "TMPDIR="+dir,
		"CLAUDE_PLUGIN_OPTION_PRESET=housellm")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	second := strings.TrimSpace(readFileString(t, pidfile))
	if second == first {
		t.Fatalf("the proxy was NOT replaced after the preset changed (pid still %s). This is the "+
			"defect the branch fixes: the option changes, the config UI shows the new value, and "+
			"the old preset stays in force indefinitely.\n%s", first, out)
	}
	if !strings.Contains(out, "restarting") {
		t.Errorf("replaced the proxy without saying so. A silent restart is indistinguishable "+
			"from a crash to anyone reading the transcript:\n%s", out)
	}
	if fp := readFileString(t, filepath.Join(state, "proxy-"+port+".fingerprint")); !strings.Contains(fp, "preset=housellm") {
		t.Errorf("the new proxy's fingerprint is %q, so the next session would restart it again", fp)
	}
}

// TestStartProxyNeverSignalsAProxyItDidNotStart.
//
// The safety property. A proxy answering on our port with no pidfile of ours is somebody else's —
// a colleague's, a hand-started one, or an unrelated service. Signalling it because THIS session
// wants a different preset would be the plugin killing a process it cannot prove it owns, on a box
// shared with other people. It must decline, say so, and leave the process alive.
func TestStartProxyNeverSignalsAProxyItDidNotStart(t *testing.T) {
	state := t.TempDir()
	dir := t.TempDir()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) }) //nolint:errcheck
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln) //nolint:errcheck
	defer srv.Close()
	port := fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)

	// A fingerprint that does NOT match, and deliberately no pidfile.
	if err := os.WriteFile(filepath.Join(state, "proxy-"+port+".fingerprint"),
		[]byte("preset=somethingelse strategy=none idle=24h upstream= port="+port+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := servingProxy(t, dir, port, filepath.Join(dir, "started"))

	out, code := startProxyIn(t, state, bin, port, "TMPDIR="+dir)
	if code != 0 {
		t.Errorf("exit %d — must never fail the session: %s", code, out)
	}
	// Still serving: nothing was killed.
	if resp, err := http.Get("http://127.0.0.1:" + port + "/healthz"); err != nil {
		t.Errorf("the listener we did not start is gone, so the hook signalled a process it could "+
			"not prove it owned: %v", err)
	} else {
		resp.Body.Close()
	}
	if !strings.Contains(out, "could not be") {
		t.Errorf("killed nothing but also said nothing, so a user whose preset change did not "+
			"take effect has no way to find out why:\n%s", out)
	}
}

// TestStartProxyIgnoresAFingerprintlessProxy. A proxy started before this change has no fingerprint,
// and "no record" must mean "leave it alone" rather than "replace it" — otherwise upgrading the
// plugin restarts every running proxy on the machine at the next SessionStart.
func TestStartProxyIgnoresAFingerprintlessProxy(t *testing.T) {
	dir, state := t.TempDir(), t.TempDir()
	port := freePort(t)
	bin := servingProxy(t, dir, port, filepath.Join(dir, "started"))

	if out, code := startProxyIn(t, state, bin, port, "TMPDIR="+dir); code != 0 {
		t.Fatalf("first start failed: exit %d %s", code, out)
	}
	pidfile := filepath.Join(state, "proxy-"+port+".pid")
	t.Cleanup(func() { stopByPidfile(pidfile) })
	first := strings.TrimSpace(readFileString(t, pidfile))

	// Simulate a proxy from before fingerprints existed.
	if err := os.Remove(filepath.Join(state, "proxy-"+port+".fingerprint")); err != nil {
		t.Fatal(err)
	}

	out, code := startProxyIn(t, state, bin, port, "TMPDIR="+dir,
		"CLAUDE_PLUGIN_OPTION_PRESET=housellm")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if got := strings.TrimSpace(readFileString(t, pidfile)); got != first {
		t.Errorf("replaced a proxy with no fingerprint (%s -> %s). An upgrade would then restart "+
			"every running proxy on the machine at the next SessionStart", first, got)
	}
}

// --- small helpers -----------------------------------------------------------------------------

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

// betweenQuotes returns the double- or single-quoted value that follows marker.
func betweenQuotes(t *testing.T, body, marker string) string {
	t.Helper()
	i := strings.Index(body, marker)
	if i < 0 {
		return ""
	}
	rest := body[i+len(marker):]
	q := strings.IndexAny(rest, `"'`)
	if q < 0 {
		return ""
	}
	rest = rest[q+1:]
	end := strings.IndexAny(rest, `"'`)
	if end < 0 {
		return ""
	}
	return rest[:end]
}

func presetLine(t *testing.T, cfg string) string {
	t.Helper()
	for _, line := range strings.Split(readFileString(t, cfg), "\n") {
		if strings.HasPrefix(line, "preset:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "preset:"))
		}
	}
	return ""
}

// stopByPidfile is cleanup, and it is pidfile-only on purpose: a `pkill` pattern matching the word
// proxy is out of the question on boxes shared with other engineers and other sessions running as
// the same unix user.
func stopByPidfile(pidfile string) {
	b, err := os.ReadFile(pidfile)
	if err != nil {
		return
	}
	if pid := strings.TrimSpace(string(b)); pid != "" {
		exec.Command("kill", pid).Run() //nolint:errcheck
	}
}

// TestTheStartupNoteUsesAStrategyNameSettingsKnows.
//
// Caught by reading a test run's output, not by a test: after `split` was renamed to `none`,
// start-proxy.sh went on printing
//
//	proxy up on 127.0.0.1:39863 (preset off, cache strategy split, idle-exit 24h)
//
// so the note announced a strategy nobody could ask for any more, named after a component that is
// not in the default pipeline. A second encoding of a name settings.py already owns — the same shape
// as the five copies of the preset default, and it survived the rename because the drift test above
// only covered the preset.
//
// Asserted against the names settings.py REPORTS rather than a hardcoded list, so renaming a
// strategy again fails here instead of going quiet.
func TestTheStartupNoteUsesAStrategyNameSettingsKnows(t *testing.T) {
	facts, code := settings(t, "strategy", "list")
	if code != 0 || facts["names"] == "" {
		t.Fatalf("could not read the strategy names from settings.py (exit %d): %v", code, facts)
	}
	known := map[string]bool{}
	for _, n := range strings.Split(facts["names"], ",") {
		known[strings.TrimSpace(n)] = true
	}

	body := readFileString(t, filepath.Join(scriptsDir(t), "start-proxy.sh"))
	got := betweenQuotes(t, body, "STRATEGY_NOTE=")
	if got == "" {
		t.Fatal("start-proxy.sh no longer assigns STRATEGY_NOTE a literal default; this test reads " +
			"it and needs re-anchoring")
	}
	if !known[got] {
		t.Errorf("start-proxy.sh reports the no-config case as cache strategy %q, which settings.py "+
			"does not list (%s). The startup note would name a strategy the user cannot ask for",
			got, facts["names"])
	}
}

// TestStartProxyReplacesAProxyThatReleasedItsPortButHasNotExited.
//
// Reported in review, and the failure it guards is the worst kind this file can produce: a session
// routed at a port with nothing listening, told the opposite.
//
// main.go handles SIGTERM through http.Server.Shutdown, which closes the LISTENERS FIRST and only
// then drains in-flight requests, for up to 25 s. So a proxy routinely spends time having released
// the port while still running - a streaming completion easily runs past ten seconds, and on a
// shared port it is somebody else's turn holding the drain open.
//
// stop_running_proxy used to wait on the PID for 10 s and treat that window as failure. The hook then
// printed "it is still running with <old fingerprint>" and exited 0, having started nothing. The
// message pointed the reader away from the one thing that was wrong: the port was dead.
//
// The fake here drains exactly that way - on SIGTERM it closes its listener and keeps running - so
// this fails against the pid-waiting version and passes against the port-waiting one.
func TestStartProxyReplacesAProxyThatReleasedItsPortButHasNotExited(t *testing.T) {
	dir, state := t.TempDir(), t.TempDir()
	port := freePort(t)
	requireTool(t, "python3")

	// Serves /healthz; on SIGTERM closes the socket and keeps running rather than exiting.
	draining := filepath.Join(dir, "draining-proxy")
	body := "#!/usr/bin/env bash\nexec python3 -c '\n" +
		"import signal, sys, threading, http.server\n" +
		"H = type(\"H\", (http.server.BaseHTTPRequestHandler,), {\n" +
		"    \"do_GET\": lambda s: (s.send_response(200), s.end_headers(), s.wfile.write(b\"ok\")),\n" +
		"    \"log_message\": lambda s, *a: None})\n" +
		"srv = http.server.HTTPServer((\"127.0.0.1\", int(sys.argv[1])), H)\n" +
		"def drain(sig, frm):\n" +
		"    threading.Thread(target=srv.shutdown, daemon=True).start()\n" +
		"signal.signal(signal.SIGTERM, drain)\n" +
		"srv.serve_forever()\n" +
		"srv.server_close()\n" +
		"while True: signal.pause()\n" +
		"' " + port + "\n"
	if err := os.WriteFile(draining, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	if out, code := startProxyIn(t, state, draining, port, "TMPDIR="+dir); code != 0 {
		t.Fatalf("first start failed: exit %d %s", code, out)
	}
	pidfile := filepath.Join(state, "proxy-"+port+".pid")
	t.Cleanup(func() { stopByPidfile(pidfile) })
	first := strings.TrimSpace(readFileString(t, pidfile))

	// A normal proxy for the replacement, and a changed preset so the restart path runs.
	fresh := servingProxy(t, dir, port, filepath.Join(dir, "started"))
	out, code := startProxyIn(t, state, fresh, port, "TMPDIR="+dir,
		"CLAUDE_PLUGIN_OPTION_PRESET=housellm")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}

	// The claim that matters: something is listening when this returns.
	resp, err := http.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		t.Fatalf("nothing is listening on %s after the hook returned, so this session is routed at a "+
			"dead port - the reported defect:\n%s", port, out)
	}
	resp.Body.Close()

	if got := strings.TrimSpace(readFileString(t, pidfile)); got == first {
		t.Errorf("the pidfile still names the drained proxy (%s); a later stop would signal the wrong "+
			"process, or none", first)
	}
	if strings.Contains(out, "STILL ANSWERING") {
		t.Errorf("reported the old proxy as still answering, but its port was already released:\n%s", out)
	}
}

// TestStartProxyDoesNotRecordAConfigurationItFailedToStart.
//
// Reported in review, and the sharpest finding on this branch: the mechanism meant to remove the
// "changing the option does nothing" defect recreated it.
//
// The fingerprint used to be written on the strength of the post-launch health poll, which cannot
// tell "the process I just launched" from "something else already on this port". One transient
// failure of the idempotence probe skips the already-up block; this launch then fails to BIND because
// the port is occupied; the OLD proxy answers the poll; and a fingerprint is persisted describing a
// configuration that never ran. The next sessions no-op because fp_have == fp_want.
//
// Strictly worse than before the branch, and the reason is the shape of the change: sessions used to
// re-derive the answer, so a stale reading corrected itself, and a persisted false claim suppresses
// the correction instead.
//
// Simulated here by occupying the port with a listener that is NOT ours and giving the hook a binary
// that cannot bind, which is the same observable state.
func TestStartProxyDoesNotRecordAConfigurationItFailedToStart(t *testing.T) {
	dir, state := t.TempDir(), t.TempDir()

	// Somebody else's listener on the port we want, which fails its FIRST health check and answers
	// every one after it. That transient failure is the reported trigger and it has to be simulated
	// rather than assumed: a listener that simply answers is caught by the idempotence probe in
	// section (2), the launch path never runs, and "no fingerprint was written" is then trivially
	// true — a test that passes while proving nothing. (It did, on the first attempt.)
	//
	// One 503 makes `curl -fsS` fail, so the already-up block is skipped exactly as it would be on a
	// blip or on a proxy whose listener is closed while it drains; the post-launch poll then succeeds
	// against this same foreign listener, which is the state the fix has to see through.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var probes atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if probes.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok")) //nolint:errcheck
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln) //nolint:errcheck
	defer srv.Close()
	port := fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)

	// A "proxy" that STAYS ALIVE and never binds, which is what a real one looks like while it starts.
	//
	// The earlier stand-in exited immediately, and that made this test pass for the wrong reason: the
	// liveness check was false because the process was already gone, not because the check could tell
	// whose proxy answered. Review supplied the timeline that exposed it — the post-launch poll was
	// satisfied 22 ms BEFORE the real binary exec'd, with its bind attempt 150 ms further out, so on
	// real timings the recorded pid is necessarily alive and the check is not a check.
	//
	// This is the same vacuity shape as the first version of this test, one layer down: `probes >= 2`
	// proves the launch path ran, and nothing proved the launched thing lived long enough for the
	// ownership question to be asked at all. Sleeping past the whole health budget is what asks it.
	cannotBind := filepath.Join(dir, "cannot-bind")
	if err := os.WriteFile(cannotBind,
		[]byte("#!/usr/bin/env bash\necho 'bind: address already in use' >&2\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopByPidfile(filepath.Join(state, "proxy-"+port+".pid")) })

	out, code := startProxyIn(t, state, cannotBind, port, "TMPDIR="+dir,
		"CLAUDE_PLUGIN_OPTION_PRESET=housellm")
	if code != 0 {
		t.Errorf("exit %d - must never fail the session: %s", code, out)
	}

	// Guard against the vacuous version of this test: the already-up block must have been SKIPPED and
	// a launch attempted, or "nothing was recorded" says nothing about the fix.
	if got := probes.Load(); got < 2 {
		t.Fatalf("only %d health probe(s) reached the listener, so the launch path was never entered "+
			"and this test proves nothing about the fingerprint", got)
	}
	// Second guard, for the second vacuity: the launched process must still have been ALIVE, or the
	// decision was taken on liveness rather than on ownership and this test would pass against the
	// version review showed to be wrong.
	if b, err := os.ReadFile(filepath.Join(state, "proxy-"+port+".pid")); err == nil {
		if pid := strings.TrimSpace(string(b)); pid != "" {
			if exec.Command("kill", "-0", pid).Run() != nil {
				t.Fatalf("the launched stand-in (pid %s) was already dead when the hook decided, so "+
					"this test exercises the liveness check rather than the ownership check", pid)
			}
		}
	}

	// The whole point: nothing may be recorded about a configuration that never ran.
	fp := filepath.Join(state, "proxy-"+port+".fingerprint")
	if b, err := os.ReadFile(fp); err == nil {
		t.Errorf("recorded %q for a proxy that never bound. The next session reads this, finds it "+
			"matches what the options ask for, and no-ops - so the configuration is permanently "+
			"believed to be in effect while something else serves the port:\n%s", strings.TrimSpace(string(b)), out)
	}
	// And it has to SAY so, or the user is told the new settings are live.
	if !strings.Contains(out, "something") {
		t.Errorf("said nothing about the port being held by another process:\n%s", out)
	}
}

// TestStrategySyncLeavesAnUnnamedConfigAlone. The one sub-case of the sync checklist with no test,
// noticed in review.
//
// A config we own but that predates named strategies has no name to re-render FROM, so sync has
// nothing to do and must say that rather than guess. Guessing the default would be the worst answer:
// it would rewrite a file that may set anything, under a name the user never chose.
func TestStrategySyncLeavesAnUnnamedConfigAlone(t *testing.T) {
	state, home := t.TempDir(), t.TempDir()
	port := "8787"
	cfg := filepath.Join(state, "keepalive-"+port+".yaml")
	// Our marker, deliberately with no strategy= in it: the shape the old keepalive skill wrote.
	ours := "# context-guru: written by /context-guru:keepalive\npreset: cache\ncache:\n  keepalive: true\n"
	if err := os.WriteFile(cfg, []byte(ours), 0o600); err != nil {
		t.Fatal(err)
	}

	facts, code := settingsIn(t, state, home, "strategy", "sync", "--port", port, "--preset", "housellm")
	if code != 0 {
		t.Errorf("exit %d - an unnamed config must not fail the SessionStart path: %v", code, facts)
	}
	if facts["result"] != "skipped" || facts["reason"] != "unnamed" {
		t.Errorf("result=%q reason=%q, want skipped/unnamed: %v", facts["result"], facts["reason"], facts)
	}
	if got := readFileString(t, cfg); got != ours {
		t.Errorf("rewrote a config with no name to re-render from.\nwas:\n%s\nnow:\n%s", ours, got)
	}
}

// TestStartProxyDoesNotRecordWhenAnotherProcessOwnsThePort closes the coverage gap I flagged on the
// round-3 fix: the `owner != started` branch of the ownership check.
//
// The recipe is review-pr-249-250's, and it is better than what I could see. Do not try to arrange a
// foreign process taking the port between two probes - that makes the test's own timing an
// assumption, which is the defect it would be testing for. Construct the PREDICATE's condition
// directly: a stand-in that hands the socket to a GRANDCHILD and then stays alive.
//
// Why that is the honest version. The branch's claim is "the pid I recorded is not the one holding the
// socket", and this produces exactly that: the bind SUCCEEDS, health PASSES, and $started is ALIVE -
// so the liveness fallback cannot catch it and only the ownership comparison can. No race, no probe
// counting, no dependence on how fast anything starts. It is also a realistic shape rather than a
// contrivance: a proxy that daemonised itself would look precisely like this.
func TestStartProxyDoesNotRecordWhenAnotherProcessOwnsThePort(t *testing.T) {
	// Needs a way to ask who holds the socket. Without one the code documents a fallback to the
	// weaker liveness check, which this test cannot distinguish - so skip rather than assert, the same
	// discipline as the check-url guard.
	if _, errSS := exec.LookPath("ss"); errSS != nil {
		if _, errLsof := exec.LookPath("lsof"); errLsof != nil {
			t.Skip("neither ss nor lsof is available, so start-proxy.sh falls back to a liveness " +
				"check and the ownership branch under test cannot fire")
		}
	}
	requireTool(t, "python3")
	dir, state := t.TempDir(), t.TempDir()
	port := freePort(t)

	// The grandchild: binds the port and serves /healthz.
	helper := filepath.Join(dir, "helper.py")
	if err := os.WriteFile(helper, []byte(
		"import sys, http.server\n"+
			"H = type(\"H\", (http.server.BaseHTTPRequestHandler,), {\n"+
			"    \"do_GET\": lambda s: (s.send_response(200), s.end_headers(), s.wfile.write(b\"ok\")),\n"+
			"    \"log_message\": lambda s, *a: None})\n"+
			"http.server.HTTPServer((\"127.0.0.1\", int(sys.argv[1])), H).serve_forever()\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The stand-in: launches the grandchild, then stays alive WITHOUT owning the socket.
	daemonising := filepath.Join(dir, "daemonising-proxy")
	body := "#!/usr/bin/env bash\n" +
		"nohup python3 " + helper + " " + port + " >/dev/null 2>&1 &\n" +
		"exec sleep 300\n"
	if err := os.WriteFile(daemonising, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	out, code := startProxyIn(t, state, daemonising, port, "TMPDIR="+dir)
	pidfile := filepath.Join(state, "proxy-"+port+".pid")
	t.Cleanup(func() { stopByPidfile(pidfile) })
	t.Cleanup(func() { exec.Command("pkill", "-f", helper).Run() }) //nolint:errcheck // bracketed by full path
	if code != 0 {
		t.Errorf("exit %d - must never fail the session: %s", code, out)
	}

	// The precondition that makes this a test of OWNERSHIP and not of liveness: the recorded pid has
	// to still be alive. If it is not, the weaker check would have caught it and this proves nothing.
	pid := strings.TrimSpace(readFileString(t, pidfile))
	if pid == "" {
		t.Fatal("no pid was recorded, so the launch path did not run")
	}
	if exec.Command("kill", "-0", pid).Run() != nil {
		t.Fatalf("the recorded pid %s is not alive, so the liveness fallback could have decided this "+
			"and the ownership branch was not exercised:\n%s", pid, out)
	}

	// And nothing may be recorded, because our proxy never held the port.
	if b, err := os.ReadFile(filepath.Join(state, "proxy-"+port+".fingerprint")); err == nil {
		t.Errorf("recorded %q while another process owned the port. The next session would read it, "+
			"match it, and no-op - believing a configuration to be in effect that never was:\n%s",
			strings.TrimSpace(string(b)), out)
	}
	if !strings.Contains(out, "something") {
		t.Errorf("said nothing about another process holding the port:\n%s", out)
	}
}
