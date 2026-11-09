package metricspush_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/rossoctl/context-guru/internal/metricspush"
)

// TestCentralRecordingRulesGiveTheRightNumber pushes counters at a steady, known rate and checks that
// the recording rules compute that rate. A rule can pass promtool and still be wrong: this checks
// the arithmetic. It takes about 6 minutes, because rate() needs a window, so it only runs when
// CENTRAL_SLOW is set.
//
// Known rates, per second, over the run:
//
//	saved usd 0.002, added usd 0.0005      -> net 0.0015
//	requests 2xx 9 and 5xx 1 per second    -> 5xx ratio 0.1
//	cache_read 30, input 10 tokens/s       -> cache read ratio 0.75
func TestCentralRecordingRulesGiveTheRightNumber(t *testing.T) {
	collector, token, prom := central(t)
	if testing.Short() || os.Getenv("CENTRAL_SLOW") == "" {
		t.Skip("slow (about 6 minutes): set CENTRAL_SLOW=1")
	}
	machine := fmt.Sprintf("rule%028d", time.Now().UnixNano()%1e12)
	p, stop, err := metricspush.Start(context.Background(), metricspush.Config{URL: collector, Token: token,
		Interval: 5 * time.Second, Proxy: "context-guru", Machine: machine, Version: "e2e", Preset: "rules"})
	if err != nil {
		t.Fatal(err)
	}
	m := p.Meter("context-guru")
	saved, _ := m.Float64Counter("llm.savings.saved.usd")
	added, _ := m.Float64Counter("llm.savings.added.usd")
	reqs, _ := m.Int64Counter("llm.inference.requests")
	toks, _ := m.Int64Counter("llm.inference.tokens")
	ctx := context.Background()
	comp := metric.WithAttributes(attribute.String("kind", "rewrite"), attribute.String("tier", "input"), attribute.String("component", "mask"))
	ok2 := metric.WithAttributes(attribute.String("model", "m1"), attribute.String("provider", "anthropic"), attribute.String("status_class", "2xx"), attribute.Bool("stream", false))
	e5 := metric.WithAttributes(attribute.String("model", "m1"), attribute.String("provider", "anthropic"), attribute.String("status_class", "5xx"), attribute.Bool("stream", false))
	tier := func(s string) metric.AddOption {
		return metric.WithAttributes(attribute.String("model", "m1"), attribute.String("provider", "anthropic"), attribute.String("tier", s))
	}
	// Each tick is 1 second of traffic.
	end := time.Now().Add(6 * time.Minute)
	for tick := time.NewTicker(time.Second); time.Now().Before(end); <-tick.C {
		saved.Add(ctx, 0.002, comp)
		added.Add(ctx, 0.0005, comp)
		reqs.Add(ctx, 9, ok2)
		reqs.Add(ctx, 1, e5)
		toks.Add(ctx, 30, tier("cache_read"))
		toks.Add(ctx, 10, tier("input"))
	}
	_ = stop(ctx)

	check := func(name, q string, want, tol float64) {
		var got float64 = -1
		for i := 0; i < 30; i++ { // the rule runs once a minute
			var res []struct {
				Value [2]any `json:"value"`
			}
			raw := promQuery(t, prom, "/api/v1/query", url.Values{"query": {q}})
			var d struct {
				Result json.RawMessage `json:"result"`
			}
			_ = json.Unmarshal(raw, &d)
			_ = json.Unmarshal(d.Result, &res)
			if len(res) > 0 {
				got, _ = strconv.ParseFloat(res[0].Value[1].(string), 64)
				break
			}
			time.Sleep(5 * time.Second)
		}
		if got < 0 {
			t.Errorf("%s: no data for %s", name, q)
			return
		}
		if got < want-tol || got > want+tol {
			t.Errorf("%s = %v, want %v +/- %v", name, got, want, tol)
		} else {
			t.Logf("%s = %v (want %v)", name, got, want)
		}
	}
	sel := fmt.Sprintf(`{machine=%q}`, machine)
	check("net saving rate", `sum(proxy_component:llm_savings_net_usd:rate5m)`, 0.0015, 0.0002)
	check("5xx ratio", `proxy:llm_inference_5xx_ratio:rate5m`, 0.1, 0.02)
	check("cache read ratio", `proxy_model:llm_cache_read_ratio:rate1h{model="m1"}`, 0.75, 0.05)
	_ = sel
}
