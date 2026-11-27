package agent

import (
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/internal/testutil"
)

const module = "github.com/rossoctl/context-guru"

// agent and every agent package under it (agent/bob, agent/claudecode, agent/codex, agent/opencode, agent/pi) are library packages: a host (the proxy,
// the bifrost adapter) imports them, never the reverse. This test enforces that rule. It reads
// the full dependency list of ./..., so an indirect import, or a new agent package, is covered.
func TestAgentDoesNotImportAHost(t *testing.T) {
	out := testutil.GoListDeps(t, "./...")
	deps := strings.Fields(out)
	for _, want := range []string{module + "/agent", module + "/agent/bob", module + "/agent/codex", module + "/agent/claudecode", module + "/agent/opencode", module + "/agent/pi"} {
		if !contains(deps, want) {
			t.Fatalf("go list did not list %s; the guard would prove nothing:\n%s", want, out)
		}
	}
	for _, pkg := range deps {
		if isHost(pkg) {
			t.Errorf("an agent package depends on host package %s; move the logic into a library package", pkg)
		}
	}
}

func TestIsHostMatchesEveryHostPackage(t *testing.T) {
	for _, pkg := range []string{module + "/proxy", module + "/proxy/sub", module + "/proxyapp",
		module + "/cmd/context-guru-proxy", module + "/adapters", module + "/adapters/bifrost"} {
		if !isHost(pkg) {
			t.Errorf("isHost(%q) = false, want true", pkg)
		}
	}
	for _, pkg := range []string{module + "/agent", module + "/apply", module + "/proxyish",
		module + "/adaptersish", "net/http"} {
		if isHost(pkg) {
			t.Errorf("isHost(%q) = true, want false", pkg)
		}
	}
}

func isHost(pkg string) bool {
	for _, host := range []string{"/proxy", "/proxyapp", "/cmd", "/adapters"} {
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
