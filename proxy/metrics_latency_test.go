package proxy

import (
	"sort"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	acmetrics "github.com/rossoctl/accountant/metrics"
	sdk "go.opentelemetry.io/otel/sdk/metric"

	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// typicalObservation is a request with a handful of operations, a spend, and a cache effect: the
// shape of a busy codesafe request.
func typicalObservation() acmetrics.Observation {
	ops := []accountant.PricedOp{}
	for _, c := range []string{"mask", "dedup", "toon", "cachesplit", "tool-prune"} {
		ops = append(ops, accountant.PricedOp{Kind: accountant.KindRewrite, Operator: "context-guru." + c,
			Saved:  accountant.TierTokens{Input: 1200, CacheRead: 300},
			Caused: accountant.TierTokens{CacheWrite5m: 40}})
	}
	ops = append(ops, accountant.PricedOp{Kind: accountant.KindSpend, Operator: "context-guru.summarize",
		Call: &accountant.ModelCall{Model: "haiku", Usage: accountant.CallUsage{
			Usage: accountant.Usage{Input: 900, CacheRead: 100, CacheWrite: 200}, Output: 120}}})
	return acmetrics.Observation{
		Meta: accountant.RequestMeta{Model: "claude-sonnet-5-5", Billing: accountant.Billing{Mode: accountant.BillingAPI}},
		Result: accountant.Result{Ops: ops, CacheEffects: []accountant.CacheEffect{
			{Kind: accountant.EffectTTLPremium, Caused: accountant.TierTokens{CacheWrite1h: 500}},
			{Kind: accountant.EffectRescue, Saved: accountant.TierTokens{CacheRead: 8000}}}},
	}
}

func typicalEvent() *dash.Event {
	e := goodEvent()
	e.CacheMissReason, e.CacheMissCause = dash.CacheUnknown, dash.CauseToolsChanged
	for _, c := range []string{"mask", "dedup", "toon", "cachesplit", "tool-prune", "summarize"} {
		e.Components = append(e.Components, dash.CompRow{Component: c, DurationMs: 3})
	}
	return e
}

func pricedLike() modelinfo.Pricer {
	return ratePricer{"claude-sonnet-5-5": {Input: 3e-6, Output: 15e-6, CacheRead: 0.3e-6, CacheWrite: 3.75e-6}, "haiku": {Input: 1e-6, Output: 5e-6, CacheRead: 0.1e-6, CacheWrite: 1.25e-6}}
}

func BenchmarkSavingsObserve(b *testing.B) {
	sink, _ := NewMetricsSink(sdk.NewMeterProvider(sdk.WithReader(sdk.NewManualReader())).Meter("b"), pricedLike())
	o := typicalObservation()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sink.Observe(o)
	}
}

func BenchmarkInferenceObserve(b *testing.B) {
	im, _ := NewInferenceMetrics(sdk.NewMeterProvider(sdk.WithReader(sdk.NewManualReader())).Meter("b"), pricedLike())
	e := typicalEvent()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		im.Observe(e, time.Second)
	}
}

// percentiles measures n calls one by one and returns p50, p99 and max. A mean hides a slow tail.
func percentiles(n int, f func()) (p50, p99, max time.Duration) {
	d := make([]time.Duration, n)
	for i := range d {
		t := time.Now()
		f()
		d[i] = time.Since(t)
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return d[n/2], d[n*99/100], d[n-1]
}

// TestMetricsCallLatency reports the added time per request at p50 and p99. It logs, and it fails
// only above a generous ceiling that would mean something is blocking.
func TestMetricsCallLatency(t *testing.T) {
	sink, _ := NewMetricsSink(sdk.NewMeterProvider(sdk.WithReader(sdk.NewManualReader())).Meter("b"), pricedLike())
	im, _ := NewInferenceMetrics(sdk.NewMeterProvider(sdk.WithReader(sdk.NewManualReader())).Meter("b"), pricedLike())
	o, e := typicalObservation(), typicalEvent()
	for i := 0; i < 1000; i++ { // warm: the first calls create the data points
		sink.Observe(o)
		im.Observe(e, time.Second)
	}
	p50, p99, mx := percentiles(20000, func() { sink.Observe(o); im.Observe(e, time.Second) })
	t.Logf("savings + inference Observe, one request: p50=%v p99=%v max=%v", p50, p99, mx)
	if p99 > 5*time.Millisecond {
		t.Fatalf("p99 = %v: something in Observe blocks", p99)
	}
}
