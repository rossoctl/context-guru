package bobplugin

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestBobStatusbarSuite runs the status bar extension's tests (node --test), so CI covers them.
// It skips when npm is not on PATH. The CI runner (ubuntu-latest) has Node and npm installed.
// When node_modules is absent it runs `npm ci`, which needs the network once.
func TestBobStatusbarSuite(t *testing.T) {
	// npm reads files that the Go test cache does not see. Reading them here puts them in the
	// cache key, so an edit to a TS file or package.json runs the test again.
	trackSources(t, "statusbar")
	npm, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("npm is not on PATH; the status bar tests need Node")
	}
	dir := "statusbar"
	if _, err := os.Stat(filepath.Join(dir, "node_modules")); err != nil {
		if out, err := command(dir, npm, "ci", "--ignore-scripts", "--no-audit", "--no-fund"); err != nil {
			// Offline on a laptop is not a failure. In CI it is: a skip there looks like a pass.
			if os.Getenv("CI") == "" {
				t.Skipf("npm ci failed (offline?); set CI to make this fail: %v\n%s", err, out)
			}
			t.Fatalf("npm ci failed: %v\n%s", err, out)
		}
	}
	out, err := command(dir, npm, "test")
	if err != nil {
		t.Fatalf("npm test failed: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}

func trackSources(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"package.json", "package-lock.json", "tsconfig.json"} {
		if _, err := os.ReadFile(filepath.Join(dir, name)); err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
	}
	for _, sub := range []string{"src", "test"} {
		err := filepath.WalkDir(filepath.Join(dir, sub), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			_, err = os.ReadFile(path)
			return err
		})
		if err != nil {
			t.Fatalf("read %s: %v", sub, err)
		}
	}
}

func command(dir, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}
