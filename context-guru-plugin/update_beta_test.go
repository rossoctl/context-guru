package plugin

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	out, _, ran := updateBranchRunEnv(t, installed, latest, answer)
	return out, ran
}

// updateBranchRunEnv is updateBranchRun that also returns what the stub saw: "<UPGRADE> <VERSION>".
func updateBranchRunEnv(t *testing.T, installed, latest, answer string) (string, string, bool) {
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
	stub := "#!/bin/sh\necho \"$CONTEXT_GURU_UPGRADE $CONTEXT_GURU_VERSION\" > '" + sentinel + "'\n"
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
		return out, "", err == nil
	}
	testutil.WaitFor(t, "the stub install.sh ran", func() bool {
		_, err := os.Stat(sentinel)
		return err == nil
	})
	seen, _ := os.ReadFile(sentinel)
	return out, strings.TrimSpace(string(seen)), true
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

// TestStartProxyInstallsTheTagItNames (#1276). The notice names the tag the check found. The
// background install must get that exact tag, not resolve "latest" again.
func TestStartProxyInstallsTheTagItNames(t *testing.T) {
	out, seen, ran := updateBranchRunEnv(t, "v0.7.3", "v0.7.4", "always")
	if !ran || !strings.Contains(out, "(v0.7.3 -> v0.7.4)") {
		t.Fatalf("no background update (install ran: %v):\n%s", ran, out)
	}
	if seen != "1 v0.7.4" {
		t.Errorf("the install saw %q, want \"1 v0.7.4\" (UPGRADE=1, VERSION=<checked tag>)", seen)
	}
}

// releaseFixture builds a fake `curl` and a fake release tarball for tag, with a fake proxy that
// prints `proxy <tag>`. It serves the releases/latest redirect (as `latest`) and the download.
func releaseFixture(t *testing.T, tag, latest string) (path string) {
	t.Helper()
	fix, bin := t.TempDir(), t.TempDir()
	arch := map[string]string{"amd64": "amd64", "arm64": "arm64"}[runtime.GOARCH]
	if arch == "" || (runtime.GOOS != "darwin" && runtime.GOOS != "linux") {
		t.Skipf("no release fixture for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	name := "context-guru_" + strings.TrimPrefix(tag, "v") + "_" + runtime.GOOS + "_" + arch + ".tar.gz"
	body := []byte("#!/bin/sh\necho 'context-guru-proxy " + tag + "'\n")
	f, err := os.Create(filepath.Join(fix, name))
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "pkg/context-guru-proxy", Mode: 0o755, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	tw.Write(body)
	tw.Close()
	gz.Close()
	f.Close()
	curl := "#!/bin/sh\nout=''; prev=''\nfor a in \"$@\"; do [ \"$prev\" = -o ] && out=$a; prev=$a; url=$a; done\n" +
		"if [ \"$out\" = /dev/null ]; then printf 'https://github.com/rossoctl/context-guru/releases/tag/" + latest + "'; exit 0; fi\n" +
		"cp '" + fix + "'/\"$(basename \"$url\")\" \"$out\"\n"
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(curl), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin + ":" + os.Getenv("PATH")
}

// runInstallExplicit runs install.sh with an installed proxy `installed`, a pinned `tag` and the
// given extra env. It returns the output, the state dir and the dest dir.
func runInstallExplicit(t *testing.T, installed, tag, latest string, extra ...string) (string, string, string) {
	t.Helper()
	requireTool(t, "bash")
	requireTool(t, "python3")
	path := releaseFixture(t, tag, latest)
	_, dest := fakeReleaseTools(t, installed, latest)
	// fakeReleaseTools put its own curl first; the fixture curl must win.
	path = strings.SplitN(path, ":", 2)[0] + ":" + dest + ":" + os.Getenv("PATH")
	if err := os.Remove(filepath.Join(dest, "curl")); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	settingsIn(t, state, t.TempDir(), "update-check", "stamp", "--latest", latest)
	cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "install.sh"))
	cmd.Env = sandboxEnv(t, append([]string{"PATH=" + path, "CONTEXT_GURU_DEST=" + dest, "CONTEXT_GURU_STATE=" + state,
		"CONTEXT_GURU_INSECURE=1", "CONTEXT_GURU_VERSION=" + tag}, extra...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return string(out), state, dest
}

// TestUpgradeWritesTheInstalledTagToTheUpdateRecord (#1276).
func TestUpgradeWritesTheInstalledTagToTheUpdateRecord(t *testing.T) {
	out, state, _ := runInstallExplicit(t, "v0.7.3", "v0.7.4", "v0.7.3", "CONTEXT_GURU_UPGRADE=1")
	if got := facts(out)["result"]; got != "installed" {
		t.Fatalf("result=%q, want installed\n%s", got, out)
	}
	shown, _ := settingsIn(t, state, t.TempDir(), "update-check", "show")
	if shown["latest"] != "v0.7.4" || shown["installed"] != "v0.7.4" {
		t.Errorf("record after the install: latest=%q installed=%q, want both v0.7.4", shown["latest"], shown["installed"])
	}
}

// TestUpgradeRefusesAnExplicitTagThatIsNotNewer (#1276). The #1256 guard also holds for a pinned tag.
func TestUpgradeRefusesAnExplicitTagThatIsNotNewer(t *testing.T) {
	for _, tag := range []string{"v0.7.3", "v0.7.2"} {
		out, _, _ := runInstallExplicit(t, "v0.7.3", tag, "v0.7.3", "CONTEXT_GURU_UPGRADE=1")
		if got := facts(out)["result"]; got != "present" || strings.Contains(out, "result=installed") {
			t.Errorf("UPGRADE=1 with pinned %s over v0.7.3: result=%q, want present\n%s", tag, got, out)
		}
	}
}

// TestPinnedTagStillInstallsWithoutUpgrade (#1276). The manual repair path installs any tag.
func TestPinnedTagStillInstallsWithoutUpgrade(t *testing.T) {
	out, _, _ := runInstallExplicit(t, "v0.7.3", "v0.7.2", "v0.7.3")
	if got := facts(out)["result"]; got != "installed" {
		t.Errorf("pinned older tag without UPGRADE: result=%q, want installed\n%s", got, out)
	}
}
