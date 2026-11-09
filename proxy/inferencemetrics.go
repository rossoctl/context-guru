package proxy

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/rossoctl/context-guru/dash"
)

// InferenceMetrics is the common inference set (metrics spec 5.1): requests, errors, tokens by tier,
// cost, and timing. It reads the finished dash.Event, so it needs no measurement of its own on the
// request path. Each Observe is a few counter adds and histogram records (R46).
//
// Not here yet: llm_inference_request_bytes_total and llm_inference_response_bytes_total. The proxy
// does not count the four body sizes on every response path, and a wrong byte count is worse than
// none.
type InferenceMetrics struct {
	requests, errors    metric.Int64Counter
	tokens              metric.Int64Counter
	cost, estimatedCost metric.Float64Counter
	duration, ttfb      metric.Float64Histogram
	upstream, added     metric.Float64Histogram
}

// Seconds buckets. The OpenTelemetry default bounds are tuned for milliseconds (0, 5, 10 ... 10000):
// a request of 0.2 s and one of 3 s would share the first bucket.
var secondsBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}

// addedBuckets suit the time the proxy adds: milliseconds to a few seconds.
var addedBuckets = []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}

// NewInferenceMetrics builds every instrument once.
func NewInferenceMetrics(m metric.Meter) (*InferenceMetrics, error) {
	im := &InferenceMetrics{}
	var err error
	i64 := func(dst *metric.Int64Counter, name, desc string) {
		if err == nil {
			*dst, err = m.Int64Counter(name, metric.WithDescription(desc))
		}
	}
	f64 := func(dst *metric.Float64Counter, name, desc string) {
		if err == nil {
			*dst, err = m.Float64Counter(name, metric.WithDescription(desc), metric.WithUnit("USD"))
		}
	}
	hist := func(dst *metric.Float64Histogram, name, desc string, b []float64) {
		if err == nil {
			*dst, err = m.Float64Histogram(name, metric.WithDescription(desc), metric.WithUnit("s"),
				metric.WithExplicitBucketBoundaries(b...))
		}
	}
	i64(&im.requests, "llm.inference.requests", "Requests that reached the proxy.")
	i64(&im.errors, "llm.inference.errors", "Requests that failed, by who caused it.")
	i64(&im.tokens, "llm.inference.tokens", "Provider-billed tokens, by tier.")
	f64(&im.cost, "llm.inference.cost", "Provider cost at request time.")
	f64(&im.estimatedCost, "llm.inference.estimated_cost", "Part of the cost priced from a fallback rate.")
	hist(&im.duration, "llm.inference.duration", "Total time, from request to last byte.", secondsBuckets)
	hist(&im.ttfb, "llm.inference.ttfb", "Time to first byte, for streamed answers.", secondsBuckets)
	hist(&im.upstream, "llm.inference.upstream", "Time the provider took.", secondsBuckets)
	hist(&im.added, "llm.inference.added", "Time the proxy added on top of the upstream time.", addedBuckets)
	if err != nil {
		return nil, err
	}
	return im, nil
}

// Observe books one finished request. elapsed is the time from the request's arrival to now. It makes
// no network call and takes no blocking lock.
func (im *InferenceMetrics) Observe(e *dash.Event, elapsed time.Duration) {
	if im == nil || e == nil {
		return
	}
	defer func() {
		if p := recover(); p != nil {
			slog.Error("metrics: inference Observe panicked; this request books no inference metric", "panic", p)
		}
	}()
	ctx := context.Background()
	model, provider := label(e.Model), label(e.Provider)
	class := statusClass(e.Status)

	im.requests.Add(ctx, 1, metric.WithAttributes(
		attribute.String("model", model), attribute.String("provider", provider),
		attribute.String("status_class", class), attribute.Bool("stream", e.Stream)))
	if src := errorSource(e.Status); src != "" {
		im.errors.Add(ctx, 1, metric.WithAttributes(
			attribute.String("model", model), attribute.String("provider", provider),
			attribute.String("status_class", class), attribute.String("source", src)))
	}

	mp := metric.WithAttributes(attribute.String("model", model), attribute.String("provider", provider))
	im.tokens.Add(ctx, e.FreshInput, withTier(model, provider, "input"))
	im.tokens.Add(ctx, e.CacheRead, withTier(model, provider, "cache_read"))
	im.tokens.Add(ctx, e.CacheWrite-e.CacheWrite1h, withTier(model, provider, "cache_write_5m"))
	im.tokens.Add(ctx, e.CacheWrite1h, withTier(model, provider, "cache_write_1h"))
	im.tokens.Add(ctx, e.OutputTokens, withTier(model, provider, "output"))

	if e.CostUSD > 0 {
		im.cost.Add(ctx, e.CostUSD, mp)
		if e.PriceEstimated {
			im.estimatedCost.Add(ctx, e.CostUSD, mp)
		}
	}

	if elapsed > 0 {
		im.duration.Record(ctx, elapsed.Seconds(), metric.WithAttributes(
			attribute.String("model", model), attribute.String("provider", provider), attribute.Bool("stream", e.Stream)))
	}
	if e.Stream && e.TTFBMs > 0 && !e.SSEBuffered {
		im.ttfb.Record(ctx, e.TTFBMs/1000, mp)
	}
	if e.UpstreamMs > 0 {
		im.upstream.Record(ctx, e.UpstreamMs/1000, mp)
	}
	if e.CGLatencyMs > 0 {
		im.added.Record(ctx, e.CGLatencyMs/1000, metric.WithAttributes(attribute.String("model", model)))
	}
}

func withTier(model, provider, tier string) metric.AddOption {
	return metric.WithAttributes(attribute.String("model", model), attribute.String("provider", provider),
		attribute.String("tier", tier))
}

// label keeps an empty model or provider from becoming an empty label value.
func label(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// statusClass groups an HTTP status. Status 0 means no upstream answer was recorded.
func statusClass(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "2xx"
	case status >= 300 && status < 400:
		return "3xx"
	case status >= 400 && status < 500:
		return "4xx"
	case status >= 500:
		return "5xx"
	}
	return "none"
}

// errorSource says who caused a failed request, as far as the proxy knows. Status 0 is not an error:
// the request never reached the upstream path (a refusal at the door, a bypass). dashcapture.go's
// failed() makes the same call.
//
//	5xx   upstream  the provider failed, or the proxy could not reach it (the proxy writes the 502)
//	4xx   upstream  the provider refused the request
//
// The proxy does not record a client that hung up, so no request is labelled "client" yet. Each 4xx
// is labelled upstream because the provider is the one that answered. A later change can split a
// client cancel out once the proxy records it (spec 5.1, `source`).
func errorSource(status int) string {
	if status >= 400 {
		return "upstream"
	}
	return ""
}
