package proxy

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/modelinfo"
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
	added               metric.Float64Histogram

	// Cache health (spec 5.4) and keep-alive (spec 5.3), read from the same finished event.
	cacheMisses   metric.Int64Counter
	cacheMissCost metric.Float64Counter
	pings         metric.Int64Counter
	pingCost      metric.Float64Counter
	prices        modelinfo.Pricer // nil: no money for a miss
	// Proxy health (spec 5.5). The info gauge and the export-failure counter live with the push.
	compDuration metric.Float64Histogram
	compFailures metric.Int64Counter
	failOpen     metric.Int64Counter
	// components bounds the `component` label to the shipped list. Nil keeps every name (a test).
	components map[string]bool
}

// Seconds buckets. The OpenTelemetry default bounds are tuned for milliseconds (0, 5, 10 ... 10000):
// a request of 0.2 s and one of 3 s would share the first bucket.
var secondsBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}

// addedBuckets suit the time the proxy adds: milliseconds to a few seconds.
var addedBuckets = []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}

// NewInferenceMetrics builds every instrument once.
//
// prices gives the miss cost its model rates (R47). Nil, or a model with no rate, books the miss
// count and no money: a blended rate in a cost is the defect this project has hit before.
func NewInferenceMetrics(m metric.Meter, prices modelinfo.Pricer, components ...string) (*InferenceMetrics, error) {
	im := &InferenceMetrics{prices: prices}
	if len(components) > 0 {
		im.components = make(map[string]bool, len(components))
		for _, c := range components {
			im.components[c] = true
		}
	}
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
	hist(&im.added, "llm.inference.added", "Time the proxy added on top of the upstream time.", addedBuckets)
	i64(&im.cacheMisses, "llm.cache.misses", "Cache misses, by cause.")
	f64(&im.cacheMissCost, "llm.cache.miss_cost", "Extra cost of those misses: the write premium on the re-written prefix.")
	hist(&im.compDuration, "llm.proxy.component_duration", "Time spent in each component.", addedBuckets)
	i64(&im.compFailures, "llm.proxy.component_failures", "A component error or panic, or a change the pipeline reverted.")
	i64(&im.failOpen, "llm.proxy.fail_open", "Requests the proxy forwarded unchanged because its own work failed.")
	i64(&im.pings, "llm.keepalive.pings", "Keep-alive pings the proxy sent, by outcome.")
	f64(&im.pingCost, "llm.keepalive.cost", "What the pings cost.")
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
	if e.KeepAlive {
		im.observePing(ctx, e, model)
		return // a ping is the proxy's own traffic: it is not a request the user made
	}
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
	if e.CGLatencyMs > 0 {
		im.added.Record(ctx, e.CGLatencyMs/1000, metric.WithAttributes(attribute.String("model", model)))
	}
	im.observeCacheMiss(ctx, e, model)
	im.observeComponents(ctx, e)
}

// observeComponents books the time and the failures of each component that ran on this request.
// A request whose components acted but were all reverted was forwarded unchanged, which is the
// fail-open (spec 5.5). A bypass is the user's choice and is not one.
func (im *InferenceMetrics) observeComponents(ctx context.Context, e *dash.Event) {
	for _, c := range e.Components {
		name := im.component(c.Component)
		if c.DurationMs > 0 {
			im.compDuration.Record(ctx, c.DurationMs/1000, metric.WithAttributes(attribute.String("component", name)))
		}
		if reason := componentFailure(c); reason != "" {
			im.compFailures.Add(ctx, 1, metric.WithAttributes(
				attribute.String("component", name), attribute.String("reason", reason)))
		}
	}
	if e.UncompressedReason == dash.ReasonReverted {
		im.failOpen.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "all_reverted")))
	}
}

// componentFailure names why a component row counts as a failure, or "" when it does not.
// A panic is told from an error by the text the pipeline gives it ("panic: ...").
func componentFailure(c dash.CompRow) string {
	switch {
	case strings.HasPrefix(c.Err, "panic:"):
		return "panic"
	case c.Err != "":
		return "error"
	case c.Reverted:
		return "reverted"
	}
	return ""
}

// component keeps the label inside the shipped list: any other name is "other".
func (im *InferenceMetrics) component(name string) string {
	if im.components == nil || im.components[name] {
		return name
	}
	return "other"
}

// observePing books one keep-alive ping row. Only ok and failed are visible: a ping that timed out
// or was skipped leaves no row, so those two outcomes are not reported here, and a count that
// leaves them out says so in its help text rather than guessing.
func (im *InferenceMetrics) observePing(ctx context.Context, e *dash.Event, model string) {
	outcome := "ok"
	if e.Status < 200 || e.Status > 299 {
		outcome = "failed"
	}
	im.pings.Add(ctx, 1, metric.WithAttributes(attribute.String("model", model), attribute.String("outcome", outcome)))
	if e.CostUSD > 0 {
		im.pingCost.Add(ctx, e.CostUSD, metric.WithAttributes(attribute.String("model", model)))
	}
}

// observeCacheMiss counts a real request that missed the cache, by cause, and prices the miss.
func (im *InferenceMetrics) observeCacheMiss(ctx context.Context, e *dash.Event, model string) {
	cause, miss := missCause(e)
	if !miss {
		return
	}
	attrs := metric.WithAttributes(attribute.String("model", model), attribute.String("cause", cause))
	im.cacheMisses.Add(ctx, 1, attrs)
	if usd := im.missCostUSD(e); usd > 0 {
		im.cacheMissCost.Add(ctx, usd, attrs)
	}
}

// missCostUSD is the avoidable write premium: the written prefix times (write rate minus read rate),
// as dash/keepalive.go defines a lapse. It is zero for a model with no rate. It does not default.
func (im *InferenceMetrics) missCostUSD(e *dash.Event) float64 {
	if im.prices == nil || e.CacheWrite <= 0 {
		return 0
	}
	p, ok := im.prices.Price(context.Background(), e.Model)
	if !ok {
		return 0
	}
	p = p.Resolved(e.Model)
	if prem := p.CacheWrite - p.CacheRead; prem > 0 {
		return float64(e.CacheWrite) * prem
	}
	return 0
}

// missCause maps the event's reason and cause to the spec's set: ttl_expiry, prefix_change,
// model_changed, compaction_after_rewrite, bypass_flip, cold_start, other. A hit is not a miss.
// tools, system and effort changes all change the cached prefix, so they are prefix_change.
func missCause(e *dash.Event) (string, bool) {
	switch e.CacheMissReason {
	case dash.CacheHit, "":
		return "", false
	case dash.CacheColdStart:
		return "cold_start", true
	case dash.CacheTTLExpiry:
		return "ttl_expiry", true
	}
	switch e.CacheMissCause {
	case dash.CauseModelChanged:
		return "model_changed", true
	case dash.CauseCompactionAfterRewrite:
		return "compaction_after_rewrite", true
	case dash.CauseBypassFlip:
		return "bypass_flip", true
	case dash.CauseToolsChanged, dash.CauseSystemChanged, dash.CauseEffortChanged:
		return "prefix_change", true
	}
	return "other", true
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
