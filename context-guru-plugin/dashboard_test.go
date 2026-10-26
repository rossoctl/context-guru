package plugin

// The dashboard skill opens the History and Cleanup tabs of THIS install's proxy.
//
// Executed rather than reviewed, like the other scripts here: the failures that matter are all
// "it opened the wrong thing" or "it opened a dead page", and none is visible by reading.

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// runDashboard runs scripts/dashboard.sh with a sandboxed environment. `opener` replaces the
// browser launcher with a script that records what it was asked to open, so no test ever starts a
// real browser.
func runDashboard(t *testing.T, extra ...string) (out string, opened []string, code int) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "opened.log")
	opener := filepath.Join(dir, "opener.sh")
	if err := os.WriteFile(opener, []byte("#!/bin/sh\necho \"$1\" >> '"+log+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(scriptsDir(t), "dashboard.sh"))
	cmd.Env = append(sandboxEnv(t), append([]string{
		"HOME=" + dir, "XDG_STATE_HOME=" + filepath.Join(dir, "state"),
		"CONTEXT_GURU_DASHBOARD_OPENER=" + opener,
	}, extra...)...)
	b, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running dashboard.sh: %v (%s)", err, b)
	}
	if lb, err := os.ReadFile(log); err == nil {
		for _, l := range strings.Split(strings.TrimSpace(string(lb)), "\n") {
			if l != "" {
				opened = append(opened, l)
			}
		}
	}
	return string(b), opened, code
}

// fakeDashboard serves /api/capture so the liveness probe passes, and returns its port. Only that
// route: a probe of a removed route (/api/history) must fail here as it would on a real proxy.
func fakeDashboard(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/capture" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"mode":"active"}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// A live install: the Cleanup tab opened, on the RESOLVED port, and its URL printed. The History
// tab is gone, so it is neither opened nor printed.
func TestTheDashboardOpensCleanupOnTheResolvedPort(t *testing.T) {
	port := fakeDashboard(t)
	out, opened, code := runDashboard(t, "CONTEXT_GURU_DASHBOARD_PORT="+port)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	want := []string{"http://127.0.0.1:" + port + "/dashboard/#savings/insights"}
	if strings.Join(opened, "\n") != strings.Join(want, "\n") {
		t.Errorf("opened %q; want exactly %q", opened, want)
	}
	if strings.Contains(out, "#savings/history") {
		t.Errorf("output still links the removed History tab:\n%s", out)
	}
	// Printed as well as opened: over SSH or on a headless machine the open does nothing, and the
	// URLs are then the only way in.
	for _, u := range want {
		if !strings.Contains(out, u) {
			t.Errorf("output does not print %s, so a session where the browser cannot open has no "+
				"way to reach the page:\n%s", u, out)
		}
	}
}

// NEVER 8787, NEVER A GUESS. Ports are allocated per project, so a guessed port is somebody else's
// proxy, and every figure on the page would be that install's spend shown under this user's name.
func TestTheDashboardRefusesToGuessAPortWhenNothingIsInstalled(t *testing.T) {
	// No override and an empty HOME: settings.py has nothing to resolve.
	out, opened, code := runDashboard(t)
	if code == 0 {
		t.Errorf("exit 0 with nothing installed; want a non-zero exit:\n%s", out)
	}
	if len(opened) != 0 {
		t.Errorf("opened %q with no install. A guessed port shows another project's spend under "+
			"this user's name", opened)
	}
	if strings.Contains(out, ":8787") {
		t.Errorf("output mentions port 8787 with nothing installed; the default is whichever "+
			"project installed first, never a safe guess:\n%s", out)
	}
	if !strings.Contains(out, "/context-guru:install") {
		t.Errorf("output does not say how to install:\n%s", out)
	}
}

// A dead proxy: say so and how to start it, instead of opening a "connection refused" page that
// reads as a broken product.
func TestTheDashboardDoesNotOpenADeadPage(t *testing.T) {
	// A port nothing listens on: bind, note it, close.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	l.Close()

	out, opened, code := runDashboard(t, "CONTEXT_GURU_DASHBOARD_PORT="+port)
	if code == 0 {
		t.Errorf("exit 0 with the proxy down:\n%s", out)
	}
	if len(opened) != 0 {
		t.Errorf("opened %q against a proxy that is not running; the reader gets 'connection "+
			"refused' and concludes the product is broken", opened)
	}
	if !strings.Contains(out, "start-proxy.sh") && !strings.Contains(out, "/context-guru:status") {
		t.Errorf("output does not say how to get the proxy running:\n%s", out)
	}
}

// --no-open prints and opens nothing, for a session that cannot open a browser or does not want one.
func TestTheDashboardCanPrintWithoutOpening(t *testing.T) {
	port := fakeDashboard(t)
	cmd := exec.Command(filepath.Join(scriptsDir(t), "dashboard.sh"), "--no-open")
	dir := t.TempDir()
	log := filepath.Join(dir, "opened.log")
	opener := filepath.Join(dir, "opener.sh")
	_ = os.WriteFile(opener, []byte("#!/bin/sh\necho \"$1\" >> '"+log+"'\n"), 0o755)
	cmd.Env = append(sandboxEnv(t), "HOME="+dir, "CONTEXT_GURU_DASHBOARD_PORT="+port,
		"CONTEXT_GURU_DASHBOARD_OPENER="+opener)
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dashboard.sh --no-open: %v (%s)", err, b)
	}
	if _, err := os.Stat(log); err == nil {
		lb, _ := os.ReadFile(log)
		t.Errorf("--no-open opened %q anyway", strings.TrimSpace(string(lb)))
	}
	if !strings.Contains(string(b), "/dashboard/#savings/insights") {
		t.Errorf("--no-open printed no URL:\n%s", b)
	}
}

// The skill may run exactly this script and nothing else. It reads the user's own spend; there is
// no reason for it to hold a general shell.
func TestTheDashboardSkillIsLockedToItsScript(t *testing.T) {
	body := readFileString(t, filepath.Join("skills", "dashboard", "SKILL.md"))
	if !strings.Contains(body, "name: dashboard") {
		t.Error("skills/dashboard/SKILL.md has no `name: dashboard` front matter")
	}
	if !strings.Contains(body, "allowed-tools: Bash(${CLAUDE_PLUGIN_ROOT}/scripts/dashboard.sh") {
		t.Error("allowed-tools is not restricted to scripts/dashboard.sh")
	}
	// What the page can and cannot claim: Cleanup covers only the time since install, and removal
	// is done by copying the command it builds -- nothing on the page runs it.
	for _, want := range []string{"since", "copy"} {
		if !strings.Contains(strings.ToLower(body), want) {
			t.Errorf("SKILL.md never says %q. It has to tell the user Cleanup covers only the time "+
				"since install, and that removal is done by copying the command Cleanup builds", want)
		}
	}
	if strings.Contains(body, "History") {
		t.Error("SKILL.md still describes the removed History tab")
	}
}

// No skill in either plugin may send the user to the removed History tab. The dashboard skill was
// fixed with the tab; the install skill was missed and still said "History is not empty on day
// one", so this walks every skill rather than naming one. It looks for the tab, not the word:
// "your own history" (past traffic) is fine.
func TestNoSkillMentionsTheRemovedHistoryTab(t *testing.T) {
	tab := regexp.MustCompile(`\bHistory (is|tab|page|view|shows|says|covers|lists)\b|\*\*History\*\*|History —|savings/history|/api/history`)
	var files []string
	for _, root := range []string{"skills", filepath.Join("..", "codex-marketplace", "plugins", "context-guru", "skills")} {
		m, err := filepath.Glob(filepath.Join(root, "*", "SKILL.md"))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	if len(files) < 10 {
		t.Fatalf("found only %d skills; the glob no longer matches, update this test", len(files))
	}
	for _, f := range files {
		if loc := tab.FindString(readFileString(t, f)); loc != "" {
			t.Errorf("%s still names the removed History tab (%q)", f, loc)
		}
	}
}
