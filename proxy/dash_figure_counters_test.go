package proxy

import (
	"context"
	"math"
	"strings"
	"testing"

	sdk "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// netUSD is the net of the dollar counters of the savings sink: saved minus added, the cache effects
// and the interaction, each by direction (the sink's own rule: add the three for the net saving).
func netUSD(t *testing.T, r *sdk.ManualReader) float64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var net float64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			d, ok := m.Data.(metricdata.Sum[float64])
			if !ok || !strings.HasPrefix(m.Name, "llm.savings.") || !strings.HasSuffix(m.Name, ".usd") {
				continue
			}
			for _, dp := range d.DataPoints {
				sign := 1.0
				if m.Name == "llm.savings.added.usd" {
					sign = -1
				}
				if v, ok := dp.Attributes.Value("direction"); ok && v.AsString() == "added" {
					sign = -1
				}
				net += sign * dp.Value
			}
		}
	}
	return net
}

// Issue #1440: the dashboard's saving of a request and the llm_savings_* counters of the same request
// are one figure. Both come from accountant.Figure, so they cannot drift. The dashboard figure is what
// the event stores: the baseline delta, the keep-alive rescue and the split saving. A request that the
// accountant could not rule a break out of (no history yet) holds its figure out of the booked columns as
// EstimatedSavedUSD (#915). The counters book it, so the test adds it back.
func TestDashboardSavingEqualsTheSavingsCounters(t *testing.T) {
	up := billedUpstream(t)
	defer up.Close()
	reader := sdk.NewManualReader()
	provider := sdk.NewMeterProvider(sdk.WithReader(reader))
	sink, err := NewMetricsSink(provider.Meter("test"), fixedPricer{})
	if err != nil {
		t.Fatal(err)
	}
	h, rec, evs := accHandlerOpts(t, up.URL, "preset: conservative\n", func(o *Options) { o.MetricsSink = sink })
	postTwice(t, h, "sess-equal")
	waitForRows(t, rec, 2)
	got := evs.waitAll(t, 2)

	var dashTotal float64
	priced := 0
	for _, e := range got {
		if !e.AccountantPriced {
			continue
		}
		priced++
		dashTotal += (e.BaselineCostUSD - e.CostUSD) + e.KeepAliveSavedUSD + e.CachesplitSavedUSD + e.EstimatedSavedUSD
	}
	if priced == 0 {
		t.Fatal("no request was priced by the accountant: the test proves nothing")
	}
	counters := netUSD(t, reader)
	if dashTotal == 0 || counters == 0 {
		t.Fatalf("a zero figure proves nothing: dashboard %v counters %v", dashTotal, counters)
	}
	if math.Abs(dashTotal-counters) > 1e-9 {
		t.Errorf("dashboard saving %v, llm_savings_* counters %v", dashTotal, counters)
	}
}
