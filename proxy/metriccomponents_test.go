package proxy

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	acmetrics "github.com/rossoctl/accountant/metrics"
	sdk "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rossoctl/context-guru/components"
	_ "github.com/rossoctl/context-guru/components/all"
)

// Every registered component is still on the list: the extra names are added, never swapped in.
func TestMetricComponentsHoldTheRegistry(t *testing.T) {
	got := MetricComponents()
	names := components.Names()
	if len(names) == 0 {
		t.Fatal("the registry is empty; the test imports components/all to fill it")
	}
	for _, n := range names {
		if !slices.Contains(got, n) {
			t.Errorf("registered component %q is not on the metrics allow-list", n)
		}
	}
	for _, n := range operatorsOutsideRegistry {
		if slices.Contains(names, n) {
			t.Errorf("%q is now a registered component; drop it from operatorsOutsideRegistry", n)
		}
	}
}

// The keeper's name comes from the library, so read it from PingOps rather than repeat it here.
func TestMetricComponentsHoldTheKeeper(t *testing.T) {
	ops, _ := accountant.PingOps(accountant.Ping{Session: "s", ThreadID: "main", Model: "m", At: time.Now()},
		accountant.Billing{Mode: accountant.BillingAPI})
	if len(ops.Ops) == 0 {
		t.Fatal("PingOps gave no operation")
	}
	for _, op := range ops.Ops {
		name := strings.TrimPrefix(op.Operator, "context-guru.")
		if !slices.Contains(MetricComponents(), name) {
			t.Errorf("the ping's operator %q is not on the metrics allow-list, so it is pushed as \"other\"", name)
		}
	}
}

// The sink built with MetricComponents labels each operator outside the registry with its own
// name, not "other". The central dashboard splits the net saving by these names.
func TestOperatorsOutsideRegistryKeepTheirComponentLabel(t *testing.T) {
	reader := sdk.NewManualReader()
	sink, err := NewMetricsSink(sdk.NewMeterProvider(sdk.WithReader(reader)).Meter("t"), nil, MetricComponents()...)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range operatorsOutsideRegistry {
		op := "context-guru." + name
		if name == "keeper" {
			op = name // the library's ping op has no host prefix
		}
		sink.Observe(acmetrics.Observation{
			Meta: accountant.RequestMeta{Model: "m", Billing: accountant.Billing{Mode: accountant.BillingAPI}},
			Result: accountant.Result{Ops: []accountant.PricedOp{{Kind: accountant.KindRewrite, Operator: op,
				Saved: accountant.TierTokens{Input: 1}, Caused: accountant.TierTokens{Input: 1}}}},
		})
	}
	labels := componentLabels(t, reader)
	for _, name := range operatorsOutsideRegistry {
		if !labels[name] {
			t.Errorf("operator %q has no component=%q series; labels seen: %v", name, name, labels)
		}
	}
	if labels["other"] {
		t.Errorf("an operator on the allow-list was pushed as component=\"other\": %v", labels)
	}
}

func componentLabels(t *testing.T, r *sdk.ManualReader) map[string]bool {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	add := func(v string, ok bool) {
		if ok {
			out[v] = true
		}
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Sum[float64]:
				for _, dp := range d.DataPoints {
					v, ok := dp.Attributes.Value("component")
					add(v.AsString(), ok)
				}
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					v, ok := dp.Attributes.Value("component")
					add(v.AsString(), ok)
				}
			}
		}
	}
	return out
}
