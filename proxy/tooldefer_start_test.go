package proxy

import (
	"bytes"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
	"github.com/rossoctl/context-guru/tooldefer"
)

// #871 defect 3. A restart with a stored Pass verdict: the state is built at start, so the very
// first request finds the verdict and defers.
func TestToolDeferFirstRequestAfterStartDefers(t *testing.T) {
	up := &tdUpstream{}
	us := httptest.NewServer(up)
	defer us.Close()
	path := filepath.Join(t.TempDir(), tooldefer.FileName)
	h := tdHandlerAt(t, "ultrahigh", us.URL, path)
	srv := httptest.NewServer(h.Mux())
	tdPost(t, srv, tdBody()) // runs the probe, which stores Pass; observes the history
	srv.Close()
	h.Close() // flushes the history and the verdict

	cfg, _ := config.LoadBytes([]byte("preset: ultrahigh\n"))
	agg := metrics.NewAggregator()
	pipe, _ := cfg.Build(agg)
	h2 := New(pipe, store.NewMemory(store.Options{}), agg, Options{AnthropicUpstream: us.URL, ToolDeferPath: path, Prices: tdPrices{}})
	defer h2.Close()
	ds, _ := h2.deferPeek()
	if ds == nil || !ds.hist.Ready() {
		t.Fatal("the deferral state is not built and loaded at start")
	}
	if h2.deferStats() == nil {
		t.Fatal("/stats has no tool_defer block before the first request")
	}
	k := tooldefer.ProbeKey{Upstream: us.URL, Model: "claude-sonnet-5-5", Route: "/v1/messages"}
	if ds.prober.Verdict(k) != tooldefer.Pass {
		t.Fatal("the stored Pass verdict is not loaded at start")
	}
	// The history (the fixture's rows are too old for the file's retention, so seed it again). No
	// probe may run: the first request must use the stored verdict.
	for i, s := range []string{"p1", "p2", "p3"} {
		ds.hist.Observe("", s, "/w", tdBody(), time.Now().UnixMilli()+int64(i))
	}
	ds.prober.Go = func(func()) { t.Error("a probe ran on the first request after the start") }
	srv2 := httptest.NewServer(h2.Mux())
	defer srv2.Close()
	tdPost(t, srv2, bytes.Replace(tdBody(), []byte("sess-1"), []byte("sess-new"), 1))
	if got := tdTools(up.last()); !strings.Contains(got, "*") {
		t.Fatalf("the first request after a start deferred nothing: %s", got)
	}

	// xhigh builds nothing at start.
	cx, _ := config.LoadBytes([]byte("preset: xhigh\n"))
	px, _ := cx.Build(agg)
	h3 := New(px, store.NewMemory(store.Options{}), agg, Options{AnthropicUpstream: us.URL, ToolDeferPath: path, Prices: tdPrices{}})
	defer h3.Close()
	if ds, _ := h3.deferPeek(); ds != nil {
		t.Fatal("xhigh built the deferral state at start")
	}
}

func TestDocDefers(t *testing.T) {
	for doc, want := range map[string]bool{
		"preset: ultrahigh\n": true,
		"preset: xhigh\n":     false,
		"pipeline: [toolfilter]\ncomponents:\n  toolfilter:\n    defer: auto\n": true,
		"pipeline: [format]\ncomponents:\n  toolfilter:\n    defer: auto\n":     false,
		"not: [yaml": false,
		"":           false,
	} {
		if got := docDefers(doc); got != want {
			t.Errorf("docDefers(%q) = %v, want %v", doc, got, want)
		}
	}
}
