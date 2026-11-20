package models

import (
	"os/exec"
	"strings"
	"testing"
)

// A host imports this package to resolve model windows without linking the proxy (issue #1251).
// If models or models/litellm ever links the proxy, that host links the whole proxy again. The
// check matches by prefix, so a subpackage such as proxyapp/metricspush is banned too.
func TestModelsLinksNoProxy(t *testing.T) {
	// The go on PATH: the toolchain running this test. runtime.GOROOT is deprecated and can be
	// empty under -trimpath or a moved toolchain.
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("go is not on PATH: %v", err)
	}
	out, err := exec.Command(goBin, "list", "-deps", ".", "./litellm").CombinedOutput()
	if err != nil {
		// Fatal, not Skip: a guard that skips proves nothing.
		t.Fatalf("go list failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "github.com/rossoctl/context-guru/internal/modelinfo") {
		t.Fatalf("go list did not list internal/modelinfo; the guard would prove nothing:\n%s", out)
	}
	for _, pkg := range strings.Fields(string(out)) {
		for _, p := range []string{"proxy", "proxyapp", "tenant", "dash", "components/all"} {
			banned := "github.com/rossoctl/context-guru/" + p
			if pkg == banned || strings.HasPrefix(pkg, banned+"/") {
				t.Errorf("models links %s; a host that needs only the resolver would link the proxy", pkg)
			}
		}
	}
}
