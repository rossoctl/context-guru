package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/metrics"
)

// stubTenantMetrics is one tenant with one of everything, so every per-tenant family is
// emitted. The VALUES are irrelevant here; only the metric names are under test.
type stubTenantMetrics struct{}

func (stubTenantMetrics) TenantMetrics(int64) ([]TenantMetricRow, error) {
	return []TenantMetricRow{{TenantID: "t1", Label: "t1", Requests: 1}}, nil
}

// A Grafana panel querying a metric this build does not export renders EMPTY — same as a
// healthy-but-idle system. Nothing catches that: the dashboards are provisioned from JSON
// and the exporter is hand-written, so the two drift silently and the first symptom is an
// operator reading a blank panel as "no problem".
//
// So: every cg_* name any dashboard queries must appear in the exposition. The per-tenant
// families need a tenant to have traffic, so the HELP line is what is checked for those — it
// is emitted with the header regardless of whether any series follows.
//
// It checks the SERVED RESPONSE, not renderMetrics() alone, and the difference is load-bearing.
// Not every series lives in the cached body: cg_metrics_age_seconds and
// cg_metrics_render_seconds are appended per response, because the body is shared by every
// scrape served from one render and its age differs for each of them — baking an age into the
// cached body would make it report the same figure to every scrape, which is the exact lie
// those two series exist to expose. Asserting against renderMetrics() would therefore fail on
// series that a scraper does in fact receive. What Prometheus actually gets is the response, so
// that is what this asserts on, and it now covers both kinds.
func TestDashboardsOnlyQueryMetricsWeExport(t *testing.T) {
	// A recorder and a tenant-metrics source, because whole families of series are
	// emitted only when those are wired — which is also why this test needs to exist:
	// the dashboards query them unconditionally.
	rec, err := dash.NewRecorder(dash.Options{DBPath: ":memory:", BatchSize: 1,
		FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	h := New(nil, nil, metrics.NewAggregator(), Options{
		Dashboard: rec, TenantMetrics: stubTenantMetrics{},
	})
	// Loopback, because metricsAllowed gates the endpoint on it when no METRICS_TOKEN is set.
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	h.metricsHandler(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/metrics returned %d, want 200: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	files, err := filepath.Glob("../deploy/grafana/dashboards/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no dashboards found: %v", err)
	}
	// Families emitted only when the state they describe exists. Each is a deliberate
	// conditional in renderMetrics, and an absent series is the honest answer for them
	// (no traffic yet / no tenant registry), so they cannot be asserted from a bare
	// handler. Listed rather than skipped by pattern, so adding one is a decision.
	conditional := map[string]string{
		"cg_cache_hit_ratio":               "emitted once any request has reported cache or fresh input",
		"cg_tenant_disabled":               "needs a tenant registry (hosted mode)",
		"cg_tenant_refused_requests_total": "emitted once a tenant has been refused",
	}
	name := regexp.MustCompile(`\bcg_[a-z0-9_]+\b`)
	missing := map[string][]string{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		// Decode and re-encode so only the JSON VALUES are scanned: a metric named in a
		// panel description is prose, and prose is allowed to mention anything.
		var d struct {
			Panels []struct {
				Targets []struct {
					Expr string `json:"expr"`
				} `json:"targets"`
			} `json:"panels"`
		}
		if err := json.Unmarshal(b, &d); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		for _, p := range d.Panels {
			for _, tg := range p.Targets {
				for _, m := range name.FindAllString(tg.Expr, -1) {
					if _, ok := conditional[m]; ok {
						continue
					}
					if strings.Contains(body, "# HELP "+m+" ") || strings.Contains(body, "\n"+m+"{") ||
						strings.Contains(body, "\n"+m+" ") {
						continue
					}
					missing[filepath.Base(f)] = append(missing[filepath.Base(f)], m)
				}
			}
		}
	}
	for f, ms := range missing {
		sort.Strings(ms)
		t.Errorf("%s queries metrics this build does not export, so those panels render empty: %v",
			f, uniq(ms))
	}
}

func uniq(in []string) []string {
	out := in[:0]
	var last string
	for _, s := range in {
		if s != last {
			out = append(out, s)
		}
		last = s
	}
	return out
}
