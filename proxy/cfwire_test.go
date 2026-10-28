package proxy

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
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
