// Package bobplugin runs the Python tests of the IBM Bob plugin, so `go test ./...` in CI runs them.
//
// The plugin is Python (it runs on the user's Mac, next to Bob). CI runs only Go tests, so without
// this file nobody would run its tests. Every test uses a temp HOME and fakes; none touches the real
// HOME, launchd or Bob. The macOS-only tests (zsh, reset.sh) skip on the Linux CI runner.
package bobplugin

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestBobPluginPythonSuite(t *testing.T) {
	// Fail, do not skip: a skip looks like a pass in CI output (see context-guru-plugin requireTool).
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatalf("python3 is required to test the Bob plugin scripts: %v", err)
	}
	scripts := filepath.Join("context-guru", "scripts")
	cmd := exec.Command("python3", "-m", "unittest", "discover", "-s", scripts, "-p", "test_*.py")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Bob plugin Python tests failed: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}

// TestBobReleasePythonSuite runs the tests of the release installer, uninstaller and packager.
// Each runs in a temp HOME with a fake launchctl, bobide, pgrep and osascript. The .command
// tests skip off macOS; the packager test runs wherever zip and shasum are.
func TestBobReleasePythonSuite(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatalf("python3 is required to test the Bob release scripts: %v", err)
	}
	cmd := exec.Command("python3", "-m", "unittest", "discover", "-s", "release", "-p", "test_*.py")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Bob release Python tests failed: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}
