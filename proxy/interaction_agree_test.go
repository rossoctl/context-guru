package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	acmetrics "github.com/rossoctl/accountant/metrics"
	"github.com/rossoctl/context-guru/dash"
	sdk "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// interactionRes is the accountant's exact interaction case (design §6.2): a ping kept a 1255-token
// system prompt warm, a mask cut 1201 tokens from messages[0], and the provider read 1255 and wrote
// 13. The rescue books 1255 write -> read, the mask 1201 at the read tier, and the interaction is
// the other 1201 write -> read, which no operation owns.
func interactionRes() accountant.Result {
	return accountant.Result{
		Ops: []accountant.PricedOp{{Kind: accountant.KindRewrite, Operator: "context-guru.mask", OpID: "op1",
			ChangeID: "mask:1", Saved: accountant.TierTokens{CacheRead: 1201}}},
		CacheEffects: []accountant.CacheEffect{{ID: "ce:1", Kind: accountant.EffectRescue, ChangeID: "ka:main:1",
			Saved: accountant.TierTokens{CacheWrite5m: 1255}, Caused: accountant.TierTokens{CacheRead: 1255}}},
		Interaction:       accountant.TierTokens{CacheWrite5m: 1201},
		InteractionCaused: accountant.TierTokens{CacheRead: 1201},
	}
}

// The dashboard books the interaction the metrics book, to the micro (it ignored it before). The
// metrics are llm.savings.interaction.usd, saved minus added.
func TestDashboardBooksTheInteractionAsTheMetricsDo(t *testing.T) {
	const model = "claude-sonnet-4-5"
	res := interactionRes()
	usage := accountant.Usage{CacheRead: 1255, CacheWrite: 13}

	r := sdk.NewManualReader()
	s, err := acmetrics.New(sdk.NewMeterProvider(sdk.WithReader(r)).Meter("test"), metricsPricer{prices: sizedTierPricer{}})
	if err != nil {
		t.Fatal(err)
	}
	s.Observe(acmetrics.Observation{Meta: accountant.RequestMeta{Model: model, At: time.Unix(1700000000, 0),
		Billing: accountant.Billing{Mode: accountant.BillingAPI}}, Usage: usage, Result: res})
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var metric float64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if d, ok := m.Data.(metricdata.Sum[float64]); ok && m.Name == "llm.savings.interaction.usd" {
				for _, dp := range d.DataPoints {
					if dir, _ := dp.Attributes.Value("direction"); dir.AsString() == "added" {
						metric -= dp.Value
					} else {
						metric += dp.Value
					}
				}
			}
		}
	}
	if metric <= 0 {
		t.Fatalf("setup: the metrics booked interaction %v, want a saving", metric)
	}

	p, _ := sizedTierPricer{}.Price(context.Background(), model)
	e := &dash.Event{CacheRead: int64(usage.CacheRead), CacheWrite: int64(usage.CacheWrite)}
	e.SetAccountant(res)
	if got := e.AccountantFigure(p).InteractionUSD; !near(got, metric) {
		t.Errorf("dashboard InteractionUSD = %.9f, metrics interaction.usd = %.9f: they must agree", got, metric)
	}
}
