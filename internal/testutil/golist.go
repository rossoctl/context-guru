package testutil

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// GoListDeps runs `go list -deps <pkgs>` and returns one import path per line.
//
// It removes -json from GOFLAGS. The heavy CI run sets GOFLAGS=-json to time each test. With
// that flag, `go list` prints JSON instead of import paths, and a dependency guard that reads the
// output proves nothing. The other GOFLAGS (for example -tags) stay.
func GoListDeps(t testing.TB, pkgs ...string) string {
	t.Helper()
	goBin, err := exec.LookPath("go") // the toolchain that runs this test
	if err != nil {
		t.Fatalf("go is not on PATH: %v", err)
	}
	cmd := exec.Command(goBin, append([]string{"list", "-deps"}, pkgs...)...)
	cmd.Env = append(os.Environ(), "GOFLAGS="+withoutJSON(os.Getenv("GOFLAGS")))
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Fatal, not Skip: a guard that skips proves nothing.
		t.Fatalf("go list failed: %v\n%s", err, out)
	}
	return string(out)
}

func withoutJSON(flags string) string {
	var keep []string
	for _, f := range strings.Fields(flags) {
		if f == "-json" || strings.HasPrefix(f, "-json=") {
			continue
		}
		keep = append(keep, f)
	}
	return strings.Join(keep, " ")
}
