package dash

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	acmetrics "github.com/rossoctl/accountant/metrics"
	sdk "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// sinkPricer is hostPricer as the sink wants it: the base rate, and the rate at a size.
type sinkPricer struct{ hostPricer }

func (s sinkPricer) Price(model string, at time.Time, t acmetrics.Tier) (float64, bool) {
	return s.PriceAt(model, at, t, 0)
}

// Issue #1440: across a price line, with operations that save and add, a rescue and an owned break, the
// dashboard's per-component figures and the llm_savings_* counters of the same request are one number.
func TestAccountantFigureEqualsTheSavingsCountersAcrossALine(t *testing.T) {
	p := gpt56()
	e := &Event{Model: "gpt-5.6", FreshInput: 100_000, CacheRead: 150_000, CacheWrite: 20_000, OutputTokens: 2_000}
	res := accountant.Result{
		Ops: []accountant.PricedOp{
			{Operator: "context-guru.mask", ChangeID: "c1", Saved: accountant.TierTokens{Input: 50_000}},
			{Operator: "context-guru.prune", ChangeID: "c2", Saved: accountant.TierTokens{CacheRead: 5_000}, Caused: accountant.TierTokens{Input: 1_000}},
		},
		CacheEffects: []accountant.CacheEffect{
			{Kind: accountant.EffectBreak, ChangeID: "c1", Saved: accountant.TierTokens{CacheRead: 20_000}, Caused: accountant.TierTokens{CacheWrite5m: 20_000}},
			{Kind: accountant.EffectRescue, ChangeID: "ka:1", Saved: accountant.TierTokens{CacheWrite5m: 1_000}, Caused: accountant.TierTokens{CacheRead: 1_000}},
		},
		Interaction:       accountant.TierTokens{CacheWrite5m: 300},
		InteractionCaused: accountant.TierTokens{CacheRead: 300},
	}
	e.SetAccountant(res)
	f := e.AccountantFigure(p)

	r := sdk.NewManualReader()
	sink, err := acmetrics.New(sdk.NewMeterProvider(sdk.WithReader(r)).Meter("t"), sinkPricer{hostPricer{p}})
	if err != nil {
		t.Fatal(err)
	}
	sink.Observe(acmetrics.Observation{
		Meta:   accountant.RequestMeta{Model: "gpt-5.6", Billing: accountant.Billing{Mode: accountant.BillingAPI}},
		Usage:  accountant.Usage{Input: 100_000, CacheRead: 150_000, CacheWrite: 20_000},
		Output: 2_000, Result: res,
	})
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var counters float64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			d, ok := m.Data.(metricdata.Sum[float64])
			if !ok {
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
				counters += sign * dp.Value
			}
		}
	}
	dashboard := f.DeltaUSD + f.KeepAliveSavedUSD + f.SplitUSD + f.TTLUSD
	if dashboard == 0 || math.Abs(dashboard-counters) > 1e-9 {
		t.Fatalf("dashboard %v, counters %v", dashboard, counters)
	}
	var rows float64
	for _, v := range f.ByComponent {
		rows += v
	}
	if math.Abs(rows+f.EndedBreakUSD+f.InteractionUSD-f.DeltaUSD) > 1e-9 {
		t.Fatalf("rows %v + ended %v + interaction %v do not add up to delta %v", rows, f.EndedBreakUSD, f.InteractionUSD, f.DeltaUSD)
	}
}

// The two split columns map to the right figure: the write and input tiers are the first removal, the
// read tier is the replay. The re-rate share of a net saver sits in the first column (accountant.Figure).
func TestAccountantFigureMapsFirstAndReplayColumns(t *testing.T) {
	e := &Event{FreshInput: 60_000, CacheRead: 40_000}
	e.SetAccountant(accountant.Result{Ops: []accountant.PricedOp{{Operator: "context-guru.mask",
		Saved: accountant.TierTokens{Input: 3_000, CacheRead: 7_000}}}})
	f := e.AccountantFigure(accTestPrice)
	if want := tierUSD(accTestPrice, accountant.TierTokens{Input: 3_000}); !accNear(f.ByComponentFirst["mask"], want) || want == 0 {
		t.Errorf("first %v want %v", f.ByComponentFirst["mask"], want)
	}
	if want := tierUSD(accTestPrice, accountant.TierTokens{CacheRead: 7_000}); !accNear(f.ByComponentReplay["mask"], want) || want == 0 {
		t.Errorf("replay %v want %v", f.ByComponentReplay["mask"], want)
	}
}
