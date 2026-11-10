// The Pi package's own tests, run from `go test ./...` so CI runs them with the rest of the repo.
// The Python tests use a temporary HOME and never start a service or open the network. The Node tests
// cover the extension's pure helpers; they need Node 22.18 or newer, which strips the TypeScript types.
package pipackage

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestPiControllerPythonTests(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	cmd := exec.Command("python3", "-I", "-W", "ignore", "-m", "unittest", "discover", "-s", "scripts")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("python tests: %v\n%s", err, out)
	}
}

func TestPiExtensionNodeTests(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	// Node 22.18 is the first release that strips TypeScript types without a flag.
	if exec.Command("node", "-e", nodeAtLeast2218).Run() != nil {
		t.Skip("node is older than 22.18 and cannot run TypeScript tests")
	}
	files, _ := filepath.Glob(filepath.Join("tests", "*.test.ts"))
	if len(files) == 0 {
		t.Fatal("no extension tests found")
	}
	if out, err := exec.Command("node", append([]string{"--test"}, files...)...).CombinedOutput(); err != nil {
		t.Fatalf("node tests: %v\n%s", err, out)
	}
}

const nodeAtLeast2218 = "const [a, b] = process.versions.node.split('.').map(Number); process.exit(a > 22 || (a === 22 && b >= 18) ? 0 : 1)"
