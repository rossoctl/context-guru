package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	acmetrics "github.com/rossoctl/accountant/metrics"
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
