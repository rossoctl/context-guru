package metricspush_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	acmetrics "github.com/rossoctl/accountant/metrics"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/proxyapp/metricspush"
	"github.com/rossoctl/context-guru/proxy"
)

// TestCentralLoad is a LOAD GENERATOR, not a test: it asserts nothing. It pushes every metric set,
// again and again, for CENTRAL_LOAD_SECONDS, so the dashboards have something to show and a blank
// panel can be told from a wrong query. It does nothing unless CENTRAL_LOAD_SECONDS is set.
func TestCentralLoad(t *testing.T) {
	collector, token, _ := central(t)
	secs := 0
	fmt.Sscan(os.Getenv("CENTRAL_LOAD_SECONDS"), &secs)
	if secs == 0 {
		t.Skip("set CENTRAL_LOAD_SECONDS")
	}
	machine := fmt.Sprintf("load%028d", time.Now().UnixNano()%1e12)
	p, stop, err := metricspush.Start(context.Background(), metricspush.Config{URL: collector, Token: token,
		Interval: 5 * time.Second, Proxy: "context-guru", Machine: machine, Version: "load", Preset: "codesafe"})
	if err != nil {
		t.Fatal(err)
	}
	m := p.Meter("context-guru")
	sink, _ := acmetrics.New(m, flat{})
	inf, _ := proxy.NewInferenceMetrics(m, rates{}, "mask", "dedup")
	g, _ := m.Int64ObservableGauge("llm.proxy.info")
	_, _ = m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveInt64(g, 1, metric.WithAttributes(metricsAttr("version", "load"), metricsAttr("preset", "codesafe")))
		return nil
	}, g)
	end := time.Now().Add(time.Duration(secs) * time.Second)
	for i := 0; time.Now().Before(end); i++ {
		sink.Observe(acmetrics.Observation{
			Meta: accountant.RequestMeta{Model: "claude-sonnet-5-5", At: time.Now(), Billing: accountant.Billing{Mode: accountant.BillingAPI}},
			Result: accountant.Result{
				Unpriced: []accountant.Unpriced{{Reason: "penalty_unknown"}}, Checks: []accountant.CheckFailure{{Check: "sum"}},
				Ops: []accountant.PricedOp{
					{Kind: accountant.KindRewrite, Operator: "context-guru.mask", Saved: accountant.TierTokens{Input: 1000, CacheRead: 200}, Caused: accountant.TierTokens{CacheWrite5m: 40}, Estimated: i%5 == 0},
					{Kind: accountant.KindRewrite, Operator: "context-guru.dedup", Saved: accountant.TierTokens{Input: 10}, Caused: accountant.TierTokens{CacheWrite5m: 900}},
				},
				CacheEffects: []accountant.CacheEffect{
					{Kind: accountant.EffectBreak, Cause: accountant.CauseModelChanged, Caused: accountant.TierTokens{CacheWrite5m: 500}},
					{Kind: accountant.EffectRescue, Saved: accountant.TierTokens{CacheWrite5m: 300}, Caused: accountant.TierTokens{CacheRead: 300}},
				}}})
		e := &dash.Event{Model: "claude-sonnet-5-5", Provider: "anthropic", Status: 200, FreshInput: 10, CacheRead: 400, CacheWrite: 30,
			OutputTokens: 5, CostUSD: 0.01, UpstreamMs: 900, CGLatencyMs: 12, TTFBMs: 300}
		e.Stream = true
		if i%9 == 0 {
			e.Status = 529
		}
		if i%3 == 0 {
			e.CacheMissReason, e.CacheMissCause = dash.CacheUnknown, dash.CauseToolsChanged
		}
		e.Components = []dash.CompRow{{Component: "mask", DurationMs: 3}, {Component: "dedup", DurationMs: 1}}
		inf.Observe(e, 1200*time.Millisecond)
		ping := &dash.Event{Model: "claude-sonnet-5-5", Provider: "anthropic", Status: 200, KeepAlive: true, CostUSD: 0.001}
		inf.Observe(ping, 0)
		time.Sleep(500 * time.Millisecond)
	}
	_ = stop(context.Background())
}

func metricsAttr(k, v string) attribute.KeyValue { return attribute.String(k, v) }
