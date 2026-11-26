//go:build !cg_record

package proxyapp

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The default build must not link the recorder's dependencies (+11.5 MB): gRPC, protobuf, and the
// OTLP exporters and generated protos that pull them in. A file that needs them must carry
// //go:build cg_record.
//
// The OpenTelemetry API and the metrics SDK are allowed. They are small (about 1.4 MB with the
// metrics push) and link neither gRPC nor protobuf. The push sends OTLP as JSON from
// proxyapp/metricspush for that reason: the standard OTLP exporter sends protobuf and would
// break this test. An earlier version banned the whole go.opentelemetry.io/ prefix, which also
// banned the API that the accountant module already imports.
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
		for _, bad := range []string{
			"google.golang.org/grpc", "google.golang.org/protobuf", "github.com/grpc-ecosystem/",
			"go.opentelemetry.io/proto/", "go.opentelemetry.io/otel/exporters/otlp/",
			"go.opentelemetry.io/otel/sdk/trace", // the recorder's tracing SDK
		} {
			if strings.HasPrefix(pkg, bad) {
				t.Errorf("default build links %s; put the file that imports it behind //go:build cg_record", pkg)
			}
		}
	}
}
