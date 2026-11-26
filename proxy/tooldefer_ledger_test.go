package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/testutil"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
	"github.com/rossoctl/context-guru/thread"
	"github.com/rossoctl/context-guru/tooldefer"
	"github.com/tidwall/gjson"
)

// sseUpstream streams one assistant turn as events: a server-side search that loads Monitor, then
// text. It answers a probe as JSON.
type sseUpstream struct{}

func (sseUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	if probe := gjson.GetBytes(b, "tools.1.name").String(); strings.HasPrefix(probe, "cg_probe_") {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"content":[{"type":"tool_search_tool_result","tool_use_id":"s","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":%q}]}},`+
			`{"type":"tool_use","id":"t","name":%q,"input":{"code":7}}],"usage":{"input_tokens":10,"output_tokens":5}}`, probe, probe)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	ev := func(typ, data string) { fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, data) }
	ev("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-sonnet-5-5","usage":{"input_tokens":5,"cache_read_input_tokens":3000,"output_tokens":1}}}`)
	ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srvtoolu_1","name":"tool_search_tool_bm25","input":{}}}`)
	ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"Monitor\",\"limit\":1}"}}`)
	ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
	ev("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_1","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"Monitor"}]}}}`)
	ev("content_block_stop", `{"type":"content_block_stop","index":1}`)
	ev("content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`)
	ev("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"ok"}}`)
	ev("content_block_stop", `{"type":"content_block_stop","index":2}`)
	ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":20}}`)
	ev("message_stop", `{"type":"message_stop"}`)
}

// #871 defect 2. A streamed turn that takes the SSE splice path (the request advertises the
// expand tool, as ultrahigh's does) counts its search in the ledger, and so does the
// stream-through path (a request without the proxy's own tools).
func TestToolDeferLedgerCountsSearchesOnEveryPath(t *testing.T) {
	us := httptest.NewServer(sseUpstream{})
	defer us.Close()
	for _, tc := range []struct {
		name, doc, inject string
	}{
		{"splice", "preset: ultrahigh\n", ""},
		// No offloader and no sweep: the request carries no proxy tool, so the stream goes through.
		{"stream-through", "pipeline: [toolfilter]\ncomponents:\n  toolfilter:\n    defer: auto\n", "never"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _ := config.LoadBytes([]byte(tc.doc))
			agg := metrics.NewAggregator()
			pipe, _ := cfg.Build(agg)
			h := New(pipe, store.NewMemory(store.Options{}), agg, Options{AnthropicUpstream: us.URL,
				ToolDeferPath: filepath.Join(t.TempDir(), tooldefer.FileName), Prices: tdPrices{}, InjectExpand: tc.inject})
			defer h.Close()
			ds := h.toolDefer()
			for i, s := range []string{"p1", "p2", "p3"} {
				ds.hist.Observe("", s, "/w", tdBody(), int64(i+1))
			}
			ds.prober.Seed(tooldefer.ProbeKey{Upstream: us.URL, Model: "claude-sonnet-5-5", Route: "/v1/messages"}, tooldefer.Pass, time.Now())
			srv := httptest.NewServer(h.Mux())
			defer srv.Close()
			b := bytes.Replace(tdBody(), []byte(`"max_tokens":64,`), []byte(`"max_tokens":64,"stream":true,`), 1)
			// Two requests of one thread. The first has no history, so the accountant holds it: unpriced, no dollars. The
			// second is priced, and /stats shows its figure with the dashboard off (#1451, #1441).
			for i := 0; i < 2; i++ {
				resp, out := tdPost(t, srv, b)
				if resp.StatusCode != http.StatusOK || !strings.Contains(out, "tool_search_tool_result") {
					t.Fatalf("status %d: %s", resp.StatusCode, out)
				}
			}
			// The dollars are booked off the handler path, after the accountant job: wait for the condition, not a snapshot.
			waitLedger(t, ds, func(st tooldefer.Stats) bool { return st.Unpriced >= 1 && st.Net != 0 })
			st := ds.ledger.Snapshot()
			if st.Requests != 2 || st.Searches != 2 {
				t.Fatalf("ledger = %+v, want 2 requests with 1 search each", st)
			}
			if st.Unpriced != 1 || st.Net == 0 || st.Gross <= 0 || math.Abs(st.Gross-st.Cost-st.Net) > 1e-12 {
				t.Fatalf("ledger = %+v, want 1 held (unpriced) request and the second one's figure, with the dashboard off", st)
			}
		})
	}
}

// #869: the request deferral changed lands on its dashboard row with the ledger's facts, so the
// net-saving panel reads the same figures /stats does. The streamed turn's search loads Monitor,
// which deferral had deferred and nothing had loaded before: one wrong guess.
func TestToolDeferFactsReachTheDashboardRow(t *testing.T) {
	us := httptest.NewServer(sseUpstream{})
	defer us.Close()
	cfg, _ := config.LoadBytes([]byte("pipeline: [toolfilter]\ncomponents:\n  toolfilter:\n    defer: auto\n"))
	agg := metrics.NewAggregator()
	pipe, _ := cfg.Build(agg)
	rec, err := dash.NewRecorder(dash.Options{DBPath: t.TempDir() + "/d.db", BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{AnthropicUpstream: us.URL, Dashboard: rec,
		ToolDeferPath: filepath.Join(t.TempDir(), tooldefer.FileName), Prices: tdPrices{}, InjectExpand: "never"})
	defer h.Close()
	ds := h.toolDefer()
	for i, s := range []string{"p1", "p2", "p3"} {
		ds.hist.Observe("", s, "/w", tdBody(), int64(i+1))
	}
	ds.prober.Seed(tooldefer.ProbeKey{Upstream: us.URL, Model: "claude-sonnet-5-5", Route: "/v1/messages"}, tooldefer.Pass, time.Now())
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	b := bytes.Replace(tdBody(), []byte(`"max_tokens":64,`), []byte(`"max_tokens":64,"stream":true,`), 1)
	if resp, out := tdPost(t, srv, b); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, out)
	}
	waitForRows(t, rec, 1)
	page, _ := rec.DB().Requests(dash.Filter{TenantAll: true}, 0, 10)
	if len(page.Requests) != 1 {
		t.Fatalf("rows = %d, want 1", len(page.Requests))
	}
	e := page.Requests[0]
	st := ds.ledger.Snapshot()
	if e.DeferTokens <= 0 || int64(e.DeferTokens) != st.DeferredTokens || e.DeferListTokens <= 0 {
		t.Errorf("row deferred %d tokens (list %d), ledger %d", e.DeferTokens, e.DeferListTokens, st.DeferredTokens)
	}
	if e.DeferSearches != 1 || e.DeferLoaded != 1 || e.DeferWrongGuesses != 1 || e.DeferDroppedSearches != 0 {
		t.Errorf("row searches %d, loaded %d, wrong %d, dropped %d; want 1, 1, 1, 0", e.DeferSearches, e.DeferLoaded,
			e.DeferWrongGuesses, e.DeferDroppedSearches)
	}
	if int64(e.DeferWrongGuesses) != st.WrongGuesses || int64(e.DeferDroppedSearches) != st.DroppedSearches {
		t.Errorf("row wrong %d dropped %d, ledger %d and %d", e.DeferWrongGuesses, e.DeferDroppedSearches, st.WrongGuesses, st.DroppedSearches)
	}
	// One figure (#1441): the row and /stats hold the same dollars, taken from the accountant. This request is the
	// first of its thread: no history, so the library holds it, and a held request books no lever, on the row or in
	// the ledger. The dollars join the held estimate with every other component's.
	if !e.EstimateHeld {
		t.Fatalf("expected a held first request, got fallback %q", e.AccountantFallback())
	}
	if e.DeferGrossUSD != 0 || e.DeferNetUSD != 0 || st.Gross != 0 || st.Net != 0 || st.Unpriced != 1 {
		t.Errorf("a held request must book no lever: row gross %v net %v, ledger %+v", e.DeferGrossUSD, e.DeferNetUSD, st)
	}
}

// The priced path of the same fact: a second request of the thread has history, so the accountant prices it. Its
// deferral figure is the library's tooldefer row, search turns included (#1441): the row, /stats and the component
// figure show one number. The search turns are three spend operations on the tooldefer operator.
func TestToolDeferPricedRowIsOneFigureWithTheSearchTurns(t *testing.T) {
	us := httptest.NewServer(sseUpstream{})
	defer us.Close()
	h, rec, tap := accHandlerOpts(t, us.URL, "pipeline: [toolfilter]\ncomponents:\n  toolfilter:\n    defer: auto\n", func(o *Options) {
		o.ToolDeferPath = filepath.Join(t.TempDir(), tooldefer.FileName)
		o.Prices = tdPrices{}
		o.InjectExpand = "never"
	})
	ds := h.toolDefer()
	for i, s := range []string{"p1", "p2", "p3"} {
		ds.hist.Observe("", s, "/w", tdBody(), int64(i+1))
	}
	ds.prober.Seed(tooldefer.ProbeKey{Upstream: us.URL, Model: "claude-sonnet-5-5", Route: "/v1/messages"}, tooldefer.Pass, time.Now())
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	b := bytes.Replace(tdBody(), []byte(`"max_tokens":64,`), []byte(`"max_tokens":64,"stream":true,`), 1)
	for i := 0; i < 2; i++ {
		if resp, out := tdPost(t, srv, b); resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status %d: %s", i, resp.StatusCode, out)
		}
	}
	waitForRows(t, rec, 2)
	var priced int
	var netSum, grossSum float64
	for i, e := range tap.all() {
		if e.DeferSearches == 0 {
			t.Fatalf("request %d ran no search: the test does not exercise the search cost", i)
		}
		netSum += e.DeferNetUSD
		grossSum += e.DeferGrossUSD
		if e.EstimateHeld || e.AccountantFallback() != "" {
			continue
		}
		priced++
		var lib float64
		for _, c := range e.Components {
			if c.Component == "tooldefer" {
				lib = c.SavedUSD
			}
		}
		// The panel's net is the component figure, exactly.
		if lib == 0 || math.Abs(e.DeferNetUSD-lib) > 1e-12 {
			t.Errorf("request %d: row net %v, component figure %v: want one figure", i, e.DeferNetUSD, lib)
		}
		if math.Abs(e.DeferGrossUSD-e.DeferNetUSD-e.DeferCostUSD) > 1e-12 || e.DeferCostUSD <= 0 {
			t.Errorf("request %d: gross %v, cost %v, net %v: cost must be gross less net and positive", i, e.DeferGrossUSD, e.DeferCostUSD, e.DeferNetUSD)
		}
		// The search turns are in the figure: dropping the three spend operations raises it by their cost.
		res, _ := e.AccountantResult()
		var spends int
		for _, o := range res.Ops {
			if o.Call != nil && o.Operator == "context-guru.tooldefer" {
				spends++
			}
		}
		if spends != 3 {
			t.Errorf("request %d: %d tooldefer spend operations, want 3 (output, second pass, next write)", i, spends)
		}
	}
	if priced != 1 {
		t.Fatalf("priced requests = %d, want 1 (the second of the thread): the test must reach the priced path", priced)
	}
	// /stats holds the sum of the rows, and /api/tool-defer too.
	st := ds.ledger.Snapshot()
	if st.Requests != 2 || math.Abs(st.Net-netSum) > 1e-12 || math.Abs(st.Gross-grossSum) > 1e-12 {
		t.Errorf("/stats ledger %+v, want net %v gross %v from the rows", st, netSum, grossSum)
	}
	rep, err := rec.DB().ToolDeferReportFor(dash.Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(rep.Total.NetUSD-st.Net) > 1e-12 || math.Abs(rep.Total.GrossUSD-st.Gross) > 1e-12 || math.Abs(rep.Total.CostUSD-st.Cost) > 1e-12 {
		t.Errorf("/api/tool-defer total %+v differs from /stats %+v", rep.Total, st.Money)
	}
}

// waitLedger waits until cond holds for the ledger's total. The dollars are booked after the accountant job, off the
// handler path (#1441), so a snapshot right after the response may come first.
func waitLedger(t *testing.T, ds *deferState, cond func(tooldefer.Stats) bool) {
	t.Helper()
	testutil.WaitFor(t, "the ledger to reach the wanted state", func() bool { return cond(ds.ledger.Snapshot()) })
}

// The ledger's dollars join the accountant, so they are booked off the handler path: deferLedger runs in serve's
// terminal defer and must not wait on the accountant job (#1059, #1456). The facts are booked at once. The dollars
// follow when finishAccountant runs, after the job.
func TestDeferLedgerDoesNotWaitForTheAccountantJob(t *testing.T) {
	h := tdHandler(t, "ultrahigh", "http://x")
	defer h.Close()
	c := asyncCapture()
	c.model, c.pricer = "claude-sonnet-5-5", tdPrices{}
	release := make(chan struct{})
	rel := sync.OnceFunc(func() { close(release) })
	t.Cleanup(rel) // a failed wait must not leak the blocked job
	c.savings.onEnd = func(_ *accountant.Request, _ endRecord) { <-release }
	body := manyToolResultsRequest("claude-sonnet-5", 4)
	c.accBegin(asyncMeta(), body)
	c.accEnd(thread.Result{ID: thread.Primary}, body, nil)

	di := &tooldefer.Info{Tenant: "t", Model: "claude-sonnet-5-5", Outcome: tooldefer.Outcome{DeferredTokens: 1000, ListTokens: 100}}
	returned := make(chan struct{})
	go func() {
		h.deferLedger(context.Background(), c, di, Usage{CacheRead: 5000, Output: 5}, true, searchTally{})
		close(returned)
	}()
	testutil.WaitFor(t, "deferLedger to return without the accountant job (it must not wait on it)", func() bool {
		select {
		case <-returned:
			return true
		default:
			return false
		}
	})
	st := h.toolDefer().ledger.Snapshot()
	if st.Requests != 1 || st.Unpriced != 0 || st.Net != 0 {
		rel()
		t.Fatalf("ledger after deferLedger = %+v, want the counts only: the dollars wait for the job", st)
	}
	rel()
	c.finishAccountant(Usage{CacheRead: 5000, Output: 5}, true) // the job's own goroutine, after the join
	if st = h.toolDefer().ledger.Snapshot(); st.Unpriced+boolInt(st.Net != 0) != 1 {
		t.Fatalf("ledger after the job = %+v, want the request booked once, priced or unpriced", st)
	}
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// #1465: /stats gates with accountant.Withhold, like the dashboard row and the metrics push. A sum failure that no
// operation claims (Check.Claims 0) still books a figure, and a claimed one is withheld.
func TestAccFigureGatesSumFailuresWithWithhold(t *testing.T) {
	zero, one := 0, 1
	op := accountant.PricedOp{OpID: "r", Kind: accountant.KindRewrite, Operator: "context-guru.tooldefer", ChangeID: "c",
		Saved: accountant.TierTokens{CacheRead: 5000}}
	for name, tc := range map[string]struct {
		claims *int
		want   bool
	}{"unclaimed": {&zero, true}, "claimed": {&one, false}} {
		t.Run(name, func(t *testing.T) {
			res := accountant.Result{Ops: []accountant.PricedOp{op},
				Checks: []accountant.CheckFailure{{Check: accountant.CheckSum, Section: accountant.SectionMessages, Claims: tc.claims}}}
			c := &capture{model: "claude-sonnet-5-5", pricer: tdPrices{}, accRes: &res}
			c.trace.Run = &components.RunReport{TokensBefore: 1000}
			_, ok := c.accFigure(context.Background(), Usage{CacheRead: 5000}, true)
			if ok != tc.want {
				t.Fatalf("accFigure ok = %v, want %v", ok, tc.want)
			}
		})
	}
}
