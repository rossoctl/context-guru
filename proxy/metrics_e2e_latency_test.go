package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/proxyapp/metricspush"
)

// e2eRun sends n requests one after another on one kept-alive connection and returns the time of
// each, from the send to the last byte of the answer.
func e2eRun(t *testing.T, h *Handler, srvURL string, client *http.Client, n int) []time.Duration {
	t.Helper()
	body := string(anthropicRequest("aws/claude-sonnet-5"))
	out := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		req, _ := http.NewRequest(http.MethodPost, srvURL+"/anthropic/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-context-guru-session", "lat")
		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		out = append(out, time.Since(start))
	}
	return out
}

func pct(d []time.Duration, p int) time.Duration {
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)*p/100]
}

// metricsMode builds the handler options for one scenario. collector is the URL to push to; ""
// means metrics are off.
func metricsMode(t *testing.T, collector string) func(*Options) {
	if collector == "" {
		return nil
	}
	return func(o *Options) {
		provider, shutdown, err := metricspush.Start(context.Background(), metricspush.Config{
			URL: collector, Interval: 20 * time.Millisecond, Proxy: "context-guru", Machine: "0123456789abcdef0123456789abcdef"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = shutdown(ctx)
		})
		sink, err := NewMetricsSink(provider.Meter("e2e"), fixedPricer{})
		if err != nil {
			t.Fatal(err)
		}
		inf, err := NewInferenceMetrics(provider.Meter("e2e"), fixedPricer{})
		if err != nil {
			t.Fatal(err)
		}
		o.MetricsSink, o.InferenceMetrics = sink, inf
	}
}

// TestMetricsDoNotChangeResponseTime is the benchmark of R46. It runs the real handler with metrics
// off and on, against a collector that answers, hangs and refuses, in interleaved rounds so that
// drift on the machine cancels out. It reports p50 and p99 of the full request time. It logs; it
// fails only above a generous ceiling, because the budget number is the maintainer's to choose.
func TestMetricsDoNotChangeResponseTime(t *testing.T) {
	if testing.Short() {
		t.Skip("benchmark")
	}
	up := usageUpstream(t)

	answers := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer answers.Close()
	release := make(chan struct{})
	hangs := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer hangs.Close()
	defer close(release)
	refuses := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	refusedURL := refuses.URL
	refuses.Close()

	type scenario struct {
		name      string
		collector string
		h         *Handler
		srv       *httptest.Server
		times     []time.Duration
	}
	scenarios := []*scenario{{name: "off", collector: ""}, {name: "on, collector answers", collector: answers.URL},
		{name: "on, collector hangs", collector: hangs.URL}, {name: "on, collector refuses", collector: refusedURL}}
	for _, s := range scenarios {
		h, _, _ := accHandlerOpts(t, up.URL, "preset: codesafe\n", metricsMode(t, s.collector))
		s.h, s.srv = h, httptest.NewServer(h.Mux())
		defer s.srv.Close()
		e2eRun(t, s.h, s.srv.URL, s.srv.Client(), 30) // warm the connection and the caches
	}
	rounds, per := 20, 25
	for r := 0; r < rounds; r++ {
		for _, s := range scenarios {
			s.times = append(s.times, e2eRun(t, s.h, s.srv.URL, s.srv.Client(), per)...)
		}
	}
	base50, base99 := pct(scenarios[0].times, 50), pct(scenarios[0].times, 99)
	var report strings.Builder
	report.WriteString("\n")
	for _, s := range scenarios {
		p50, p99 := pct(s.times, 50), pct(s.times, 99)
		report.WriteString(strings.Join([]string{
			pad(s.name, 26), "p50=", p50.String(), " (", (p50 - base50).String(), ")  p99=", p99.String(), " (", (p99 - base99).String(), ")\n"}, ""))
		if p99 > base99*3+5*time.Millisecond {
			t.Errorf("%s: p99 %v is far above the metrics-off p99 %v", s.name, p99, base99)
		}
	}
	t.Log(report.String())
	if os.Getenv("R46_VERBOSE") != "" {
		t.Logf("%d requests for each scenario", len(scenarios[0].times))
	}
}

func pad(s string, n int) string {
	for len(s) < n {
		s += " "
	}
	return s
}
