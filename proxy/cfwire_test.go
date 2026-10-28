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

// In counterfactual mode the stored component figures and the baseline come from the library's
// Result when the event qualifies, and equal the legacy-priced copy of the same event when not.
// shadow and legacy always keep the legacy figures. The harness's request may yield no rewrite
// operation; the test then covers the fallback arm and the invariant, and the dash tests cover
// the override arithmetic.
func TestCounterfactualModeStoresLibraryFiguresOrFallsBack(t *testing.T) {
	price := fixedPricer{}
	p, _ := price.Price(nil, "aws/claude-sonnet-5")
	overridden := 0
	for _, mode := range []string{"legacy", "shadow", "counterfactual"} {
		for i, ev := range cfTwoRequests(t, mode) {
			e := ev
			e.Components = append([]dash.CompRow(nil), ev.Components...)
			want := ev
			want.Components = append([]dash.CompRow(nil), ev.Components...)
			// want: the same event priced by the legacy rule only.
			want.Price(p, true)
			fig := e.CounterfactualFigure(p)
			qualifies := mode == "counterfactual" && e.HasCounterfactual() && len(fig.Unpriced) == 0
			if qualifies {
				for j, c := range e.Components {
					if c.Acted {
						if _, ok := fig.ByComponent[c.Component]; !ok {
							qualifies = false
						}
					}
					_ = j
				}
			}
			if qualifies {
				overridden++
				var sum float64
				for j, c := range e.Components {
					if d := c.SavedUSD - fig.ByComponent[c.Component]; d > 1e-12 || d < -1e-12 {
						t.Errorf("%s event %d component %d: SavedUSD %v, library %v", mode, i, j, c.SavedUSD, fig.ByComponent[c.Component])
					}
					sum += c.SavedUSD
				}
				if d := sum - (e.BaselineCostUSD - e.CostUSD); d > 1e-9 || d < -1e-9 {
					t.Errorf("%s event %d: components %v != baseline delta %v", mode, i, sum, e.BaselineCostUSD-e.CostUSD)
				}
				continue
			}
			if e.BaselineCostUSD != want.BaselineCostUSD {
				t.Errorf("%s event %d: baseline %v, legacy %v", mode, i, e.BaselineCostUSD, want.BaselineCostUSD)
			}
			for j := range e.Components {
				if e.Components[j].SavedUSD != want.Components[j].SavedUSD {
					t.Errorf("%s event %d component %d: SavedUSD %v, legacy %v", mode, i, j, e.Components[j].SavedUSD, want.Components[j].SavedUSD)
				}
			}
		}
	}
	if overridden == 0 {
		t.Error("no counterfactual event qualified for the override: the override arm is untested")
	}
}
