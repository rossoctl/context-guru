package proxy

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/tooldefer"
)

// R56 (accountant#61): the accountant is given the prompt the provider BILLS. A tool deferred with
// defer_loading stays in the bytes the proxy sends, and the provider bills no definition for it, so the
// accountant's copy of the forwarded body carries a stub in its place. Deferral is then an ordinary
// shrink of `tools` positions, and the library prices it (#908).
func TestToolDeferIsPricedByTheLibraryFromAStubSizedBody(t *testing.T) {
	var mu sync.Mutex
	var sent [][]byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(b), "cg_probe_") {
			fmt.Fprint(w, `{"content":[],"usage":{"input_tokens":10,"output_tokens":5}}`)
			return
		}
		mu.Lock()
		sent = append(sent, b)
		mu.Unlock()
		// The provider bills the prompt without the deferred definitions: about 1 token in 4 bytes of what
		// is NOT deferred. A cache read of the whole prompt after the first request.
		billed := len(billedBytesOf(b)) / 4
		fmt.Fprintf(w, `{"id":"m1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],`+
			`"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":5,"cache_read_input_tokens":%d}}`, billed)
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
		if resp, out := tdPost(t, srv, tdBody()); resp.StatusCode != 200 {
			t.Fatal(out)
		}
	}
	waitForRows(t, rec, 3)

	// The provider got the real bytes: every deferred definition is still there, marked defer_loading.
	mu.Lock()
	last := sent[len(sent)-1]
	mu.Unlock()
	deferred := 0
	gjson.GetBytes(last, "tools").ForEach(func(_, tl gjson.Result) bool {
		if tl.Get("defer_loading").Bool() {
			deferred++
			if !strings.Contains(tl.Raw, "Detailed usage notes") {
				t.Errorf("a deferred definition was shortened on the wire: %s", tl.Raw)
			}
		}
		return true
	})
	if deferred == 0 {
		t.Fatal("nothing was deferred: the test does not exercise deferral")
	}

	var rowSum float64
	for i, e := range tap.all() {
		if fb := e.AccountantFallback(); fb != "" {
			t.Fatalf("request %d fell back: %q", i, fb)
		}
		var def float64
		for _, c := range e.Components {
			if c.Component == "tooldefer" {
				def = c.SavedUSD
			}
		}
		if i > 0 && def <= 0 {
			t.Errorf("request %d: the library books %v for tool deferral, want a saving", i, def)
		}
		rowSum += e.BaselineCostUSD - e.CostUSD
	}

	// Deferral is counted once. The library's figure is in the baseline delta; the lever beside it is the
	// search cost alone, which these requests do not have (the fake provider runs no search).
	o, err := rec.DB().Overview(dash.Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if o.DeferNetUSD != 0 {
		t.Errorf("deferral lever %v, want 0: no search ran, and the library's figure is already in the net", o.DeferNetUSD)
	}
	if math.Abs(o.NetSavedUSD-rowSum) > 1e-12 || math.Abs(o.TotalSavedUSD-o.NetSavedUSD) > 1e-12 {
		t.Errorf("net %v, total %v, want both %v: deferral counted once", o.NetSavedUSD, o.TotalSavedUSD, rowSum)
	}
	rep, err := rec.DB().ToolDeferReportFor(dash.Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(rep.Total.NetUSD-o.DeferNetUSD) > 1e-12 {
		t.Errorf("/api/tool-defer net %v, Overview lever %v: want one figure", rep.Total.NetUSD, o.DeferNetUSD)
	}
}

// billedBytesOf is the size, in bytes, of the prompt the provider bills: the whole body less the
// definitions of the tools it deferred.
func billedBytesOf(b []byte) []byte {
	out := b
	gjson.GetBytes(b, "tools").ForEach(func(_, tl gjson.Result) bool {
		if tl.Get("defer_loading").Bool() {
			out = bytes.Replace(out, []byte(tl.Raw), nil, 1)
		}
		return true
	})
	return out
}

// Models that ignore defer_loading get our own search tool instead (clientsearch.go). There the deferred
// definitions are really removed from the body, so no stub is built and the accountant records the forwarded
// body as it is (R56: "nothing in R56 applies"). The deferral must still be priced, from the real shrink, with
// no fallback, and with the bytes sent upstream untouched.
func TestClientSearchDeferralKeepsTheRealShrinkAndAddsNoFallback(t *testing.T) {
	var sentBodies [][]byte
	var mu sync.Mutex
	us := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		sentBodies = append(sentBodies, b)
		mu.Unlock()
		n := len(b) / 4
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"done"},`+
			`"finish_reason":"stop"}],"usage":{"prompt_tokens":%d,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":%d}}}`, n, n*8/10)
	}))
	defer us.Close()
	h, rec, tap := accHandlerOpts(t, us.URL, "preset: ultrahigh\n", func(o *Options) {
		o.ToolDeferPath = filepath.Join(t.TempDir(), tooldefer.FileName)
		o.Prices = tdPrices{}
	})
	ds := h.toolDefer()
	for i, s := range []string{"p1", "p2", "p3"} {
		ds.hist.Observe("", s, "", tooldefer.View("openai", csBody(s)), int64(i+1))
	}
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	for i := 0; i < 2; i++ {
		if st, out := csPost(t, srv, "s1", csBody("s1")); st != http.StatusOK {
			t.Fatal(out)
		}
	}
	waitForRows(t, rec, 2)
	mu.Lock()
	for i, b := range sentBodies {
		if bytes.Contains(b, []byte("defer_loading")) {
			t.Errorf("upstream body %d carries defer_loading on the client-search wire", i)
		}
		if got := csTools(b); !strings.Contains(got, tooldefer.ClientSearchName) {
			t.Errorf("upstream body %d has no search tool: %s", i, got)
		}
	}
	mu.Unlock()
	// The accountant cannot price this wire today: Chat Completions usage carries no cache-write tier, so the
	// event falls back with no_usage, with or without deferral (checked on the branch before this change). What
	// this test pins is that deferral adds no reason of its own to that: the body is recorded as it was sent.
	for i, e := range tap.all() {
		if fb := e.AccountantFallback(); fb != "" && fb != dash.FallbackUnpriced {
			t.Errorf("request %d fell back with %q, want only the existing unpriced (no_usage) reason", i, fb)
		}
	}
}

// A stub that is not smaller than the definition it replaces is no shrink the provider billed less for, so billedBody
// refuses the whole body and the caller keeps the forwarded one.
func TestBilledBodyRefusesAStubThatWouldGrowTheTool(t *testing.T) {
	body := []byte(`{"model":"m","tools":[{"name":"a","description":"x"},{"name":"b","defer_loading":true}],` +
		`"messages":[{"role":"user","content":"hi"}]}`)
	billed, ok := billedBody(body, deferredNames(body))
	if ok || !bytes.Equal(billed, body) {
		t.Errorf("ok=%v changed=%v: a stub as large as the definition must leave the body alone", ok, !bytes.Equal(billed, body))
	}
}

func TestBilledBodyStubsEveryDeferredToolAndKeepsTheOthers(t *testing.T) {
	long := strings.Repeat("Detailed usage notes. ", 20)
	body := []byte(`{"model":"m","tools":[{"name":"keep","description":"` + long + `"},` +
		`{"name":"a","description":"` + long + `","defer_loading":true},{"name":"b","description":"` + long + `","defer_loading":true}],` +
		`"messages":[{"role":"user","content":"hi"}]}`)
	billed, ok := billedBody(body, deferredNames(body))
	if !ok {
		t.Fatal("not stubbed")
	}
	if got := gjson.GetBytes(billed, "tools.0").Raw; got != gjson.GetBytes(body, "tools.0").Raw {
		t.Errorf("a loaded tool changed: %s", got)
	}
	for i, name := range []string{"a", "b"} {
		if got := gjson.GetBytes(billed, "tools."+strconv.Itoa(i+1)).Raw; got != `{"name":"`+name+`","defer_loading":true}` {
			t.Errorf("stub %d = %s", i+1, got)
		}
	}
	if !bytes.Contains(body, []byte(long)) {
		t.Error("the input body was changed")
	}
}

func TestBilledBodyLeavesABodyWithNoDeferralAlone(t *testing.T) {
	body := []byte(`{"model":"m","tools":[{"name":"a","description":"x"}],"messages":[]}`)
	billed, ok := billedBody(body, deferredNames(body))
	if ok || !bytes.Equal(billed, body) {
		t.Errorf("ok=%v, body changed=%v: want the body as it was", ok, !bytes.Equal(billed, body))
	}
}

// accBilled gives the accountant the body as it was, and names the reason, when a deferral cannot be described to
// the library. Deferral's own rule keeps a stub that cannot shrink out of a real request, so this is the only test
// of that guard.
func TestAccBilledKeepsTheBodyAndNamesTheReasonWhenAStubCannotShrink(t *testing.T) {
	body := []byte(`{"model":"m","tools":[{"name":"b","defer_loading":true}],"messages":[{"role":"user","content":"hi"}]}`)
	c := &capture{accPreDefer: []byte(`{}`)}
	if got := c.accBilled(body); !bytes.Equal(got, body) {
		t.Errorf("body changed: %s", got)
	}
	if c.accNoResult != accDeferralUnplaced {
		t.Errorf("reason = %q, want %q", c.accNoResult, accDeferralUnplaced)
	}
	// A request deferral did not touch is left alone, with no reason.
	c = &capture{}
	if got := c.accBilled(body); !bytes.Equal(got, body) || c.accNoResult != "" {
		t.Errorf("an unchanged request got reason %q", c.accNoResult)
	}
}
