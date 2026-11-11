//go:build !cg_record

package proxyapp

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The default build must not link the recorder's dependencies (+11.5 MB). A file that pulls
// them in must carry //go:build cg_record.
func TestDefaultBuildLinksNoOpenTelemetryGRPCOrProtobuf(t *testing.T) {
	goBin := filepath.Join(runtime.GOROOT(), "bin", "go")
	out, err := exec.Command(goBin, "list", "-deps", ".").CombinedOutput()
	if err != nil {
		// Fatal, not Skip: a guard that skips proves nothing.
		t.Fatalf("go list failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "github.com/rossoctl/context-guru/proxy") {
		t.Fatalf("go list did not list the proxy package; the guard would prove nothing:\n%s", out)
	}
	for _, pkg := range strings.Fields(string(out)) {
		for _, bad := range []string{"go.opentelemetry.io/", "google.golang.org/grpc", "google.golang.org/protobuf"} {
			if strings.HasPrefix(pkg, bad) {
				t.Errorf("default build links %s; put the file that imports it behind //go:build cg_record", pkg)
			}
		}
	}
}
