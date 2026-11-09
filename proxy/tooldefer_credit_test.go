package proxy

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/tooldefer"
)

// #908: tool deferral is reported to the accountant, and Overview and /api/tool-defer show one figure.
func TestToolDeferIsCreditedFromTheLedgerOnly(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(b), "cg_probe_") {
			fmt.Fprint(w, `{"content":[],"usage":{"input_tokens":10,"output_tokens":5}}`)
			return
		}
		fmt.Fprintf(w, `{"id":"m1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],`+
			`"stop_reason":"end_turn","usage":{"input_tokens":50,"output_tokens":5,"cache_read_input_tokens":%d}}`, len(b)/4)
	}))
	defer up.Close()
	h, rec, tap := accHandlerOpts(t, up.URL, "preset: ultrahigh\n", func(o *Options) {
		o.ToolDeferPath = filepath.Join(t.TempDir(), tooldefer.FileName)
		o.Prices = tdPrices{}
	})
	ds := h.toolDefer()
	for i, s := range []string{"p1", "p2", "p3"} {
		ds.hist.Observe("", s, "/w", tdBody(), int64(i+1))
	}
	ds.prober.Seed(tooldefer.ProbeKey{Upstream: up.URL, Model: "claude-sonnet-5-5", Route: "/v1/messages"}, tooldefer.Pass, time.Now())
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	for i := 0; i < 3; i++ {
		resp, out := tdPost(t, srv, tdBody())
		if resp.StatusCode != 200 {
			t.Fatal(out)
		}
	}
	waitForRows(t, rec, 3)
	for _, e := range tap.all() {
		if fb := e.AccountantFallback(); fb != "" {
			t.Fatalf("fell back: %q", fb)
		}
		for _, c := range e.Components {
			if c.Component == "tooldefer" && c.SavedUSD != 0 {
				t.Errorf("%s books %v: the deferral ledger owns deferral's money, not the accountant", c.Component, c.SavedUSD)
			}
		}
	}
	o, err := rec.DB().Overview(dash.Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := rec.DB().ToolDeferReportFor(dash.Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total.NetUSD <= 0 || math.Abs(o.DeferNetUSD-rep.Total.NetUSD) > 1e-12 {
		t.Errorf("Overview defer %v, /api/tool-defer net %v: want one positive figure", o.DeferNetUSD, rep.Total.NetUSD)
	}
	// The accountant sees only that deferral added bytes. Its cost must not sit in the net beside the
	// ledger's, which already subtracts the names list.
	if want := o.NetSavedUSD + o.CachesplitSavedUSD + o.KeepAliveNetUSD + o.ManualPings.SavedUSD + o.DeferNetUSD; math.Abs(o.TotalSavedUSD-want) > 1e-12 {
		t.Errorf("total %v, want %v", o.TotalSavedUSD, want)
	}
}
