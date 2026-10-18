package proxy

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
)

// A config that still pins a component to its own endpoint with the four retired model: keys
// must build and serve, and the dropped endpoint must never be called. On main these keys were
// valid; refusing them at startup stopped the proxy for anyone who upgraded with such a config.
//
// The cheap-model call goes to the HOST's configured model (Options.CheapModel), which is the
// replacement for those keys. The retired base_url points at a second server that counts every
// request it gets, and the test fails if it gets any.
func TestARetiredModelEndpointBuildsServesAndIsNeverCalled(t *testing.T) {
	var retiredCalls atomic.Int64
	retired := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		retiredCalls.Add(1)
		http.Error(w, "the retired endpoint was called", http.StatusTeapot)
	}))
	defer retired.Close()
	cheap, cheapCalls := fakeCheapModel(t, 10_000, 2_000)
	up := fakeUpstream(t)
	defer up.Close()

	cfg, err := config.LoadBytes([]byte(
		"pipeline: [summarize]\ncomponents:\n  summarize:\n    keep_last: 1\n" +
			"    start_from_message: 0\n    min_tokens: 1\n" +
			"    trigger: {min_request_frac: 0}\n    model:\n      source: config\n" +
			"      provider: anthropic\n      base_url: " + retired.URL + "\n" +
			"      api_key: sk-retired\n      auth: bearer\n"))
	if err != nil {
		t.Fatalf("a config with the retired model keys did not load: %v", err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatalf("a config with the retired model keys did not build: %v", err)
	}
	rec, err := dash.NewRecorder(dash.Options{
		DBPath: filepath.Join(t.TempDir(), "d.db"), BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{
		AnthropicUpstream: up.URL,
		Dashboard:         rec,
		Prices:            fixedPricer{},
		CheapModel: &CheapModelConfig{Provider: "anthropic", BaseURL: cheap.URL,
			APIKey: "host-key", Model: "m"},
	})
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	postChat(t, srv, "sess-retired")
	waitForRows(t, rec, 1)
	if !offload.WaitForAllSummariesForTest(5 * time.Second) {
		t.Fatal("the summary never finished")
	}
	if cheapCalls.Load() == 0 {
		t.Fatal("the summarizer never called the host's cheap model; the check below would be vacuous")
	}
	if n := retiredCalls.Load(); n != 0 {
		t.Errorf("the dropped model.base_url was called %d times", n)
	}
}
