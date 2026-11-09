package proxy

import (
	"context"
	"log/slog"
	"time"

	"github.com/rossoctl/accountant"
	acmetrics "github.com/rossoctl/accountant/metrics"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"go.opentelemetry.io/otel/metric"
)

// metricsPricer gives the shared metrics module this proxy's own rates (R47). The module has no
// rate table. Price reads the same chain the dashboard prices from, and it never blocks: the
// LiteLLM link fetches in the background and answers from memory.
type metricsPricer struct{ prices modelinfo.Pricer }

func (p metricsPricer) Price(model string, _ time.Time, t acmetrics.Tier) (float64, bool) {
	if p.prices == nil {
		return 0, true
	}
	pr, ok := p.prices.Price(context.Background(), model)
	if !ok {
		return 0, true // unknown model: no money, marked estimated
	}
	pr = pr.Resolved(model)
	switch t {
	case acmetrics.TierInput:
		return pr.Input, false
	case acmetrics.TierCacheRead:
		return pr.CacheRead, false
	case acmetrics.TierCacheWrite5m:
		return pr.CacheWrite, false
	case acmetrics.TierCacheWrite1h:
		return pr.CacheWrite1h(), false
	case acmetrics.TierOutput:
		return pr.Output, false
	}
	return 0, true
}

// NewMetricsSink returns the accountant sink for the savings counters, priced from prices. A nil
// prices gives zero money and marks every figure estimated.
func NewMetricsSink(m metric.Meter, prices modelinfo.Pricer, components ...string) (acmetrics.Observer, error) {
	var opts []acmetrics.Option
	if len(components) > 0 {
		opts = append(opts, acmetrics.WithComponents(components...))
	}
	return acmetrics.New(m, metricsPricer{prices: prices}, opts...)
}

// observeMetrics hands one priced request to the sink. It runs after the request is answered. A
// panic in the sink costs the metric only: the request has been served (R46, R20).
func (c *capture) observeMetrics(res accountant.Result, usage accountant.Usage) {
	if c == nil || c.metrics == nil || c.accReq == nil {
		return
	}
	defer func() {
		if p := recover(); p != nil {
			slog.Error("metrics: Observe panicked; this request books no metric", "panic", p)
		}
	}()
	c.metrics.Observe(acmetrics.Observation{Meta: c.accReq.Meta, Usage: usage, Result: res})
}
