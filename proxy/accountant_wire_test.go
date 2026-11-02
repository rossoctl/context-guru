package proxy

import (
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

type cfTap struct {
	mu  sync.Mutex
	evs []dash.Event
}

func (c *cfTap) add(e *dash.Event) { c.mu.Lock(); c.evs = append(c.evs, *e); c.mu.Unlock() }
func (c *cfTap) all() []dash.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]dash.Event(nil), c.evs...)
}

func cfHandler(t *testing.T, up, savings string) (*Handler, *dash.Recorder, *cfTap) {
	t.Helper()
	return cfHandlerYAML(t, up, savings, "preset: codesafe\n")
}

// cfHandlerYAML is cfHandler with the config document spelled out.
func cfHandlerYAML(t *testing.T, up, savings, yaml string) (*Handler, *dash.Recorder, *cfTap) {
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
	tap := &cfTap{}
	rec, err := dash.NewRecorder(dash.Options{DBPath: filepath.Join(t.TempDir(), "d.db"),
		BatchSize: 1, FlushInterval: time.Millisecond, Tap: tap.add})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{
		AnthropicUpstream: up, OpenAIUpstream: up, Preset: cfg.Preset,
		Dashboard: rec, Prices: fixedPricer{}, Savings: savings,
	})
	t.Cleanup(h.Close)
	return h, rec, tap
}

func cfPost(t *testing.T, h *Handler, session string) {
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

func cfTwoRequests(t *testing.T, savings string) []dash.Event {
	up := fakeUpstream(t)
	defer up.Close()
	h, rec, tap := cfHandler(t, up.URL, savings)
	cfPost(t, h, "sess-cf")
	cfPost(t, h, "sess-cf")
	waitForRows(t, rec, 2)
	return tap.all()
}

func TestShadowAttachesCounterfactualAndKeepsLegacyFigures(t *testing.T) {
	shadow, legacy := cfTwoRequests(t, "shadow"), cfTwoRequests(t, "legacy")
	if len(shadow) != 2 || len(legacy) != 2 {
		t.Fatalf("events: shadow %d legacy %d", len(shadow), len(legacy))
	}
	for i := range shadow {
		if !shadow[i].HasCounterfactual() {
			t.Errorf("shadow event %d has no counterfactual result", i)
		}
		if legacy[i].HasCounterfactual() {
			t.Errorf("legacy event %d has a counterfactual result", i)
		}
		if shadow[i].CostUSD != legacy[i].CostUSD || shadow[i].BaselineCostUSD != legacy[i].BaselineCostUSD {
			t.Errorf("event %d legacy figures changed: %v/%v vs %v/%v", i,
				shadow[i].CostUSD, shadow[i].BaselineCostUSD, legacy[i].CostUSD, legacy[i].BaselineCostUSD)
		}
		if len(shadow[i].Components) != len(legacy[i].Components) {
			t.Fatalf("event %d component count differs", i)
		}
		for j := range shadow[i].Components {
			if shadow[i].Components[j].SavedUSD != legacy[i].Components[j].SavedUSD {
				t.Errorf("event %d component %d SavedUSD changed", i, j)
			}
		}
	}
}

func TestLegacyModeBuildsNoTracker(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	for _, s := range []string{"", "legacy"} {
		h, _, _ := cfHandler(t, up.URL, s)
		if h.savings != nil || h.opts.Savings != "legacy" {
			t.Errorf("Savings %q: savings=%v opts=%q", s, h.savings, h.opts.Savings)
		}
	}
	h, _, _ := cfHandler(t, up.URL, "counterfactual")
	if h.savings == nil {
		t.Error("counterfactual mode must build the tracker")
	}
}

func TestUnknownSavingsValueIsRejected(t *testing.T) {
	if _, err := ParseSavings("bogus"); err == nil {
		t.Fatal("ParseSavings accepted bogus")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("New accepted an unknown Savings value")
		}
	}()
	up := fakeUpstream(t)
	defer up.Close()
	cfHandler(t, up.URL, "bogus")
}

func TestNoUsageMeansNoCounterfactualResult(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
	}))
	defer up.Close()
	h, rec, tap := cfHandler(t, up.URL, "shadow")
	cfPost(t, h, "sess-err")
	waitForRows(t, rec, 1)
	for _, e := range tap.all() {
		if e.HasCounterfactual() {
			t.Error("an event without usage carries a counterfactual result")
		}
	}
}

// cfRunUsage posts two requests through a fake upstream that reports the given prompt usage and
// returns the second recorded event and the byte length of the body the upstream received.
func cfRunUsage(t *testing.T, mode string, in, cr, cw int) ([]dash.Event, int) {
	evs, fwd := cfRunUsageBody(t, mode, in, cr, cw)
	return evs, len(fwd)
}

// cfRunUsageBody is cfRunUsage returning the forwarded body itself.
func cfRunUsageBody(t *testing.T, mode string, in, cr, cw int) ([]dash.Event, []byte) {
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
	h, rec, tap := cfHandler(t, up.URL, mode)
	// Two requests: the first has no previous request to classify cache breaks against, so the
	// library leaves it with an unpriced "penalty_unknown" and the event keeps its legacy figures.
	cfPost(t, h, "sess-cf")
	cfPost(t, h, "sess-cf")
	waitForRows(t, rec, 2)
	b, _ := fwd.Load().([]byte)
	return tap.all()[1:], b
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
//	positions    = the body's tools, system and messages elements (proxy cfBodies); here only
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
func TestCounterfactualModeRealisticUsageMatchesHandDerivation(t *testing.T) {
	evs, fwd := cfRunUsageBody(t, "counterfactual", 20, 0, 50)
	if len(evs) != 1 {
		t.Fatalf("events = %d", len(evs))
	}
	e := evs[0]
	r, ok := e.Counterfactual()
	if !ok || len(r.Checks) != 0 {
		t.Fatalf("Result attached %v, checks %+v: the real path must pass every library check", ok, r.Checks)
	}
	if fb := e.CounterfactualFallback(); fb != "" {
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
	legacy, _ := cfRunUsage(t, "legacy", 20, 0, 50)
	lg := extractRow(t, legacy[0]).SavedUSD
	if lg <= 0 || got/lg > 20 || lg/got > 20 {
		t.Fatalf("library %v and legacy %v differ by more than 20x", got, lg)
	}
	t.Logf("library %v legacy %v hand %v", got, lg, want)
}

// The old fixture: 10,512 prompt tokens for a few-hundred-byte forwarded body. The library's
// token figure then exceeds the request's size, so PriceMode must fall back to legacy, and for that
// reason: the Result has counted operations whose Saved tokens exceed TokensBefore and nothing
// unpriced, so the plausibility bound is what fired, not some earlier gate.
func TestCounterfactualModeAbsurdUsageFallsBackToLegacy(t *testing.T) {
	cfs, _ := cfRunUsage(t, "counterfactual", 12, 9000, 1500)
	leg, _ := cfRunUsage(t, "legacy", 12, 9000, 1500)
	got, want := extractRow(t, cfs[0]).SavedUSD, extractRow(t, leg[0]).SavedUSD
	if got != want || cfs[0].BaselineCostUSD != leg[0].BaselineCostUSD {
		t.Fatalf("absurd usage did not fall back: component %v vs legacy %v, baseline %v vs %v",
			got, want, cfs[0].BaselineCostUSD, leg[0].BaselineCostUSD)
	}
	r, ok := cfs[0].Counterfactual()
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
	if fb := cfs[0].CounterfactualFallback(); fb != dash.FallbackImplausibleSize {
		t.Fatalf("fallback reason %q, want %q", fb, dash.FallbackImplausibleSize)
	}
}
