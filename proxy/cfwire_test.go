package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	cfg, err := config.LoadBytes([]byte("preset: codesafe\n"))
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
	t.Helper()
	var fwd int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		atomic.StoreInt64(&fwd, int64(len(b)))
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
	return tap.all()[1:], int(atomic.LoadInt64(&fwd))
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

// A usage consistent with the forwarded body: the request is billed as ~0.3 token per byte, as
// real traffic is. Hand derivation of the extract figure:
//
//	client body  = len(anthropicRequest)            bytes
//	forwarded    = len of the body the upstream got  bytes (the extracted stub)
//	removed      = client - forwarded                bytes
//	usage        = 20 input + 50 cache write         = 70 tokens
//	ratio        = 70 tokens / forwarded bytes
//	saved tokens = removed * 70 / forwarded          (in the Input tier, the removed span was never cached)
//	SavedUSD     = saved tokens * 2e-6               (fixedPricer Input rate)
//
// The library's removed span is the extracted text, slightly smaller than client - forwarded, so
// the derived figure is checked within 10%.
func TestCounterfactualModeRealisticUsageMatchesHandDerivation(t *testing.T) {
	evs, fwd := cfRunUsage(t, "counterfactual", 20, 0, 50)
	if len(evs) != 1 {
		t.Fatalf("events = %d", len(evs))
	}
	e := evs[0]
	client := len(anthropicRequest("aws/claude-sonnet-5"))
	want := float64(client-fwd) * 70 / float64(fwd) * 2e-6
	got := extractRow(t, e).SavedUSD
	if got < want*0.9 || got > want*1.1 {
		t.Fatalf("extract SavedUSD = %v, hand-derived %v (client %d forwarded %d)", got, want, client, fwd)
	}
	legacy, _ := cfRunUsage(t, "legacy", 20, 0, 50)
	lg := extractRow(t, legacy[0]).SavedUSD
	if lg <= 0 || got/lg > 20 || lg/got > 20 {
		t.Fatalf("library %v and legacy %v differ by more than 20x", got, lg)
	}
	t.Logf("library %v legacy %v hand %v", got, lg, want)
}

// The old fixture: 10,512 prompt tokens for a few-hundred-byte forwarded body. The library's
// token figure then exceeds the request's size, so PriceMode must fall back to legacy.
func TestCounterfactualModeAbsurdUsageFallsBackToLegacy(t *testing.T) {
	cfs, _ := cfRunUsage(t, "counterfactual", 12, 9000, 1500)
	leg, _ := cfRunUsage(t, "legacy", 12, 9000, 1500)
	got, want := extractRow(t, cfs[0]).SavedUSD, extractRow(t, leg[0]).SavedUSD
	if got != want || cfs[0].BaselineCostUSD != leg[0].BaselineCostUSD {
		t.Fatalf("absurd usage did not fall back: component %v vs legacy %v, baseline %v vs %v",
			got, want, cfs[0].BaselineCostUSD, leg[0].BaselineCostUSD)
	}
}
