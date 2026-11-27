package proxy

import (
	"log/slog"

	"github.com/rossoctl/accountant"
	acmetrics "github.com/rossoctl/accountant/metrics"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"go.opentelemetry.io/otel/metric"
)

// NewMetricsSink returns the accountant sink for the savings counters, priced from prices. A nil
// prices gives zero money and marks every figure estimated.
func NewMetricsSink(m metric.Meter, prices modelinfo.Pricer, components ...string) (acmetrics.Observer, error) {
	var opts []acmetrics.Option
	if len(components) > 0 {
		opts = append(opts, acmetrics.WithComponents(components...))
	}
	return acmetrics.New(m, modelinfo.ChainRates(prices), opts...)
}

// observeMetrics hands one priced request to the sink. It runs after the request is answered. A
// panic in the sink costs the metric only: the request has been served (R46, R20).
func (c *capture) observeMetrics(res accountant.Result, usage accountant.Usage, output int) {
	if c == nil || c.metrics == nil || c.accReq == nil {
		return
	}
	defer func() {
		if p := recover(); p != nil {
			slog.Error("metrics: Observe panicked; this request books no metric", "panic", p)
		}
	}()
	c.metrics.Observe(acmetrics.Observation{Meta: c.accMeta, Usage: usage, Result: res, Output: output})
}
