package models

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A host imports this package to resolve model windows without linking the proxy (issue #1251).
// If models or models/litellm ever links proxy, proxyapp, tenant or dash, that host links the whole proxy again.
func TestModelsLinksNoProxy(t *testing.T) {
	goBin := filepath.Join(runtime.GOROOT(), "bin", "go")
	out, err := exec.Command(goBin, "list", "-deps", ".", "./litellm").CombinedOutput()
	if err != nil {
		// Fatal, not Skip: a guard that skips proves nothing.
		t.Fatalf("go list failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "github.com/rossoctl/context-guru/internal/modelinfo") {
		t.Fatalf("go list did not list internal/modelinfo; the guard would prove nothing:\n%s", out)
	}
	banned := map[string]bool{}
	for _, p := range []string{"proxy", "proxyapp", "tenant", "dash", "components/all"} {
		banned["github.com/rossoctl/context-guru/"+p] = true
	}
	for _, pkg := range strings.Fields(string(out)) {
		if banned[pkg] {
			t.Errorf("models links %s; a host that needs only the resolver would link the proxy", pkg)
		}
	}
}
