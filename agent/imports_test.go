package agent

import (
	"os/exec"
	"strings"
	"testing"
)

const module = "github.com/rossoctl/context-guru"

// agent is a library package: a host (the proxy, the bifrost adapter) imports it, never the
// reverse. This test enforces that rule. It reads the full dependency list, so an indirect
// import through another package also fails it.
func TestAgentDoesNotImportAHost(t *testing.T) {
	// The go on PATH: the toolchain running this test. runtime.GOROOT is deprecated and can be
	// empty under -trimpath or a moved toolchain.
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("go is not on PATH: %v", err)
	}
	out, err := exec.Command(goBin, "list", "-deps", ".").CombinedOutput()
	if err != nil {
		// Fatal, not Skip: a guard that skips proves nothing.
		t.Fatalf("go list failed: %v\n%s", err, out)
	}
	deps := strings.Fields(string(out))
	if !contains(deps, module+"/agent") {
		t.Fatalf("go list did not list the agent package; the guard would prove nothing:\n%s", out)
	}
	for _, pkg := range deps {
		if isHost(pkg) {
			t.Errorf("package agent depends on host package %s; move the logic into a library package", pkg)
		}
	}
}

func TestIsHostMatchesEveryHostPackage(t *testing.T) {
	for _, pkg := range []string{module + "/proxy", module + "/proxy/sub", module + "/proxyapp",
		module + "/cmd/context-guru-proxy"} {
		if !isHost(pkg) {
			t.Errorf("isHost(%q) = false, want true", pkg)
		}
	}
	for _, pkg := range []string{module + "/agent", module + "/apply", module + "/proxyish", "net/http"} {
		if isHost(pkg) {
			t.Errorf("isHost(%q) = true, want false", pkg)
		}
	}
}

func isHost(pkg string) bool {
	for _, host := range []string{"/proxy", "/proxyapp", "/cmd"} {
		root := module + host
		if pkg == root || strings.HasPrefix(pkg, root+"/") {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
