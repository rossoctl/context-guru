package proxy

import (
	"context"
	"testing"
	"time"

	sdk "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rossoctl/context-guru/dash"
)

func newInference(t *testing.T) (*InferenceMetrics, *sdk.ManualReader) {
	t.Helper()
	r := sdk.NewManualReader()
	im, err := NewInferenceMetrics(sdk.NewMeterProvider(sdk.WithReader(r)).Meter("test"))
	if err != nil {
		t.Fatal(err)
	}
	return im, r
}

func collectAll(t *testing.T, r *sdk.ManualReader) map[string]metricdata.Aggregation {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]metricdata.Aggregation{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m.Data
		}
	}
	return out
}

func intByLabel(agg metricdata.Aggregation, key string) map[string]int64 {
	out := map[string]int64{}
	if s, ok := agg.(metricdata.Sum[int64]); ok {
		for _, dp := range s.DataPoints {
			v, _ := dp.Attributes.Value(attrKey(key))
			out[v.Emit()] += dp.Value
		}
	}
	return out
}

func sumFloat(agg metricdata.Aggregation) (n float64) {
	if s, ok := agg.(metricdata.Sum[float64]); ok {
		for _, dp := range s.DataPoints {
			n += dp.Value
		}
	}
	return n
}

func goodEvent() *dash.Event {
	e := &dash.Event{
		Model: "claude-sonnet-5-5", Provider: "anthropic", Status: 200,
		FreshInput: 100, CacheRead: 2000, CacheWrite: 700, CacheWrite1h: 200, OutputTokens: 50,
		CostUSD: 0.02, TTFBMs: 400, UpstreamMs: 1500, CGLatencyMs: 12,
	}
	e.Stream = true // promoted from the embedded Meta: not allowed in a literal before Go 1.27
	return e
}

func TestTokensSplitIntoFiveTiers(t *testing.T) {
	im, r := newInference(t)
	im.Observe(goodEvent(), 2*time.Second)
	got := intByLabel(collectAll(t, r)["llm.inference.tokens"], "tier")
	want := map[string]int64{"input": 100, "cache_read": 2000, "cache_write_5m": 500, "cache_write_1h": 200, "output": 50}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("tier %s = %d, want %d (5m is CacheWrite minus the 1h part)", k, got[k], v)
		}
	}
}

func TestRequestsAndErrorsByStatus(t *testing.T) {
	im, r := newInference(t)
	for _, st := range []int{200, 200, 429, 502, 0} {
		e := goodEvent()
		e.Status = st
		im.Observe(e, time.Second)
	}
	all := collectAll(t, r)
	reqs := intByLabel(all["llm.inference.requests"], "status_class")
	if reqs["2xx"] != 2 || reqs["4xx"] != 1 || reqs["5xx"] != 1 || reqs["none"] != 1 {
		t.Errorf("requests by class = %v", reqs)
	}
	var errs int64
	for _, n := range intByLabel(all["llm.inference.errors"], "source") {
		errs += n
	}
	if errs != 2 {
		t.Errorf("errors = %d, want 2 (the 429 and the 502; status 0 is not an error)", errs)
	}
}

func TestCostAndEstimatedCost(t *testing.T) {
	im, r := newInference(t)
	im.Observe(goodEvent(), time.Second)
	e := goodEvent()
	e.PriceEstimated = true
	im.Observe(e, time.Second)
	all := collectAll(t, r)
	if c := sumFloat(all["llm.inference.cost"]); c < 0.0399 || c > 0.0401 {
		t.Errorf("cost = %v, want 0.04", c)
	}
	if c := sumFloat(all["llm.inference.estimated_cost"]); c < 0.0199 || c > 0.0201 {
		t.Errorf("estimated cost = %v, want 0.02 (a subset of cost)", c)
	}
}

// The default OpenTelemetry bounds are for milliseconds. A 0.2 s and a 3 s request must not share a bucket.
func TestDurationHistogramHasSecondBuckets(t *testing.T) {
	im, r := newInference(t)
	im.Observe(goodEvent(), 200*time.Millisecond)
	im.Observe(goodEvent(), 3*time.Second)
	h, ok := collectAll(t, r)["llm.inference.duration"].(metricdata.Histogram[float64])
	if !ok || len(h.DataPoints) == 0 {
		t.Fatal("no duration histogram")
	}
	dp := h.DataPoints[0]
	var filled int
	for _, c := range dp.BucketCounts {
		if c > 0 {
			filled++
		}
	}
	if filled != 2 {
		t.Errorf("a 0.2 s and a 3 s request fell into %d bucket(s) of %v, want 2", filled, dp.Bounds)
	}
}

func TestTTFBOnlyForStreamedAndNotBuffered(t *testing.T) {
	im, r := newInference(t)
	e := goodEvent()
	e.Stream = false
	im.Observe(e, time.Second)
	b := goodEvent()
	b.SSEBuffered = true // a buffered stream's first byte is its last byte: a different measurement
	im.Observe(b, time.Second)
	if _, has := collectAll(t, r)["llm.inference.ttfb"]; has {
		t.Fatal("ttfb recorded for a non-stream or a buffered stream")
	}
}

func TestMissingModelAndProviderGetUnknown(t *testing.T) {
	im, r := newInference(t)
	e := goodEvent()
	e.Model, e.Provider = "", ""
	im.Observe(e, time.Second)
	if got := intByLabel(collectAll(t, r)["llm.inference.requests"], "model"); got["unknown"] != 1 {
		t.Errorf("model labels = %v, want unknown (never an empty value)", got)
	}
}

func TestNilInferenceMetricsIsSafe(t *testing.T) {
	var im *InferenceMetrics
	im.Observe(goodEvent(), time.Second)
	im2, _ := newInference(t)
	im2.Observe(nil, time.Second)
}

// A nil event field must not take the request down.
func TestInferenceObserveSurvivesAPanic(t *testing.T) {
	im, _ := newInference(t)
	im.requests = nil // a nil instrument panics on Add
	im.Observe(goodEvent(), time.Second)
}
