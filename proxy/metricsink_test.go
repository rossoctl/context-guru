package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	acmetrics "github.com/rossoctl/accountant/metrics"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"go.opentelemetry.io/otel/attribute"
	sdk "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func usageUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant",
		  "content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",
		  "usage":{"input_tokens":20,"output_tokens":34,
		           "cache_read_input_tokens":0,"cache_creation_input_tokens":50}}`)
	}))
	t.Cleanup(up.Close)
	return up
}

// counted wraps a sink and counts the requests it saw.
type counted struct {
	acmetrics.Observer
	n atomic.Int32
}

func (c *counted) Observe(o acmetrics.Observation) { c.n.Add(1); c.Observer.Observe(o) }

func metricNames(t *testing.T, r *sdk.ManualReader) map[string]bool {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = true
		}
	}
	return out
}

// The sink sees each priced request once, and the savings counters appear.
func TestMetricsSinkSeesEachPricedRequest(t *testing.T) {
	up := usageUpstream(t)
	reader := sdk.NewManualReader()
	provider := sdk.NewMeterProvider(sdk.WithReader(reader))
	inner, err := NewMetricsSink(provider.Meter("test"), fixedPricer{})
	if err != nil {
		t.Fatal(err)
	}
	sink := &counted{Observer: inner}
	h, rec, _ := accHandlerOpts(t, up.URL, "preset: codesafe\n", func(o *Options) { o.MetricsSink = sink })
	accPost(t, h, "sess-metrics")
	accPost(t, h, "sess-metrics")
	waitForRows(t, rec, 2)

	if got := sink.n.Load(); got != 2 {
		t.Fatalf("the sink saw %d requests, want 2", got)
	}
	if !metricNames(t, reader)["llm.savings.requests"] {
		t.Fatalf("no llm.savings.requests counter: %v", metricNames(t, reader))
	}
}

// No sink, no change: the default must keep working.
func TestNoMetricsSinkIsFine(t *testing.T) {
	up := usageUpstream(t)
	h, rec, _ := accHandler(t, up.URL)
	accPost(t, h, "sess-nosink")
	waitForRows(t, rec, 1)
}

type panicSink struct{}

func (panicSink) Observe(acmetrics.Observation) { panic("boom") }

// A panic in the sink costs the metric only. The request is answered and recorded (R46, R20).
func TestPanickingMetricsSinkDoesNotHurtTheRequest(t *testing.T) {
	up := usageUpstream(t)
	h, rec, tap := accHandlerOpts(t, up.URL, "preset: codesafe\n", func(o *Options) { o.MetricsSink = panicSink{} })
	accPost(t, h, "sess-panic")
	accPost(t, h, "sess-panic")
	waitForRows(t, rec, 2)
	if !tap.all()[1].HasAccountant() {
		t.Fatal("the panic in the sink cost the request its accountant result")
	}
}

// A slow sink is the only way a metric could delay a request, so say it plainly: the pricer must not
// block. This pins that the pricer adapter returns at once for an unknown model.
func TestPricerAdapterNeverBlocks(t *testing.T) {
	p := metricsPricer{prices: fixedPricer{}}
	done := make(chan struct{})
	go func() {
		p.Price("no-such-model", time.Now(), acmetrics.TierInput)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Price blocked")
	}
}

// #1248: the Pricer contract says a fallback rate is marked estimated. A cache-read rate filled
// from a default multiple, for a model that is not Claude, is one. The write tiers follow a rule
// per family, so they are never marked.
func TestPricerMarksFallbackTiersEstimated(t *testing.T) {
	p := metricsPricer{prices: partialPricer{}}
	for _, tc := range []struct {
		model string
		tier  acmetrics.Tier
		want  bool
	}{
		{"gemini-3-flash-preview", acmetrics.TierInput, false},
		{"gemini-3-flash-preview", acmetrics.TierCacheRead, true},
		{"gemini-3-flash-preview", acmetrics.TierCacheWrite5m, false},
		{"gemini-3-flash-preview", acmetrics.TierCacheWrite1h, false},
		{"claude-sonnet-5-5", acmetrics.TierCacheRead, false},
		{"claude-sonnet-5-5", acmetrics.TierCacheWrite1h, false},
	} {
		if _, est := p.Price(tc.model, time.Now(), tc.tier); est != tc.want {
			t.Errorf("%s tier %v: estimated = %v, want %v", tc.model, tc.tier, est, tc.want)
		}
	}
}

// partialPricer states only the input and output rates, as a price feed that omits cache rates does.
type partialPricer struct{}

func (partialPricer) Price(_ context.Context, model string) (modelinfo.Price, bool) {
	return modelinfo.Price{Input: 2e-06, Output: 1e-05}.Resolved(model), true
}

// readPricer states the input, output and cache-read rates, and no write rate, as LiteLLM does for
// most models that are not Claude.
type readPricer struct{}

func (readPricer) Price(_ context.Context, model string) (modelinfo.Price, bool) {
	return modelinfo.Price{Input: 2e-06, Output: 1e-05, CacheRead: 2e-07}.Resolved(model), true
}

// #1248 round 2: the sink asks the rate of every tier, also a tier with 0 tokens. A request that
// uses only stated rates and family rules must not be counted in llm.savings.estimated.
func TestSinkCountsAFallbackRateOnlyWhenOneIsUsed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prices modelinfo.Pricer
		want   int64
	}{
		{"stated read rate, no write rate, no 1h tokens", readPricer{}, 0},
		{"read rate filled from a default multiple", partialPricer{}, 1},
	} {
		reader := sdk.NewManualReader()
		sink, err := NewMetricsSink(sdk.NewMeterProvider(sdk.WithReader(reader)).Meter("test"), tc.prices)
		if err != nil {
			t.Fatal(err)
		}
		sink.Observe(acmetrics.Observation{
			Meta: accountant.RequestMeta{Model: "gpt-4o", At: time.Now(), Billing: accountant.Billing{Mode: accountant.BillingAPI}},
			Result: accountant.Result{CacheEffects: []accountant.CacheEffect{{Kind: accountant.EffectRescue,
				Saved: accountant.TierTokens{CacheWrite5m: 50}, Caused: accountant.TierTokens{CacheRead: 50}}}}})
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatal(err)
		}
		var got int64
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if s, ok := m.Data.(metricdata.Sum[int64]); ok && m.Name == "llm.savings.estimated" {
					for _, dp := range s.DataPoints {
						got += dp.Value
					}
				}
			}
		}
		if got != tc.want {
			t.Errorf("%s: llm.savings.estimated = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func attrKey(k string) attribute.Key { return attribute.Key(k) }

// A real request through the handler books into the inference set, with the tokens the upstream stated.
func TestInferenceMetricsSeeARealRequest(t *testing.T) {
	up := usageUpstream(t)
	reader := sdk.NewManualReader()
	im, err := NewInferenceMetrics(sdk.NewMeterProvider(sdk.WithReader(reader)).Meter("test"), nil)
	if err != nil {
		t.Fatal(err)
	}
	h, rec, _ := accHandlerOpts(t, up.URL, "preset: codesafe\n", func(o *Options) { o.InferenceMetrics = im })
	accPost(t, h, "sess-inference")
	waitForRows(t, rec, 1)

	all := collectAll(t, reader)
	reqs := intByLabel(all["llm.inference.requests"], "status_class")
	if reqs["2xx"] != 1 {
		t.Fatalf("requests by class = %v, want one 2xx", reqs)
	}
	tok := intByLabel(all["llm.inference.tokens"], "tier")
	if tok["output"] != 34 || tok["cache_write_5m"] != 50 {
		t.Errorf("tokens by tier = %v, want output 34 and cache_write_5m 50 (from the upstream's usage)", tok)
	}
	if _, ok := all["llm.inference.duration"]; !ok {
		t.Error("no duration was recorded")
	}
}
