package proxy

import (
	"context"
	"testing"
	"time"

	sdk "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

func newInference(t *testing.T) (*InferenceMetrics, *sdk.ManualReader) {
	t.Helper()
	r := sdk.NewManualReader()
	im, err := NewInferenceMetrics(sdk.NewMeterProvider(sdk.WithReader(r)).Meter("test"), nil)
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
	if c := sumFloat(all["llm.inference.cost.usd"]); c < 0.0399 || c > 0.0401 {
		t.Errorf("cost = %v, want 0.04", c)
	}
	if c := sumFloat(all["llm.inference.estimated_cost.usd"]); c < 0.0199 || c > 0.0201 {
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

type ratePricer map[string]modelinfo.Price

func (r ratePricer) Price(_ context.Context, model string) (modelinfo.Price, bool) {
	p, ok := r[model]
	return p, ok
}

func newInferenceWith(t *testing.T, prices modelinfo.Pricer) (*InferenceMetrics, *sdk.ManualReader) {
	t.Helper()
	r := sdk.NewManualReader()
	im, err := NewInferenceMetrics(sdk.NewMeterProvider(sdk.WithReader(r)).Meter("test"), prices)
	if err != nil {
		t.Fatal(err)
	}
	return im, r
}

func missEvent(reason, cause string) *dash.Event {
	e := goodEvent()
	e.CacheRead, e.CacheWrite, e.CacheWrite1h = 0, 1000, 0
	e.CacheMissReason, e.CacheMissCause = reason, cause
	return e
}

// Every reason and cause the event can carry lands in the spec's set, and a hit is not a miss.
func TestMissCauseMapsIntoTheSpecSet(t *testing.T) {
	for _, tc := range []struct {
		reason, cause, want string
		miss                bool
	}{
		{dash.CacheHit, "", "", false},
		{"", "", "", false},
		{dash.CacheColdStart, "", "cold_start", true},
		{dash.CacheTTLExpiry, "", "ttl_expiry", true},
		{dash.CacheUnknown, dash.CauseModelChanged, "model_changed", true},
		{dash.CacheUnknown, dash.CauseCompactionAfterRewrite, "compaction_after_rewrite", true},
		{dash.CacheUnknown, dash.CauseBypassFlip, "bypass_flip", true},
		{dash.CacheUnknown, dash.CauseToolsChanged, "prefix_change", true},
		{dash.CacheUnknown, dash.CauseSystemChanged, "prefix_change", true},
		{dash.CacheUnknown, dash.CauseEffortChanged, "prefix_change", true},
		{dash.CacheUnknown, dash.CauseConcurrent, "other", true},
		{dash.CacheUnknown, "", "other", true},
	} {
		got, miss := missCause(missEvent(tc.reason, tc.cause))
		if got != tc.want || miss != tc.miss {
			t.Errorf("(%q,%q) = (%q,%v), want (%q,%v)", tc.reason, tc.cause, got, miss, tc.want, tc.miss)
		}
	}
}

// The miss cost is the write premium on the written prefix, with the model's own rates.
func TestMissCostIsTheWritePremium(t *testing.T) {
	im, r := newInferenceWith(t, ratePricer{"claude-sonnet-5-5": {CacheWrite: 3.75e-6, CacheRead: 0.3e-6}})
	im.Observe(missEvent(dash.CacheTTLExpiry, ""), time.Second)
	all := collectAll(t, r)
	if got := intByLabel(all["llm.cache.misses"], "cause"); got["ttl_expiry"] != 1 {
		t.Errorf("misses = %v, want one ttl_expiry", got)
	}
	want := 1000 * (3.75e-6 - 0.3e-6) // 1000 written tokens
	if got := sumFloat(all["llm.cache.miss_cost.usd"]); got < want*0.999 || got > want*1.001 {
		t.Errorf("miss cost = %v, want %v (1000 x (write - read))", got, want)
	}
}

// A model with no rate counts the miss and books no money. It never falls back to another rate.
func TestUnpricedMissBooksNoMoney(t *testing.T) {
	im, r := newInferenceWith(t, ratePricer{})
	im.Observe(missEvent(dash.CacheTTLExpiry, ""), time.Second)
	all := collectAll(t, r)
	if intByLabel(all["llm.cache.misses"], "cause")["ttl_expiry"] != 1 {
		t.Error("the miss was not counted")
	}
	if _, has := all["llm.cache.miss_cost.usd"]; has {
		t.Error("a miss on an unpriced model booked money")
	}
}

func TestHitIsNotAMiss(t *testing.T) {
	im, r := newInferenceWith(t, ratePricer{})
	e := goodEvent()
	e.CacheMissReason = dash.CacheHit
	im.Observe(e, time.Second)
	if _, has := collectAll(t, r)["llm.cache.misses"]; has {
		t.Fatal("a hit was counted as a miss")
	}
}

// A ping is the proxy's own traffic: it books pings and cost, and nothing in the request metrics.
func TestPingBooksKeepAliveAndNoRequest(t *testing.T) {
	im, r := newInferenceWith(t, nil)
	ok := goodEvent()
	ok.KeepAlive, ok.CostUSD = true, 0.003
	bad := goodEvent()
	bad.KeepAlive, bad.Status, bad.CostUSD = true, 529, 0 // a refused ping costs nothing
	im.Observe(ok, time.Second)
	im.Observe(bad, time.Second)
	all := collectAll(t, r)
	got := intByLabel(all["llm.keepalive.pings"], "outcome")
	if got["ok"] != 1 || got["failed"] != 1 {
		t.Errorf("pings = %v, want one ok and one failed", got)
	}
	if c := sumFloat(all["llm.keepalive.cost.usd"]); c < 0.0029 || c > 0.0031 {
		t.Errorf("ping cost = %v, want 0.003", c)
	}
	for _, name := range []string{"llm.inference.requests", "llm.inference.tokens", "llm.inference.cost.usd"} {
		if _, has := all[name]; has {
			t.Errorf("a ping reached %s: it is not a request the user made", name)
		}
	}
}

func healthEvent(rows ...dash.CompRow) *dash.Event {
	e := goodEvent()
	e.Components = rows
	return e
}

func TestComponentTimeAndFailureReasons(t *testing.T) {
	r := sdk.NewManualReader()
	im, err := NewInferenceMetrics(sdk.NewMeterProvider(sdk.WithReader(r)).Meter("test"), nil, "mask", "dedup")
	if err != nil {
		t.Fatal(err)
	}
	im.Observe(healthEvent(
		dash.CompRow{Component: "mask", DurationMs: 12},
		dash.CompRow{Component: "dedup", Err: "panic: boom"},
		dash.CompRow{Component: "mask", Err: "bad input"},
		dash.CompRow{Component: "dedup", Reverted: true},
		dash.CompRow{Component: "not-shipped", Err: "x"},
	), time.Second)
	all := collectAll(t, r)
	got := intByLabel(all["llm.proxy.component_failures"], "reason")
	if got["panic"] != 1 || got["error"] != 2 || got["reverted"] != 1 {
		t.Errorf("failures by reason = %v, want panic 1, error 2 (one of them from a name outside the list), reverted 1", got)
	}
	comps := intByLabel(all["llm.proxy.component_failures"], "component")
	if comps["other"] != 1 || comps["not-shipped"] != 0 {
		t.Errorf("failures by component = %v, want the unlisted name folded into other", comps)
	}
	if _, ok := all["llm.proxy.component_duration"]; !ok {
		t.Error("no component duration was recorded")
	}
}

// All-reverted is a fail-open. A bypass is the user's choice and is not.
func TestFailOpenIsAllRevertedNotBypass(t *testing.T) {
	im, r := newInferenceWith(t, nil)
	rev := healthEvent()
	rev.UncompressedReason = dash.ReasonReverted
	byp := healthEvent()
	byp.UncompressedReason = dash.ReasonBypassed
	none := healthEvent()
	im.Observe(rev, time.Second)
	im.Observe(byp, time.Second)
	im.Observe(none, time.Second)
	got := intByLabel(collectAll(t, r)["llm.proxy.fail_open"], "reason")
	if got["all_reverted"] != 1 || len(got) != 1 {
		t.Errorf("fail_open = %v, want only one all_reverted", got)
	}
}

func TestHealthyRequestBooksNoFailure(t *testing.T) {
	im, r := newInferenceWith(t, nil)
	im.Observe(healthEvent(dash.CompRow{Component: "mask", DurationMs: 3, Acted: true}), time.Second)
	all := collectAll(t, r)
	if _, has := all["llm.proxy.component_failures"]; has {
		t.Error("a healthy component was counted as a failure")
	}
	if _, has := all["llm.proxy.fail_open"]; has {
		t.Error("a healthy request was counted as a fail-open")
	}
}

// /compact reaches finish but calls no provider. It must not count as a request or add tokens.
func TestCompactIsNotAnInferenceRequest(t *testing.T) {
	im, r := newInferenceWith(t, nil)
	e := goodEvent()
	e.Route = "/compact"
	im.Observe(e, time.Second)
	if all := collectAll(t, r); len(all) != 0 {
		t.Fatalf("a /compact row booked metrics: %v", all)
	}
	im.Observe(goodEvent(), time.Second) // an ordinary request still counts
	if _, has := collectAll(t, r)["llm.inference.requests"]; !has {
		t.Fatal("an ordinary request was not counted")
	}
}
