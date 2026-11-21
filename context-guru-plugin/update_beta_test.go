package plugin

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/internal/testutil"
)

// TestUpdateIsNewerOrdersVersionsLikeSemver (#1254). `releases/latest` never names a prerelease, so
// "the tags differ" must not count as an update: an installed beta differs from the older stable.
func TestUpdateIsNewerOrdersVersionsLikeSemver(t *testing.T) {
	cases := []struct {
		latest, installed, want string
	}{
		{"v0.7.4", "v0.7.5-beta.1", "false"}, // the bug: a beta is not behind the older stable
		{"v0.7.4", "0.7.5-beta.1", "false"},  // a `v` on one side only
		{"0.7.4", "v0.7.5-beta.1", "false"},
		{"v0.7.5", "v0.7.5-beta.1", "true"}, // the stable of the same version ships
		{"v0.7.5", "0.7.5-beta.2", "true"},
		{"v0.7.5-beta.2", "v0.7.5-beta.1", "true"},
		{"v0.7.5-beta.1", "v0.7.5-beta.2", "false"},
		{"v0.7.5-beta.10", "v0.7.5-beta.9", "true"}, // numeric, not text, order
		{"v0.7.5-rc.1", "v0.7.5-beta.9", "true"},
		{"v0.7.5-beta", "v0.7.5-beta.1", "false"}, // fewer parts is lower
		{"v0.7.5", "v0.7.5", "false"},             // equal
		{"v0.7.5", "0.7.5", "false"},
		{"v0.7.5-beta.1", "v0.7.5-beta.1", "false"},
		{"v0.7.5", "v0.7.4", "true"},
		{"v0.10.0", "v0.9.9", "true"},
		{"v0.7.4", "v0.7.5", "false"},
		{"v0.8.0-beta.1", "v0.7.5", "true"},
		// Unparsable: keep the old test, "the tags differ", so the path still fails open.
		{"v0.7.5", "dev", "true"},
		{"v0.7.5", "", "true"},
		{"latest", "v0.7.5", "true"},
		{"dev", "dev", "false"},
		{"v0.7.5", "v0.7", "true"},
	}
	state, home := t.TempDir(), t.TempDir()
	for _, c := range cases {
		facts, code := settingsIn(t, state, home, "update-check", "newer", "--latest", c.latest, "--installed", c.installed)
		if code != 0 || facts["newer"] != c.want {
			t.Errorf("latest=%q installed=%q: newer=%q (exit %d), want %q", c.latest, c.installed, facts["newer"], code, c.want)
		}
	}
}

// updateBranchRun runs start-proxy.sh in a temp copy of the scripts whose install.sh is a stub that
// leaves a sentinel, so no real install runs. The update record is fresh, so no network is touched.
// It returns the output and whether the stub install ran.
func updateBranchRun(t *testing.T, installed, latest, answer string) (string, bool) {
	t.Helper()
	requireTool(t, "bash")
	dir, state, home := t.TempDir(), t.TempDir(), t.TempDir()
	scripts := filepath.Join(dir, "scripts")
	if err := os.MkdirAll(scripts, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"start-proxy.sh", "settings.py"} {
		b, err := os.ReadFile(filepath.Join(scriptsDir(t), name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(scripts, name), b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sentinel := filepath.Join(dir, "install-ran")
	stub := "#!/bin/sh\necho \"$CONTEXT_GURU_UPGRADE\" > '" + sentinel + "'\n"
	if err := os.WriteFile(filepath.Join(scripts, "install.sh"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "proxy-version"), []byte(installed+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if answer != "" {
		settingsIn(t, state, home, "update-check", "answer", "--answer", answer)
	}
	settingsIn(t, state, home, "update-check", "stamp", "--latest", latest)

	port := freePort(t)
	bin := servingProxy(t, dir, port, filepath.Join(dir, "started"))
	out, _ := startProxyScript(t, filepath.Join(scripts, "start-proxy.sh"), state, home, bin, port, "TMPDIR="+dir)
	t.Cleanup(func() { stopByPidfile(filepath.Join(state, "proxy-"+port+".pid")) })
	// Deterministic: start-proxy.sh starts the install and prints its note in the same branch, so
	// the note is the end-of-decision signal. With the note, wait for the stub (no time limit of
	// our own). Without it, the branch was not taken and the stub cannot run.
	if !strings.Contains(out, "updating the proxy in the background") {
		_, err := os.Stat(sentinel)
		return out, err == nil
	}
	testutil.WaitFor(t, "the stub install.sh ran", func() bool {
		_, err := os.Stat(sentinel)
		return err == nil
	})
	return out, true
}

func TestStartProxyNeverUpdatesABetaToAnOlderStable(t *testing.T) {
	for _, answer := range []string{"always", ""} {
		out, ran := updateBranchRun(t, "v0.7.5-beta.1", "v0.7.4", answer)
		if ran || strings.Contains(out, "updating the proxy") || strings.Contains(out, "newer proxy is released") {
			t.Errorf("answer=%q: an installed beta v0.7.5-beta.1 was offered or given v0.7.4 (install ran: %v):\n%s", answer, ran, out)
		}
	}
}

func TestStartProxyUpdatesABetaToTheStableOfTheSameVersion(t *testing.T) {
	out, ran := updateBranchRun(t, "v0.7.5-beta.1", "v0.7.5", "always")
	if !ran || !strings.Contains(out, "updating the proxy in the background (v0.7.5-beta.1 -> v0.7.5)") {
		t.Errorf("the stable v0.7.5 did not replace beta.1 (install ran: %v):\n%s", ran, out)
	}
}

func TestStartProxyUpdatesAStableToANewerStable(t *testing.T) {
	out, ran := updateBranchRun(t, "v0.7.4", "v0.7.5", "always")
	if !ran || !strings.Contains(out, "updating the proxy in the background") {
		t.Errorf("a newer stable did not install (install ran: %v):\n%s", ran, out)
	}
}

func TestStartProxyAsksAboutANewerStableOnly(t *testing.T) {
	out, _ := updateBranchRun(t, "v0.7.4", "v0.7.5", "")
	if !strings.Contains(out, "a newer proxy is released: v0.7.5") {
		t.Errorf("answer ask: no notice for a newer stable:\n%s", out)
	}
}

// startProxyScript is startProxyIn for a start-proxy.sh at another path (a temp copy of the scripts).
func startProxyScript(t *testing.T, script, state, home, bin, port string, extra ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("bash", script)
	cmd.Env = append(sandboxEnv(t),
		"CONTEXT_GURU_STATE="+state,
		"HOME="+home,
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

// fakeReleaseTools puts a fake `curl` (answers the releases/latest redirect with `latest`) and a fake
// context-guru-proxy (prints `installed`) first on PATH. No network, no real binary.
func fakeReleaseTools(t *testing.T, installed, latest string) (path, dest string) {
	t.Helper()
	bin := t.TempDir()
	curl := "#!/bin/sh\nprintf 'https://github.com/rossoctl/context-guru/releases/tag/" + latest + "'\n"
	proxy := "#!/bin/sh\necho 'context-guru-proxy " + installed + "'\n"
	for name, body := range map[string]string{"curl": curl, "context-guru-proxy": proxy} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return bin + ":" + os.Getenv("PATH"), bin
}

func TestCheckLatestDoesNotCallABetaOutdated(t *testing.T) {
	requireTool(t, "bash")
	for _, c := range []struct{ installed, latest, want string }{
		{"v0.7.5-beta.1", "v0.7.4", "false"},
		{"v0.7.5-beta.1", "v0.7.5", "true"},
		{"v0.7.4", "v0.7.5", "true"},
		{"v0.7.5", "v0.7.5", "false"},
	} {
		path, bin := fakeReleaseTools(t, c.installed, c.latest)
		cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "install.sh"), "--check-latest")
		cmd.Env = sandboxEnv(t, "PATH="+path, "CONTEXT_GURU_DEST="+bin, "CONTEXT_GURU_STATE="+t.TempDir())
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", c.installed, err, out)
		}
		if got := facts(string(out))["update_available"]; got != c.want {
			t.Errorf("installed %s, latest %s: update_available=%q, want %q\n%s", c.installed, c.latest, got, c.want, out)
		}
	}
}

func TestUpgradeNeverInstallsAnOlderStableOverABeta(t *testing.T) {
	requireTool(t, "bash")
	path, bin := fakeReleaseTools(t, "v0.7.5-beta.1", "v0.7.4")
	state := t.TempDir()
	cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "install.sh"))
	cmd.Env = sandboxEnv(t, "PATH="+path, "CONTEXT_GURU_DEST="+bin, "CONTEXT_GURU_STATE="+state, "CONTEXT_GURU_UPGRADE=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := facts(string(out))["result"]; got != "present" {
		t.Errorf("CONTEXT_GURU_UPGRADE=1 with a beta installed and an older latest: result=%q, want present\n%s", got, out)
	}
	if strings.Contains(string(out), "downloading") || strings.Contains(string(out), "result=installed") {
		t.Errorf("an install ran:\n%s", out)
	}
}

func writeUpdateRecord(t *testing.T, state, installed, latest string) {
	t.Helper()
	// `stamp` writes the record with the real ownership marker line.
	if _, code := settingsIn(t, state, t.TempDir(), "update-check", "stamp", "--latest", latest); code != 0 {
		t.Fatalf("update-check stamp exited %d", code)
	}
	if err := os.WriteFile(filepath.Join(state, "proxy-version"), []byte(installed+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestStatusLineShowsNoUpdateForABetaAheadOfLatest(t *testing.T) {
	py := requireTool(t, "python3")
	for _, c := range []struct {
		installed, latest string
		shown             bool
	}{
		{"v0.7.5-beta.1", "v0.7.4", false},
		{"0.7.5-beta.1", "v0.7.4", false},
		{"v0.7.5-beta.1", "v0.7.5", true},
		{"v0.7.4", "v0.7.5", true},
	} {
		state := t.TempDir()
		writeUpdateRecord(t, state, c.installed, c.latest)
		cmd := exec.Command(py, "-c", "import statusline; print(statusline._update_available_segment() or 'NONE')")
		cmd.Dir = scriptsDir(t)
		cmd.Env = sandboxEnv(t, "CONTEXT_GURU_STATE="+state)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if shown := !strings.Contains(string(out), "NONE"); shown != c.shown {
			t.Errorf("installed %s, latest %s: segment shown=%v, want %v (%q)", c.installed, c.latest, shown, c.shown, out)
		}
	}
}

func TestInsightsRaisesNoUpdateFindingForABetaAheadOfLatest(t *testing.T) {
	for _, c := range []struct {
		installed, latest string
		raised            bool
	}{
		{"v0.7.5-beta.1", "v0.7.4", false},
		{"v0.7.4", "v0.7.5", true},
	} {
		stub := newAPIStub(t, map[string]string{"/api/stats": window(3)})
		dir := insightsProject(t, map[string]any{"port": stub.port})
		writeUpdateRecord(t, filepath.Join(dir, "state"), c.installed, c.latest)
		out, _ := runInsights(t, dir, nil, "all")
		raised := false
		for _, id := range findingIDs(out) {
			raised = raised || id == "proxy-update"
		}
		if raised != c.raised {
			t.Errorf("installed %s, latest %s: proxy-update raised=%v, want %v\n%s", c.installed, c.latest, raised, c.raised, out)
		}
	}
}
