package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	acmetrics "github.com/rossoctl/accountant/metrics"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	sdk "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type onePrice struct{}

func (onePrice) Price(context.Context, string) (modelinfo.Price, bool) {
	return modelinfo.Price{Input: 1e-6, Output: 5e-6, CacheRead: 1e-7, CacheWrite: 1.25e-6}, true
}

func observation() acmetrics.Observation {
	return acmetrics.Observation{
		Meta: accountant.RequestMeta{Model: "m", At: time.Now(), Billing: accountant.Billing{Mode: accountant.BillingAPI}},
		Result: accountant.Result{Ops: []accountant.PricedOp{{Kind: accountant.KindRewrite, Operator: "context-guru.mask",
			Saved: accountant.TierTokens{Input: 100}}}},
	}
}

func settings(t *testing.T, url string, off bool) metricsPushSettings {
	return metricsPushSettings{dashboard: true, dashboardDB: filepath.Join(t.TempDir(), "d.db"),
		url: url, token: "tok", interval: 20 * time.Millisecond, off: off}
}

// The sink reaches the collector with the token, and Stop flushes.
func TestStartMetricsPushReachesTheCollector(t *testing.T) {
	var hits atomic.Int32
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		auth.Store(r.Header.Get("Authorization"))
	}))
	defer srv.Close()

	got, stop := startMetricsPush(settings(t, srv.URL, false), onePrice{}, "codesafe")
	sink := got.savings
	if sink == nil || got.inference == nil {
		t.Fatal("no sink")
	}
	sink.Observe(observation())
	stop()
	if hits.Load() == 0 {
		t.Fatal("the collector got no push")
	}
	if auth.Load() != "Bearer tok" {
		t.Errorf("Authorization = %v, want Bearer tok", auth.Load())
	}
}

// Off makes no connection, and the proxy still gets a safe (nil or no-op) sink.
func TestStartMetricsPushOffMakesNoConnection(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	got, stop := startMetricsPush(settings(t, srv.URL, true), onePrice{}, "codesafe")
	if got.savings != nil {
		got.savings.Observe(observation())
	}
	stop()
	if hits.Load() != 0 {
		t.Fatalf("a disabled push made %d connection(s)", hits.Load())
	}
}

// No dashboard means no accountant, so no sink and no connection.
func TestStartMetricsPushWithoutDashboardIsOff(t *testing.T) {
	s := settings(t, "http://127.0.0.1:1", false)
	s.dashboard = false
	if got, _ := startMetricsPush(s, onePrice{}, "x"); got.savings != nil || got.inference != nil {
		t.Fatal("a run with no dashboard built a sink")
	}
}

// A bad URL must not stop the proxy: no sink, one log line (R20).
func TestStartMetricsPushBadURLFailsOpen(t *testing.T) {
	got, stop := startMetricsPush(settings(t, "http://collector.example.com", false), onePrice{}, "x")
	defer stop()
	if got.savings != nil {
		t.Fatal("a plain http URL to a remote host was accepted")
	}
}

// The install ID is kept beside the database and does not change between runs.
func TestMachineIDPath(t *testing.T) {
	for db, want := range map[string]string{
		"/var/cg/dash.db":       "/var/cg/machine-id",
		":memory:":              "",
		"":                      "",
		"file::memory:?cache=1": "",
	} {
		if got := machineIDPath(db); got != want {
			t.Errorf("machineIDPath(%q) = %q, want %q", db, got, want)
		}
	}
}

// No default URL: with none set, the proxy warns, reports nothing, and writes no machine ID.
func TestStartMetricsPushWithoutURLReportsNothing(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	s := settings(t, "", false)
	got, stop := startMetricsPush(s, onePrice{}, "x")
	defer stop()
	if got.savings != nil || got.inference != nil {
		t.Fatal("a sink was built with no collector URL")
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "no collector URL") {
		t.Fatalf("want one warning that names the missing URL, got: %q", logs.String())
	}
	if _, err := os.Stat(machineIDPath(s.dashboardDB)); err == nil {
		t.Fatal("a machine ID was written although nothing is reported")
	}
}

func TestStartMetricsPushWhitespaceURLIsNoURL(t *testing.T) {
	if got, _ := startMetricsPush(settings(t, "   ", false), onePrice{}, "x"); got.savings != nil {
		t.Fatal("a blank URL built a sink")
	}
}

// llm_proxy_info is always 1 and carries the version and the preset.
func TestProxyInfoGauge(t *testing.T) {
	r := sdk.NewManualReader()
	m := sdk.NewMeterProvider(sdk.WithReader(r)).Meter("t")
	if err := registerProxyInfo(m, "v9.9", "codesafe"); err != nil {
		t.Fatal(err)
	}
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	g, ok := rm.ScopeMetrics[0].Metrics[0].Data.(metricdata.Gauge[int64])
	if !ok || len(g.DataPoints) != 1 || g.DataPoints[0].Value != 1 {
		t.Fatalf("gauge = %+v, want one point of value 1", rm.ScopeMetrics[0].Metrics[0].Data)
	}
	v, _ := g.DataPoints[0].Attributes.Value("version")
	p, _ := g.DataPoints[0].Attributes.Value("preset")
	if v.AsString() != "v9.9" || p.AsString() != "codesafe" {
		t.Errorf("labels = %v, %v", v.AsString(), p.AsString())
	}
}
