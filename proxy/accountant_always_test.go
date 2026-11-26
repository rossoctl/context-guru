package proxy

import (
	"context"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	acmetrics "github.com/rossoctl/accountant/metrics"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/internal/testutil"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
	sdk "go.opentelemetry.io/otel/sdk/metric"
)

// Issue #1451: the accountant runs on every request, with or without the dashboard.

// figTap collects what each finished request's accountant figure was.
type figTap struct {
	mu   sync.Mutex
	figs []accountant.USDFigure
	oks  []bool
}

func (f *figTap) hook(h *Handler) {
	h.savings.onFinish = func(c *capture, u Usage, usageOK bool) {
		fig, ok := c.accFigure(context.Background(), u, usageOK)
		f.mu.Lock()
		f.figs, f.oks = append(f.figs, fig), append(f.oks, ok)
		f.mu.Unlock()
	}
}

func (f *figTap) wait(t *testing.T, n int) {
	t.Helper()
	testutil.WaitForState(t, "figures", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.figs) >= n }, func() string { return "waiting" })
}

// noDashHandler is a handler with no dashboard. edit changes the options before New.
func noDashHandler(t *testing.T, up string, edit func(*Options)) *Handler {
	t.Helper()
	cfg, err := config.LoadBytes([]byte("preset: codesafe\n"))
	if err != nil {
		t.Fatal(err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{AnthropicUpstream: up, OpenAIUpstream: up, Preset: cfg.Preset, Prices: fixedPricer{}}
	if edit != nil {
		edit(&opts)
	}
	h := New(pipe, store.NewMemory(store.Options{}), agg, opts)
	t.Cleanup(h.Close)
	return h
}

func postTwice(t *testing.T, h *Handler, session string) {
	t.Helper()
	accPost(t, h, session)
	accPost(t, h, session)
}

func sumFigure(f accountant.USDFigure) float64 {
	t := f.DeltaUSD
	for _, v := range f.ByComponent {
		t += v
	}
	return t
}

// Dashboard off: a request with a savings operation gets an accountant result, the metrics counters move, and the
// per-request figure hook returns the figure.
func TestAccountantRunsWithoutTheDashboard(t *testing.T) {
	up := billedUpstream(t)
	defer up.Close()
	reader := sdk.NewManualReader()
	provider := sdk.NewMeterProvider(sdk.WithReader(reader))
	inner, err := NewMetricsSink(provider.Meter("test"), fixedPricer{})
	if err != nil {
		t.Fatal(err)
	}
	sink := &counted{Observer: inner}
	h := noDashHandler(t, up.URL, func(o *Options) { o.MetricsSink = sink })
	if h.rec != nil {
		t.Fatal("the dashboard is on")
	}
	var tap figTap
	tap.hook(h)
	postTwice(t, h, "sess-nodash")
	tap.wait(t, 2)

	if got := sink.n.Load(); got != 2 {
		t.Errorf("the metrics sink saw %d requests, want 2", got)
	}
	if !metricNames(t, reader)["llm.savings.requests"] {
		t.Errorf("no llm.savings.requests counter: %v", metricNames(t, reader))
	}
	if !tap.oks[1] || len(tap.figs[1].ByComponent) == 0 {
		t.Fatalf("the figure hook gave no figure for the second request: ok=%v fig=%+v", tap.oks[1], tap.figs[1])
	}
	if sumFigure(tap.figs[1]) == 0 {
		t.Errorf("the figure is all zero: %+v", tap.figs[1])
	}
}

// Dashboard on: the figures do not change. The hook gives the same figure as the dashboard event, request by request.
func TestAccountantFigureMatchesTheDashboardRow(t *testing.T) {
	up := billedUpstream(t)
	defer up.Close()
	var tap figTap
	var hh *Handler
	h, rec, evs := accHandlerOpts(t, up.URL, "preset: codesafe\n", nil)
	hh = h
	tap.hook(hh)
	postTwice(t, h, "sess-dash-on")
	waitForRows(t, rec, 2)
	got := evs.waitAll(t, 2)
	tap.wait(t, 2)

	for i, e := range got {
		want := e.AccountantFigure(modelinfo.Price{Input: 2e-06, Output: 1e-05, CacheRead: 2e-07, CacheWrite: 2.5e-06})
		if !tap.oks[i] {
			continue
		}
		if math.Abs(want.DeltaUSD-tap.figs[i].DeltaUSD) > 1e-12 {
			t.Errorf("request %d: dashboard %v, hook %v", i, want.DeltaUSD, tap.figs[i].DeltaUSD)
		}
		if math.Abs(want.KeepAliveSavedUSD-tap.figs[i].KeepAliveSavedUSD) > 1e-12 || math.Abs(want.TTLUSD-tap.figs[i].TTLUSD) > 1e-12 {
			t.Errorf("request %d: keep-alive/ttl differ: dashboard %v/%v, hook %v/%v", i, want.KeepAliveSavedUSD, want.TTLUSD, tap.figs[i].KeepAliveSavedUSD, tap.figs[i].TTLUSD)
		}
	}
	if !tap.oks[1] {
		t.Fatal("no figure on the second request")
	}
}

// Dashboard off and on give the same figure for the same requests.
func TestAccountantFigureIsTheSameWithAndWithoutTheDashboard(t *testing.T) {
	up := billedUpstream(t)
	defer up.Close()
	off := noDashHandler(t, up.URL, nil)
	var a figTap
	a.hook(off)
	postTwice(t, off, "sess-same")
	a.wait(t, 2)

	on, rec, _ := accHandlerOpts(t, up.URL, "preset: codesafe\n", nil)
	var b figTap
	b.hook(on)
	postTwice(t, on, "sess-same")
	waitForRows(t, rec, 2)
	b.wait(t, 2)

	for i := range 2 {
		if a.oks[i] != b.oks[i] || math.Abs(a.figs[i].DeltaUSD-b.figs[i].DeltaUSD) > 1e-12 {
			t.Errorf("request %d: off %v/%v, on %v/%v", i, a.oks[i], a.figs[i].DeltaUSD, b.oks[i], b.figs[i].DeltaUSD)
		}
	}
}

// panicPricer prices normally until armed. The test arms it once the request is at the upstream, so the panic comes
// in the accountant's step (the figure and the metrics), after the pipeline has priced and forwarded the request.
type panicPricer struct {
	armed *atomic.Bool
	hits  *atomic.Int32
}

func (p panicPricer) Price(ctx context.Context, m string) (modelinfo.Price, bool) {
	if p.armed.Load() {
		p.hits.Add(1)
		panic("pricer boom")
	}
	return fixedPricer{}.Price(ctx, m)
}

// Fail open: a panicking pricer costs the figure and the metric only. The request is forwarded and answered.
func TestPanickingPricerStillForwardsTheRequestWithoutTheDashboard(t *testing.T) {
	var got atomic.Int32
	var upBody atomic.Value
	pp := panicPricer{armed: new(atomic.Bool), hits: new(atomic.Int32)}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		upBody.Store(string(b))
		got.Add(1)
		pp.armed.Store(true)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":20,"output_tokens":34,"cache_read_input_tokens":0,"cache_creation_input_tokens":50}}`))
	}))
	defer up.Close()
	sink, err := NewMetricsSink(sdk.NewMeterProvider(sdk.WithReader(sdk.NewManualReader())).Meter("t"), pp)
	if err != nil {
		t.Fatal(err)
	}
	h := noDashHandler(t, up.URL, func(o *Options) { o.Prices = pp; o.MetricsSink = sink })
	var tap figTap
	tap.hook(h)

	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	for range 1 { // one request: the upstream arms the panic, and no later request must meet it in the pipeline
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages", strings.NewReader(string(anthropicRequest("aws/claude-sonnet-5"))))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-context-guru-session", "sess-panic-pricer")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), `"ok"`) {
			t.Fatalf("status %d body %s: the panicking pricer changed the answer", resp.StatusCode, b)
		}
	}
	tap.wait(t, 1)
	if got.Load() != 1 || upBody.Load() == nil {
		t.Fatalf("upstream saw %d requests, want 1", got.Load())
	}
	if pp.hits.Load() == 0 {
		t.Error("the pricer never panicked: the test proved nothing")
	}
	for i, ok := range tap.oks {
		if ok {
			t.Errorf("request %d: a figure came out of a panicking pricer", i)
		}
	}
}

var _ acmetrics.Observer = (*counted)(nil)

// BenchmarkRequestWithoutDashboard is the cost of the always-on accountant (#1451): one full request through the
// handler with no dashboard, with the accountant off (the old behavior: no savings tracker) and on.
func BenchmarkRequestWithoutDashboard(b *testing.B) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":20,"output_tokens":34,"cache_read_input_tokens":0,"cache_creation_input_tokens":50}}`))
	}))
	defer up.Close()
	defer slog.SetDefault(slog.Default())
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	bodies := map[string][]byte{
		"small": anthropicRequest("aws/claude-sonnet-5"),
		"large": manyToolResultsRequest("claude-sonnet-5", 24),
	}
	for _, size := range []string{"small", "large"} {
		for _, on := range []bool{false, true} {
			name := size + "/accountant_off"
			if on {
				name = size + "/accountant_on"
			}
			b.Run(name, func(b *testing.B) {
				cfg, _ := config.LoadBytes([]byte("preset: codesafe\n"))
				agg := metrics.NewAggregator()
				pipe, _ := cfg.Build(agg)
				h := New(pipe, store.NewMemory(store.Options{}), agg, Options{AnthropicUpstream: up.URL, OpenAIUpstream: up.URL, Preset: cfg.Preset, Prices: fixedPricer{}})
				defer h.Close()
				if !on {
					h.savings = nil
				}
				srv := httptest.NewServer(h.Mux())
				defer srv.Close()
				body := bodies[size]
				b.ReportAllocs()
				var headers time.Duration
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					t0 := time.Now()
					req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages", strings.NewReader(string(body)))
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("x-context-guru-session", "bench")
					resp, err := srv.Client().Do(req)
					if err != nil {
						b.Fatal(err)
					}
					headers += time.Since(t0) // the client has the response head: what it sees as latency
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
				b.ReportMetric(float64(headers.Nanoseconds())/float64(b.N), "headers-ns/op")
			})
		}
	}
}
