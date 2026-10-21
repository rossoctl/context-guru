package proxy

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dispatch"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/routing"
	"github.com/rossoctl/context-guru/store"
)

type routePricer struct{ p modelinfo.Price }

func (pr routePricer) Price(context.Context, string) (modelinfo.Price, bool) { return pr.p, true }

var exampleRow = []routing.Row{{Feature: "example_feature", ModelNameEnv: "M", KeyEnv: "K",
	BaseURLEnv: "U", Dialect: "bob", PriceInput: 0.1, PriceOutput: 0.4}}

func routingProxy(t *testing.T, rows []routing.Row, env map[string]string) (*Handler, *httptest.Server) {
	t.Helper()
	cfg, err := config.LoadBytes([]byte("pipeline: [dedup]\n"))
	if err != nil {
		t.Fatal(err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{
		AnthropicUpstream: "http://127.0.0.1:1", OpenAIUpstream: "http://127.0.0.1:1",
		Mode: components.ModeSync, Routing: rows,
		RoutingEnv: func(k string) string { return env[k] },
		Prices:     routePricer{modelinfo.Price{Input: 3e-6, Output: 15e-6, CacheRead: 0.3e-6, CacheWrite: 3.75e-6}},
	})
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h.Mux())
	t.Cleanup(srv.Close)
	return h, srv
}

func routingStats(t *testing.T, srv *httptest.Server) (string, gjson.Result) {
	t.Helper()
	resp, err := http.Get(srv.URL + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), gjson.GetBytes(b, "routing.0")
}

// With no routing file there is no table, no lookup, and no routing section in /stats.
func TestNoRoutingFileChangesNothing(t *testing.T) {
	h, srv := routingProxy(t, nil, nil)
	if h.routes != nil {
		t.Fatal("a table was built without rows")
	}
	if _, ok := h.routes.Lookup("example_feature"); ok {
		t.Fatal("lookup answered without a row")
	}
	if _, r := routingStats(t, srv); r.Exists() {
		t.Fatalf("stats has a routing section: %s", r.Raw)
	}
}

// An inactive row (env var unset) is listed in /stats, and Lookup says no.
func TestInactiveRowIsListedAndNotLookedUp(t *testing.T) {
	h, srv := routingProxy(t, exampleRow, map[string]string{"M": "m"})
	if _, ok := h.routes.Lookup("example_feature"); ok {
		t.Fatal("an inactive row answered a lookup")
	}
	_, r := routingStats(t, srv)
	if r.Get("active").Bool() || r.Get("reason").String() == "" {
		t.Fatalf("stats=%s", r.Raw)
	}
}

// An active row is pinned on the dispatcher: the feature's Ref reaches the row's endpoint with the
// row's credential, and /stats shows the row without the key.
func TestActiveRowIsPinnedAndShownWithoutTheKey(t *testing.T) {
	var gotAuth, gotPath atomic.Value
	end := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		gotPath.Store(r.URL.Path)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer end.Close()
	h, srv := routingProxy(t, exampleRow, map[string]string{"M": "bob-fast", "K": "sekret-key", "U": end.URL})

	route, ok := h.routes.Lookup("example_feature")
	if !ok || route.Model != "bob-fast" || route.Ref == "" ||
		route.Path != "/inference/v1/chat/completions" || route.Dialect != "bob" {
		t.Fatalf("route=%+v ok=%v", route, ok)
	}
	if math.Abs(route.Price.Input-0.1e-6) > 1e-15 || math.Abs(route.Price.Output-0.4e-6) > 1e-15 {
		t.Fatalf("price=%+v", route.Price)
	}
	resp, err := h.dispatcher().Do(context.Background(), dispatch.Request{
		Purpose: dispatch.PurposeRouted, Ref: route.Ref, Path: route.Path, Body: []byte(`{}`)})
	if err != nil || resp.Status != 200 {
		t.Fatalf("do: status=%d err=%v", resp.Status, err)
	}
	if gotAuth.Load() != "Bearer sekret-key" || gotPath.Load() != "/inference/v1/chat/completions" {
		t.Fatalf("auth=%v path=%v", gotAuth.Load(), gotPath.Load())
	}

	h.routes.Record("example_feature", routing.Routed, 0.25)
	h.routes.Record("example_feature", routing.FellBack, 0)
	h.routes.Record("example_feature", routing.NotDetected, 0)
	raw, r := routingStats(t, srv)
	if !r.Get("active").Bool() || r.Get("model").String() != "bob-fast" ||
		r.Get("base_url_host").String() == "" || r.Get("calls").Int() != 2 ||
		r.Get("routed").Int() != 1 || r.Get("fell_back").Int() != 1 ||
		r.Get("not_detected").Int() != 1 || r.Get("saved_usd").Float() != 0.25 {
		t.Fatalf("stats=%s", r.Raw)
	}
	if strings.Contains(raw, "sekret-key") {
		t.Fatal("key in /stats")
	}
}

// saved_usd = cost at the original model's price - cost at the row's price, on the same counts.
func TestRoutedSavingIsPriced(t *testing.T) {
	h, _ := routingProxy(t, nil, nil)
	routed := dispatch.Price{Input: 0.1e-6, Output: 0.4e-6}
	u := RoutedUsage{Fresh: 100, CacheRead: 900, Output: 9}
	// Original: 100*3e-6 + 900*0.3e-6 + 9*15e-6 = 0.000705. Routed: 100*0.1e-6 + 9*0.4e-6 = 0.0000136.
	want := 0.000705 - 0.0000136
	if got := h.routedSaving(context.Background(), "claude-sonnet-5-5", u, routed); math.Abs(got-want) > 1e-9 {
		t.Fatalf("saving=%.9f want %.9f", got, want)
	}
	if got := h.routedSaving(context.Background(), "", u, routed); got != 0 {
		t.Fatalf("no original model: saving=%v, want 0", got)
	}
}
