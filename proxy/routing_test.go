package proxy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/proxy"
	"github.com/rossoctl/context-guru/routing"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
)

func classifierBody() []byte {
	b, _ := json.Marshal(map[string]any{
		"model": "claude-sonnet-5-5", "max_tokens": 64, "stream": false,
		"stop_sequences": []string{"</block>"},
		"system": []map[string]any{
			{"type": "text", "text": "s1"}, {"type": "text", "text": "s2"}, {"type": "text", "text": "s3"}},
		"messages": []map[string]any{
			{"role": "user", "content": "context"}},
	})
	return b
}

type pricer struct{ p modelinfo.Price }

func (pr pricer) Price(context.Context, string) (modelinfo.Price, bool) { return pr.p, true }

func routingHandler(t *testing.T, anthropicUp string, rows []routing.Row, env map[string]string) (*proxy.Handler, *httptest.Server) {
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
	h := proxy.New(pipe, store.NewMemory(store.Options{}), agg, proxy.Options{
		AnthropicUpstream: anthropicUp, OpenAIUpstream: anthropicUp, Mode: components.ModeSync,
		Routing: rows, RoutingEnv: func(k string) string { return env[k] },
		ClassifierBudget: 2 * time.Second,
		Prices:           pricer{modelinfo.Price{Input: 3e-6, Output: 15e-6, CacheRead: 0.3e-6, CacheWrite: 3.75e-6}},
	})
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h.Mux())
	t.Cleanup(srv.Close)
	return h, srv
}

func postAnthropic(t *testing.T, srv *httptest.Server, body []byte) (int, []byte) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/anthropic/v1/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func statsRouting(t *testing.T, srv *httptest.Server) gjson.Result {
	t.Helper()
	resp, err := http.Get(srv.URL + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return gjson.GetBytes(b, "routing.0")
}

var classifierRow = []routing.Row{{Feature: "classifier", ModelNameEnv: "M", KeyEnv: "K", BaseURLEnv: "U",
	Dialect: "bob", PriceInput: 0.1, PriceOutput: 0.4}}

const bobAnswer = `{"choices":[{"message":{"content":"<block>no</block>"},"finish_reason":"stop"}],
"usage":{"prompt_tokens":1000,"completion_tokens":9,"prompt_tokens_details":{"cached_tokens":900}}}`

// With no routing file the request is forwarded exactly as before: same bytes upstream, same
// answer back, no routing section in /stats.
func TestNoRoutingLeavesTheClassifierRequestByteForByte(t *testing.T) {
	up, got := captureUpstream(t)
	_, srv := routingHandler(t, up.URL, nil, nil)
	body := classifierBody()
	code, out := postAnthropic(t, srv, body)
	if code != 200 || string(out) != `{"ok":true}` {
		t.Fatalf("code=%d out=%s", code, out)
	}
	bodies := got()
	if len(bodies) != 1 || !bytes.Equal(bodies[0], body) {
		t.Fatalf("upstream body changed:\n got %s\nwant %s", bodies, body)
	}
	if r := statsRouting(t, srv); r.Exists() {
		t.Fatalf("stats has a routing section: %s", r.Raw)
	}
}

// A row that is inactive (env var unset) is the same as no row.
func TestInactiveRowLeavesTheRequestAlone(t *testing.T) {
	up, got := captureUpstream(t)
	_, srv := routingHandler(t, up.URL, classifierRow, map[string]string{"M": "m"})
	body := classifierBody()
	postAnthropic(t, srv, body)
	if b := got(); len(b) != 1 || !bytes.Equal(b[0], body) {
		t.Fatalf("upstream bodies=%s", b)
	}
	r := statsRouting(t, srv)
	if r.Get("active").Bool() || r.Get("calls").Int() != 0 {
		t.Fatalf("stats=%s", r.Raw)
	}
}

func TestClassifierIsRoutedAndSavingIsPriced(t *testing.T) {
	up, got := captureUpstream(t)
	var gotAuth, gotPath atomic.Value
	bob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		gotPath.Store(r.URL.Path)
		w.Write([]byte(bobAnswer))
	}))
	defer bob.Close()
	env := map[string]string{"M": "bob-fast", "K": "sekret-key", "U": bob.URL} // http://127.0.0.1
	_, srv := routingHandler(t, up.URL, classifierRow, env)

	code, out := postAnthropic(t, srv, classifierBody())
	if code != 200 || gjson.GetBytes(out, "content.0.text").String() != "<block>no" ||
		gjson.GetBytes(out, "stop_reason").String() != "stop_sequence" {
		t.Fatalf("code=%d out=%s", code, out)
	}
	if len(got()) != 0 {
		t.Fatal("the original upstream must not be called when the route answers")
	}
	if gotAuth.Load() != "Bearer sekret-key" || gotPath.Load() != "/inference/v1/chat/completions" {
		t.Fatalf("auth=%v path=%v", gotAuth.Load(), gotPath.Load())
	}
	r := statsRouting(t, srv)
	// 100 fresh, 900 cache read, 9 out. Original (sonnet-class prices above):
	//   100*3e-6 + 900*0.3e-6 + 9*15e-6 = 0.000705. Routed: 100*0.1e-6 + 9*0.4e-6 = 0.0000136.
	want := 0.000705 - 0.0000136
	if r.Get("routed").Int() != 1 || r.Get("calls").Int() != 1 || r.Get("fell_back").Int() != 0 ||
		r.Get("saved_usd").Float() < want-1e-9 || r.Get("saved_usd").Float() > want+1e-9 ||
		r.Get("model").String() != "bob-fast" || r.Get("base_url_host").String() == "" {
		t.Fatalf("stats=%s want saved %.9f", r.Raw, want)
	}
	if strings.Contains(r.Raw, "sekret-key") {
		t.Fatal("key in /stats")
	}
}

func TestInaccessibleRouteFallsBackToTheOriginal(t *testing.T) {
	up, got := captureUpstream(t)
	var calls atomic.Int32
	bob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(503)
	}))
	defer bob.Close()
	_, srv := routingHandler(t, up.URL, classifierRow, map[string]string{"M": "m", "K": "k", "U": bob.URL})
	body := classifierBody()
	code, out := postAnthropic(t, srv, body)
	if code != 200 || string(out) != `{"ok":true}` {
		t.Fatalf("code=%d out=%s", code, out)
	}
	if calls.Load() != 3 {
		t.Fatalf("routed attempts=%d, want 3", calls.Load())
	}
	if b := got(); len(b) != 1 || !bytes.Equal(b[0], body) {
		t.Fatalf("the original request must reach the upstream unchanged: %s", b)
	}
	if r := statsRouting(t, srv); r.Get("fell_back").Int() != 1 || r.Get("routed").Int() != 0 || r.Get("saved_usd").Float() != 0 {
		t.Fatalf("stats=%s", r.Raw)
	}
}

func TestOtherRequestsAreNotDetectedAndNotRouted(t *testing.T) {
	up, got := captureUpstream(t)
	var calls atomic.Int32
	bob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer bob.Close()
	_, srv := routingHandler(t, up.URL, classifierRow, map[string]string{"M": "m", "K": "k", "U": bob.URL})
	near := bytes.Replace(classifierBody(), []byte(`"max_tokens":64`), []byte(`"max_tokens":65`), 1)
	postAnthropic(t, srv, near)
	if calls.Load() != 0 || len(got()) != 1 {
		t.Fatalf("routed calls=%d upstream=%d", calls.Load(), len(got()))
	}
	if r := statsRouting(t, srv); r.Get("not_detected").Int() != 1 || r.Get("calls").Int() != 0 {
		t.Fatalf("stats=%s", r.Raw)
	}
}
