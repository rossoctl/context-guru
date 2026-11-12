package metricspush_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	acmetrics "github.com/rossoctl/accountant/metrics"

	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/metricspush"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/proxy"
)

// These tests need the central stack (deploy/central/compose.yaml) to be up. They push real metrics
// through the real collector into Prometheus and read them back. They are skipped unless
// CENTRAL_COLLECTOR and CENTRAL_PROMETHEUS are set:
//
//	CENTRAL_COLLECTOR=http://127.0.0.1:4319 CENTRAL_PROMETHEUS=http://127.0.0.1:9090 \
//	go test ./internal/metricspush -run Central -v
//
// Authentication is off on the central (rossoctl/context-guru#1130), so no token is needed. The one test
// that checks a token is refused runs only with CENTRAL_AUTH=on.
//
// A unit test cannot catch what these catch: a unit suffix that the exporter appends as written
// (llm_inference_cost_USD_total), labels the collector adds or drops, and a rule that reads a
// metric no proxy exports.

type rates struct{}

func (rates) Price(context.Context, string) (modelinfo.Price, bool) {
	return modelinfo.Price{Input: 3e-6, Output: 15e-6, CacheRead: 0.3e-6, CacheWrite: 3.75e-6}, true
}

type flat struct{}

func (flat) Price(string, time.Time, acmetrics.Tier) (float64, bool) { return 1e-6, false }

func central(t *testing.T) (collector, token, prom string) {
	t.Helper()
	collector, prom = os.Getenv("CENTRAL_COLLECTOR"), os.Getenv("CENTRAL_PROMETHEUS")
	if collector == "" || prom == "" {
		t.Skip("set CENTRAL_COLLECTOR and CENTRAL_PROMETHEUS to run against the central stack")
	}
	return collector, os.Getenv("CENTRAL_TOKEN"), prom
}

func promQuery(t *testing.T, prom, path string, q url.Values) json.RawMessage {
	t.Helper()
	resp, err := http.Get(prom + path + "?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("prometheus: %v\n%s", err, b)
	}
	return out.Data
}

// push sends one machine's metrics and returns that machine's ID.
func push(t *testing.T, collector, token, machine string) {
	t.Helper()
	p, stop, err := metricspush.Start(context.Background(), metricspush.Config{URL: collector, Token: token,
		Interval: time.Hour, Proxy: "context-guru", Machine: machine, Version: "e2e", Preset: "codesafe"})
	if err != nil {
		t.Fatal(err)
	}
	m := p.Meter("context-guru")
	sink, _ := acmetrics.New(m, flat{})
	inf, _ := proxy.NewInferenceMetrics(m, rates{}, "mask")
	sink.Observe(acmetrics.Observation{
		Meta: accountant.RequestMeta{Model: "claude-sonnet-5-5", At: time.Now(), Billing: accountant.Billing{Mode: accountant.BillingAPI}},
		Result: accountant.Result{
			Unpriced:    []accountant.Unpriced{{Reason: "x"}},
			Checks:      []accountant.CheckFailure{{Check: "sum"}},
			Interaction: accountant.TierTokens{CacheWrite5m: 10},
			Ops: []accountant.PricedOp{{Kind: accountant.KindRewrite, Operator: "context-guru.mask",
				Saved: accountant.TierTokens{Input: 100}, Caused: accountant.TierTokens{CacheWrite5m: 10}}},
			CacheEffects: []accountant.CacheEffect{{Kind: accountant.EffectRescue,
				Saved: accountant.TierTokens{CacheWrite5m: 50}, Caused: accountant.TierTokens{CacheRead: 50}}}}})
	ok := &dash.Event{Model: "claude-sonnet-5-5", Provider: "anthropic", Status: 200, FreshInput: 10, CacheRead: 20,
		CacheWrite: 30, OutputTokens: 5, CostUSD: 0.01, UpstreamMs: 900, CGLatencyMs: 12, TTFBMs: 300}
	ok.Stream = true
	ok.CacheMissReason = dash.CacheTTLExpiry
	ok.Components = []dash.CompRow{{Component: "mask", DurationMs: 3}}
	inf.Observe(ok, 1200*time.Millisecond)
	bad := &dash.Event{Model: "claude-sonnet-5-5", Provider: "anthropic", Status: 529, CostUSD: 0.002, PriceEstimated: true,
		CacheWrite: 100, CacheMissReason: dash.CacheTTLExpiry, UncompressedReason: dash.ReasonReverted}
	bad.Components = []dash.CompRow{{Component: "mask", Err: "panic: x"}}
	inf.Observe(bad, time.Second)
	ping := &dash.Event{Model: "claude-sonnet-5-5", Provider: "anthropic", Status: 200, KeepAlive: true, CostUSD: 0.001}
	inf.Observe(ping, 0)
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := stop(stopCtx); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

// names lists the llm_ series names Prometheus holds for one machine.
func names(t *testing.T, prom, machine string) []string {
	t.Helper()
	var out []string
	for i := 0; i < 30; i++ {
		var raw json.RawMessage = promQuery(t, prom, "/api/v1/label/__name__/values",
			url.Values{"match[]": {fmt.Sprintf(`{machine=%q}`, machine)}})
		out = nil
		_ = json.Unmarshal(raw, &out)
		if len(out) > 10 {
			break
		}
		time.Sleep(time.Second)
	}
	sort.Strings(out)
	return out
}

// Every name a proxy pushes must arrive spelled as the spec spells it.
func TestCentralNamesMatchTheSpec(t *testing.T) {
	collector, token, prom := central(t)
	machine := fmt.Sprintf("e2e%029d", time.Now().UnixNano()%1e12)
	push(t, collector, token, machine)
	got := names(t, prom, machine)
	want := []string{
		"llm_cache_miss_cost_usd_total", "llm_cache_misses_total",
		"llm_inference_cost_usd_total", "llm_inference_errors_total", "llm_inference_estimated_cost_usd_total",
		"llm_inference_requests_total", "llm_inference_tokens_total",
		"llm_keepalive_cost_usd_total", "llm_keepalive_pings_total",
		"llm_proxy_component_failures_total", "llm_proxy_fail_open_total",
		"llm_savings_cache_effect_usd_total", "llm_savings_check_failures_total", "llm_savings_interaction_usd_total",
		"llm_savings_requests_total", "llm_savings_saved_tokens_total", "llm_savings_saved_usd_total",
		"llm_savings_added_usd_total", "llm_savings_unpriced_total",
		"llm_inference_duration_seconds_bucket", "llm_inference_ttfb_seconds_bucket", "llm_inference_added_seconds_bucket",
		"llm_proxy_component_duration_seconds_bucket",
	}
	have := map[string]bool{}
	for _, n := range got {
		have[n] = true
		if strings.Contains(n, "_USD") || strings.HasPrefix(n, "otel_") {
			t.Errorf("name %q has a unit suffix or prefix the spec forbids", n)
		}
	}
	for _, n := range want {
		if !have[n] {
			t.Errorf("missing %s\nPrometheus has: %v", n, got)
		}
	}
}

// The collector, not the sender, decides the labels (spec 5.6).
func TestCentralLabelsAreTheSpecSet(t *testing.T) {
	collector, token, prom := central(t)
	machine := fmt.Sprintf("lbl%029d", time.Now().UnixNano()%1e12)
	push(t, collector, token, machine)
	var series []map[string]string
	for i := 0; i < 30 && len(series) == 0; i++ {
		raw := promQuery(t, prom, "/api/v1/series", url.Values{"match[]": {fmt.Sprintf(`llm_inference_tokens_total{machine=%q}`, machine)}})
		_ = json.Unmarshal(raw, &series)
		time.Sleep(time.Second)
	}
	if len(series) == 0 {
		t.Fatal("no series arrived")
	}
	allowed := map[string]bool{"__name__": true, "machine": true, "proxy": true, "preset": true, "service_version": true,
		"model": true, "provider": true, "tier": true}
	for k := range series[0] {
		if !allowed[k] {
			t.Errorf("label %q is not in the spec set", k)
		}
	}
	if series[0]["machine"] != machine || series[0]["proxy"] != "context-guru" {
		t.Errorf("labels = %v", series[0])
	}
}

// A wrong token gets nothing in. Only meaningful when the central checks tokens (CENTRAL_AUTH=on).
func TestCentralRefusesAWrongToken(t *testing.T) {
	if os.Getenv("CENTRAL_AUTH") != "on" {
		t.Skip("authentication is off on the central (rossoctl/context-guru#1130); set CENTRAL_AUTH=on to run this")
	}
	collector, _, _ := central(t)
	req, _ := http.NewRequest(http.MethodPost, collector+"/v1/metrics", strings.NewReader(`{"resourceMetrics":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer not-a-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}
