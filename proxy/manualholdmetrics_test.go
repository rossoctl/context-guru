package proxy

import (
	"net/http"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/keepalive"
)

// Issue #1103: /metrics shows the manual-hold measures. The counters and the histogram are
// process-wide. The gauge carries the tenant id and label, like cg_tenant_refused_requests_total.
func TestMetricsExportTheManualHoldMeasures(t *testing.T) {
	f := newHostedFixture(t, "up", "anthropic")
	tn, _ := f.register(t, "user@ibm.com")
	now := time.Now()
	hdr := http.Header{"Authorization": {"Bearer sk-caller"}, "Anthropic-Version": {"2023-06-01"}}
	f.h.keeper.Record(keepalive.Record{Key: keepalive.Key{Tenant: tn.ID, Session: "s1"}, StartedAt: now,
		Body: []byte(kaBody), Provider: bschemas.Anthropic, Route: "/v1/messages", Status: http.StatusOK,
		Usage: keepalive.Usage{CacheRead: 48576, CacheWrite: 1200}, UsageOK: true,
		Policy: keepalive.Policy{ManualHold: true}, Header: hdr})
	body := f.h.renderMetrics()
	for _, want := range []string{
		"# TYPE cg_keepalive_manual_hold_evicted_total counter",
		`cg_keepalive_manual_hold_evicted_total{reason="bytes"} 0`,
		`cg_keepalive_manual_hold_evicted_total{reason="count"} 0`,
		"# TYPE cg_keepalive_refused_total counter",
		`cg_keepalive_refused_total{reason="too_large"} 0`,
		"# TYPE cg_keepalive_manual_hold_body_bytes histogram",
		`cg_keepalive_manual_hold_body_bytes_bucket{le="1048576"} 1`,
		`cg_keepalive_manual_hold_body_bytes_bucket{le="33554432"} 1`,
		`cg_keepalive_manual_hold_body_bytes_bucket{le="+Inf"} 1`,
		"cg_keepalive_manual_hold_body_bytes_count 1",
		"# TYPE cg_tenant_keepalive_manual_hold_bytes gauge",
		`cg_tenant_keepalive_manual_hold_bytes{tenant="` + tn.ID + `",label="laptop"} `,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics is missing %q", want)
		}
	}
}
