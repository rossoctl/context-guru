package proxy

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
)

type accTap struct {
	mu  sync.Mutex
	evs []dash.Event
}

func (c *accTap) add(e *dash.Event) { c.mu.Lock(); c.evs = append(c.evs, *e); c.mu.Unlock() }
func (c *accTap) all() []dash.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]dash.Event(nil), c.evs...)
}

func accHandler(t *testing.T, up string) (*Handler, *dash.Recorder, *accTap) {
	t.Helper()
	return accHandlerYAML(t, up, "preset: codesafe\n")
}

// accHandlerYAML is accHandler with the config document spelled out.
func accHandlerYAML(t *testing.T, up, yaml string) (*Handler, *dash.Recorder, *accTap) {
	t.Helper()
	cfg, err := config.LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	tap := &accTap{}
	rec, err := dash.NewRecorder(dash.Options{DBPath: filepath.Join(t.TempDir(), "d.db"),
		BatchSize: 1, FlushInterval: time.Millisecond, Tap: tap.add})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{
		AnthropicUpstream: up, OpenAIUpstream: up, Preset: cfg.Preset,
		Dashboard: rec, Prices: fixedPricer{},
	})
	t.Cleanup(h.Close)
	return h, rec, tap
}

func accPost(t *testing.T, h *Handler, session string) {
	t.Helper()
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages",
		strings.NewReader(string(anthropicRequest("aws/claude-sonnet-5"))))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-context-guru-session", session)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestNoUsageMeansNoAccountantResult(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
	}))
	defer up.Close()
	h, rec, tap := accHandler(t, up.URL)
	accPost(t, h, "sess-err")
	waitForRows(t, rec, 1)
	for _, e := range tap.all() {
		if e.HasAccountant() {
			t.Error("an event without usage carries an accountant result")
		}
	}
}

// accRunUsage posts two requests through a fake upstream that reports the given prompt usage and
// returns the second recorded event and the byte length of the body the upstream received.
func accRunUsage(t *testing.T, in, cr, cw int) ([]dash.Event, int) {
	evs, fwd := accRunUsageBody(t, in, cr, cw)
	return evs, len(fwd)
}

// accRunUsageBody is accRunUsage returning the forwarded body itself.
func accRunUsageBody(t *testing.T, in, cr, cw int) ([]dash.Event, []byte) {
	t.Helper()
	var fwd atomic.Value
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		fwd.Store(b)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"msg_1","type":"message","role":"assistant",
		  "content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",
		  "usage":{"input_tokens":%d,"output_tokens":34,
		           "cache_read_input_tokens":%d,"cache_creation_input_tokens":%d}}`, in, cr, cw)
	}))
	defer up.Close()
	h, rec, tap := accHandler(t, up.URL)
	// Two requests: the first has no previous request to classify cache breaks against, so the
	// library leaves it with an unpriced "penalty_unknown" and the event keeps its legacy figures.
	accPost(t, h, "sess-cf")
	accPost(t, h, "sess-cf")
	waitForRows(t, rec, 2)
	b, _ := fwd.Load().([]byte)
	return tap.all()[1:], b
}

// costOnlyOf prices a copy of a recorded event with Event.Price alone: its cost and no saving,
// which is what an event the accountant library could not price carries.
func costOnlyOf(e dash.Event) dash.Event {
	e.Components = append([]dash.CompRow(nil), e.Components...)
	p, _ := fixedPricer{}.Price(context.Background(), e.Model)
	e.Price(p, true)
	return e
}

func extractRow(t *testing.T, e dash.Event) dash.CompRow {
	t.Helper()
	for _, c := range e.Components {
		if c.Component == "extract" {
			return c
		}
	}
	t.Fatal("no extract row")
	return dash.CompRow{}
}

// A usage consistent with the forwarded body: 70 tokens over the ~490 message bytes of the forwarded
// body, ~0.14 token per byte (real traffic is 0.25-0.33; what matters is that the usage bills the
// forwarded body, not a different one). Hand derivation of the extract figure:
//
//	positions    = the body's tools, system and messages elements (proxy accBodies); here only
//	               messages, so the envelope (model, max_tokens) is in no position
//	fwdBytes     = sum of the forwarded message elements' bytes
//	removed      = client message bytes - forwarded message bytes (one tool_result rewritten)
//	usage        = 20 input + 50 cache write = 70 tokens
//	ratio        = 70 tokens / fwdBytes                      (the library's bytes-to-tokens ratio)
//	saved tokens = floor(before*70/fwdBytes) - floor(after*70/fwdBytes), in the Input tier: the
//	               removed span was never cached (no previous request cached it)
//	SavedUSD     = saved tokens * 2e-6                       (fixedPricer Input rate)
//
// The op sizes are wire bytes, so the derivation is exact up to the two floors (1 token).
// It runs through the real proxy path, and the Result must carry no failed check: before the op
// sizes were wire bytes, the sum check failed on every request (M1).
func TestRealisticUsageMatchesHandDerivation(t *testing.T) {
	evs, fwd := accRunUsageBody(t, 20, 0, 50)
	if len(evs) != 1 {
		t.Fatalf("events = %d", len(evs))
	}
	e := evs[0]
	r, ok := e.AccountantResult()
	if !ok || len(r.Checks) != 0 {
		t.Fatalf("Result attached %v, checks %+v: the real path must pass every library check", ok, r.Checks)
	}
	if fb := e.AccountantFallback(); fb != "" {
		t.Fatalf("the override must apply, fell back: %s", fb)
	}
	msgBytes := func(b []byte) int {
		n := 0
		for _, m := range gjson.GetBytes(b, "messages").Array() {
			n += len(m.Raw)
		}
		return n
	}
	client, fb := msgBytes(anthropicRequest("aws/claude-sonnet-5")), msgBytes(fwd)
	want := float64((client-fb)*70/fb) * 2e-6
	got := extractRow(t, e).SavedUSD
	if math.Abs(got-want) > 2*2e-6 {
		t.Fatalf("extract SavedUSD = %v, hand-derived %v (client %d forwarded %d message bytes)", got, want, client, fb)
	}
	t.Logf("library %v hand %v", got, want)
}

// The old fixture: 10,512 prompt tokens for a few-hundred-byte forwarded body. The library's
// token figure then exceeds the request's size, so PriceMode must book no saving, and for that
// reason: the Result has counted operations whose Saved tokens exceed TokensBefore and nothing
// unpriced, so the plausibility bound is what fired, not some earlier gate.
func TestAbsurdUsageBooksNoSaving(t *testing.T) {
	cfs, _ := accRunUsage(t, 12, 9000, 1500)
	if got := extractRow(t, cfs[0]).SavedUSD; got != 0 || cfs[0].BaselineCostUSD != cfs[0].CostUSD {
		t.Fatalf("absurd usage booked a saving: component %v, baseline %v, cost %v",
			got, cfs[0].BaselineCostUSD, cfs[0].CostUSD)
	}
	r, ok := cfs[0].AccountantResult()
	if !ok || len(r.Unpriced) != 0 || len(r.Checks) != 0 {
		t.Fatalf("Result %v: unpriced %+v checks %+v, want a clean Result", ok, r.Unpriced, r.Checks)
	}
	saved := 0
	for _, o := range r.Ops {
		if !o.DryRun && o.Call == nil {
			saved += o.Saved.Input + o.Saved.CacheRead + o.Saved.CacheWrite5m + o.Saved.CacheWrite1h
		}
	}
	if saved <= cfs[0].TokensBefore {
		t.Fatalf("counted Saved %d <= TokensBefore %d: the bound would not fire", saved, cfs[0].TokensBefore)
	}
	if fb := cfs[0].AccountantFallback(); fb != dash.FallbackImplausibleSize {
		t.Fatalf("fallback reason %q, want %q", fb, dash.FallbackImplausibleSize)
	}
}

// The tracker needs a dashboard: the Result is attached to the recorded event, so without a
// recorder there is nowhere to put it and nothing is built.
func TestNoDashboardBuildsNoTracker(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	h := New(nil, store.NewMemory(store.Options{}), nil, Options{AnthropicUpstream: up.URL, Prices: fixedPricer{}})
	defer h.Close()
	if h.savings != nil {
		t.Error("a handler without a dashboard built a savings tracker")
	}
	hd, _, _ := accHandler(t, up.URL)
	if hd.savings == nil {
		t.Error("a handler with a dashboard must build the tracker")
	}
}
