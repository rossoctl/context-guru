package proxy

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	acmetrics "github.com/rossoctl/accountant/metrics"
	sdk "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rossoctl/context-guru/dash"
)

// seriesCount is how many time series Prometheus would store for these metrics: one for each data
// point of a sum or gauge, and for a histogram one for each bucket plus _sum, _count and the +Inf.
func seriesCount(t *testing.T, r *sdk.ManualReader) (total int, byMetric map[string]int) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	byMetric = map[string]int{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			n := 0
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				n = len(d.DataPoints)
			case metricdata.Sum[float64]:
				n = len(d.DataPoints)
			case metricdata.Gauge[int64]:
				n = len(d.DataPoints)
			case metricdata.Histogram[float64]:
				for _, dp := range d.DataPoints {
					n += len(dp.Bounds) + 1 + 2 // buckets, +Inf, _sum, _count
				}
			}
			byMetric[m.Name] = n
			total += n
		}
	}
	return total, byMetric
}

// A busy machine, savings counters included: 10 models, 3 providers, every component, every miss cause and many kinds of
// change. The spec's budget is about 1,700 series for a machine and under 2,000 (5.7).
func TestSeriesCountStaysInsideTheBudget(t *testing.T) {
	r := sdk.NewManualReader()
	p := sdk.NewMeterProvider(sdk.WithReader(r))
	comps := []string{"mask", "dedup", "toon", "extract_llm", "cachesplit", "tool-prune", "summarize", "sweep"}
	im, err := NewInferenceMetrics(p.Meter("t"), nil, comps...)
	if err != nil {
		t.Fatal(err)
	}
	sink, err := NewMetricsSink(p.Meter("t"), nil, comps...)
	if err != nil {
		t.Fatal(err)
	}
	causes := []struct{ reason, cause string }{
		{dash.CacheColdStart, ""}, {dash.CacheTTLExpiry, ""}, {dash.CacheUnknown, dash.CauseModelChanged},
		{dash.CacheUnknown, dash.CauseToolsChanged}, {dash.CacheUnknown, dash.CauseBypassFlip},
		{dash.CacheUnknown, dash.CauseCompactionAfterRewrite}, {dash.CacheUnknown, ""},
	}
	provs := []string{"anthropic", "openai", "bedrock"}
	for m := 0; m < 10; m++ {
		// A model name belongs to one provider, so the label sets do not multiply by the provider count.
		pr, prov := m%3, provs[m%3]
		{
			for _, st := range []int{200, 429, 500} {
				for _, stream := range []bool{true, false} {
					for ci, c := range causes {
						e := goodEvent()
						e.Model, e.Provider, e.Status = fmt.Sprintf("model-%d", m), prov, st
						e.Stream = stream
						e.CacheMissReason, e.CacheMissCause = c.reason, c.cause
						e.CacheWrite = 100 + int64(pr+ci)
						for _, comp := range comps {
							e.Components = append(e.Components, dash.CompRow{Component: comp, DurationMs: 1, Err: map[bool]string{true: "e"}[ci == 0]})
						}
						im.Observe(e, time.Second)
					}
				}
			}
		}
	}
	// The savings counters: every component, the three kinds and the four tiers an op can book.
	for _, comp := range comps {
		for _, kind := range []accountant.Kind{accountant.KindRewrite, accountant.KindSpend, accountant.KindSetting} {
			sink.Observe(acmetrics.Observation{
				Meta: accountant.RequestMeta{Model: "model-0", Billing: accountant.Billing{Mode: accountant.BillingAPI}},
				Result: accountant.Result{
					Ops: []accountant.PricedOp{{Kind: kind, Operator: "context-guru." + comp,
						Saved:  accountant.TierTokens{Input: 1, CacheRead: 1, CacheWrite5m: 1, CacheWrite1h: 1},
						Caused: accountant.TierTokens{Input: 1, CacheRead: 1, CacheWrite5m: 1, CacheWrite1h: 1}}},
				},
			})
		}
	}
	// And every cache effect, in both directions, with and without a cause.
	effects := []accountant.CacheEffectKind{accountant.EffectBreak, accountant.EffectRescue, accountant.EffectSplit,
		accountant.EffectTTLPremium, accountant.EffectTTLRescue, accountant.EffectMarkPremium}
	for _, k := range effects {
		sink.Observe(acmetrics.Observation{
			Meta: accountant.RequestMeta{Model: "model-0", Billing: accountant.Billing{Mode: accountant.BillingAPI}},
			Result: accountant.Result{CacheEffects: []accountant.CacheEffect{
				{Kind: k, Saved: accountant.TierTokens{CacheRead: 1}, Caused: accountant.TierTokens{CacheWrite5m: 1}},
				{Kind: k, Cause: accountant.CauseModelChanged, Caused: accountant.TierTokens{CacheWrite5m: 1}},
			}},
		})
	}
	total, by := seriesCount(t, r)
	t.Logf("series for the whole machine (savings, inference, cache, keep-alive, health): %d", total)
	for name, n := range by {
		t.Logf("  %-34s %5d", name, n)
	}
	if total > 1700 {
		t.Fatalf("%d series, over the 1,700 budget of the spec (5.7)", total)
	}
}
