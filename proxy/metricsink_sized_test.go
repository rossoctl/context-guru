package proxy

import (
	"context"
	"github.com/rossoctl/context-guru/internal/testutil"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	acmetrics "github.com/rossoctl/accountant/metrics"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	sdk "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// sizedTierPricer prices every model at 1 USD per million input and 4 per million output, with the
// built-in tier of the model (gpt-5.6: above 272,000 prompt tokens, 2x input and 1.5x output).
type sizedTierPricer struct{}

func (sizedTierPricer) Price(_ context.Context, model string) (modelinfo.Price, bool) {
	return modelinfo.Price{Input: 1e-6, Output: 4e-6, CacheRead: 1e-7, CacheWrite: 1.25e-6}.WithTiers(model), true
}

// sizedOnly hides PriceAt: the pricer as it was before this change.
type sizedOnly struct{ p acmetrics.Pricer }

func (h sizedOnly) Price(m string, at time.Time, t acmetrics.Tier) (float64, bool) {
	return h.p.Price(m, at, t)
}

func sinkSums(t *testing.T, p acmetrics.Pricer, o acmetrics.Observation) (saved, added float64) {
	t.Helper()
	r := sdk.NewManualReader()
	s, err := acmetrics.New(sdk.NewMeterProvider(sdk.WithReader(r)).Meter("test"), p)
	if err != nil {
		t.Fatal(err)
	}
	s.Observe(o)
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			d, ok := m.Data.(metricdata.Sum[float64])
			if !ok {
				continue
			}
			for _, dp := range d.DataPoints {
				switch m.Name {
				case "llm.savings.saved.usd":
					saved += dp.Value
				case "llm.savings.added.usd":
					added += dp.Value
				}
			}
		}
	}
	return saved, added
}

func trimObs(model string, billed, output, saved, caused int) acmetrics.Observation {
	return acmetrics.Observation{
		Meta: accountant.RequestMeta{Model: model, At: time.Unix(1700000000, 0),
			Billing: accountant.Billing{Mode: accountant.BillingAPI}},
		Usage:  accountant.Usage{Input: billed},
		Output: output,
		Result: accountant.Result{Ops: []accountant.PricedOp{{Kind: accountant.KindRewrite, Operator: "x.trim",
			Saved: accountant.TierTokens{Input: saved}, Caused: accountant.TierTokens{Input: caused}}}},
	}
}

// 290K without context-guru, 250K with it, on gpt-5.6: the counters book the whole bill difference,
// the output included. 290,000 in at 2e-6 and 1,000 out at 6e-6, minus 250,000 in at 1e-6 and 1,000
// out at 4e-6.
func TestMetricsSinkBooksTheFullBillDifferenceAcrossALine(t *testing.T) {
	p := modelinfo.ChainRates(sizedTierPricer{})
	saved, added := sinkSums(t, p, trimObs("gpt-5.6", 250_000, 1_000, 50_000, 10_000))
	want := (290_000*2e-6 + 1_000*6e-6) - (250_000*1e-6 + 1_000*4e-6)
	if !near(saved-added, want) {
		t.Errorf("net = %v, want the bill difference %v", saved-added, want)
	}
	if !near(added, 10_000*1e-6) {
		t.Errorf("added = %v, want 0.01 (caused tokens at the billed size, 1x)", added)
	}
}

// Without the output, the output re-rate (1,000 tokens x (6e-6 - 4e-6)) is lost.
func TestMetricsSinkBooksTheOutputReRate(t *testing.T) {
	p := modelinfo.ChainRates(sizedTierPricer{})
	with, _ := sinkSums(t, p, trimObs("gpt-5.6", 250_000, 1_000, 50_000, 10_000))
	without, _ := sinkSums(t, p, trimObs("gpt-5.6", 250_000, 0, 50_000, 10_000))
	if !near(with-without, 1_000*(6e-6-4e-6)) {
		t.Errorf("output re-rate = %v, want %v", with-without, 1_000*(6e-6-4e-6))
	}
}

// A request that stays below the line books what it booked before: PriceAt gives the base rate.
func TestMetricsSinkBelowTheLineIsUnchanged(t *testing.T) {
	o := trimObs("gpt-5.6", 100_000, 1_000, 20_000, 5_000)
	sv, ad := sinkSums(t, modelinfo.ChainRates(sizedTierPricer{}), o)
	ov, oa := sinkSums(t, sizedOnly{modelinfo.ChainRates(sizedTierPricer{})}, o)
	if !near(sv, ov) || !near(ad, oa) {
		t.Errorf("sized = (%v, %v), base = (%v, %v)", sv, ad, ov, oa)
	}
	if !near(sv, 20_000*1e-6) {
		t.Errorf("saved = %v, want 0.02", sv)
	}
}

func TestMetricsPricerPriceAt(t *testing.T) {
	p := modelinfo.ChainRates(sizedTierPricer{})
	at := time.Unix(1700000000, 0)
	for _, tc := range []struct {
		model string
		n     int64
		tier  acmetrics.Tier
		want  float64
	}{
		{"gpt-5.6", 272_000, acmetrics.TierInput, 1e-6}, // the line is exclusive
		{"gpt-5.6", 272_001, acmetrics.TierInput, 2e-6},
		{"gpt-5.6", 272_001, acmetrics.TierOutput, 6e-6},
		{"gpt-5.6", 272_001, acmetrics.TierCacheRead, 2e-7},
		{"claude-sonnet-5-5", 900_000, acmetrics.TierInput, 1e-6}, // flat model
		{"claude-haiku-5-5", 100_001, acmetrics.TierInput, 5e-6},
	} {
		got, est := p.PriceAt(tc.model, at, tc.tier, tc.n)
		if !near(got, tc.want) || est {
			t.Errorf("%s n=%d %v: got (%v, %v), want (%v, false)", tc.model, tc.n, tc.tier, got, est, tc.want)
		}
		if base, _ := p.Price(tc.model, at, tc.tier); tc.n <= 272_000 && tc.model != "claude-haiku-5-5" && !near(base, got) {
			t.Errorf("%s: Price %v differs from PriceAt below the line %v", tc.model, base, got)
		}
	}
	// Fail open: an unknown model is no money and estimated, as Price says.
	if v, est := (modelinfo.ChainRates(unknownPricer{})).PriceAt("x", at, acmetrics.TierInput, 300_000); v != 0 || !est {
		t.Errorf("unknown model: got (%v, %v), want (0, true)", v, est)
	}
	if v, est := (modelinfo.ChainRates(nil)).PriceAt("gpt-5.6", at, acmetrics.TierInput, 300_000); v != 0 || !est {
		t.Errorf("nil prices: got (%v, %v), want (0, true)", v, est)
	}
}

type unknownPricer struct{}

func (unknownPricer) Price(context.Context, string) (modelinfo.Price, bool) {
	return modelinfo.Price{}, false
}

type recordingSink struct {
	mu  sync.Mutex
	obs []acmetrics.Observation
}

func (r *recordingSink) Observe(o acmetrics.Observation) {
	r.mu.Lock()
	r.obs = append(r.obs, o)
	r.mu.Unlock()
}

// The host fills Observation.Output with the billed output tokens (the upstream answers with 34).
func TestObservationCarriesTheOutputTokens(t *testing.T) {
	up := usageUpstream(t)
	rs := &recordingSink{}
	h, rec, _ := accHandlerOpts(t, up.URL, "preset: conservative\n", func(o *Options) { o.MetricsSink = rs })
	accPost(t, h, "sess-output")
	waitForRows(t, rec, 1)
	testutil.WaitFor(t, "the sink sees a request", func() bool {
		rs.mu.Lock()
		defer rs.mu.Unlock()
		return len(rs.obs) > 0
	})
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for i, o := range rs.obs {
		if o.Output != 34 {
			t.Errorf("observation %d: Output = %d, want 34", i, o.Output)
		}
	}
}
