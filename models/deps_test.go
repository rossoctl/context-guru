package models

import (
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/internal/testutil"
)

// A host imports this package to resolve model windows without linking the proxy (issue #1251).
// If models or models/litellm ever links the proxy, that host links the whole proxy again. The
// check matches by prefix, so a subpackage such as proxyapp/metricspush is banned too.
func TestModelsLinksNoProxy(t *testing.T) {
	out := testutil.GoListDeps(t, ".", "./litellm")
	if !strings.Contains(out, "github.com/rossoctl/context-guru/internal/modelinfo") {
		t.Fatalf("go list did not list internal/modelinfo; the guard would prove nothing:\n%s", out)
	}
	for _, pkg := range strings.Fields(out) {
		for _, p := range []string{"proxy", "proxyapp", "tenant", "dash", "components/all"} {
			banned := "github.com/rossoctl/context-guru/" + p
			if pkg == banned || strings.HasPrefix(pkg, banned+"/") {
				t.Errorf("models links %s; a host that needs only the resolver would link the proxy", pkg)
			}
		}
	}
}
