package proxy

import (
	"bytes"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"

	cf "github.com/rossoctl/context-guru/counterfactual"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestShadowDiff_BothFigures(t *testing.T) {
	// 1000 cache-read tokens saved at $0.30/M = $0.0003, and a rescue that saved a 2000-token
	// write ($3.75/M) for a read ($0.30/M): 2000 x 3.45e-6 = $0.0069.
	p := modelinfo.Price{Input: 3e-6, CacheWrite: 3.75e-6, CacheRead: 0.3e-6, Output: 15e-6}
	e := &dash.Event{BaselineCostUSD: 0.0100, CostUSD: 0.0097, KeepAliveSavedUSD: 0.0001, CachesplitSavedUSD: 0.0002}
	e.SetCounterfactual(cf.Result{
		Ops: []cf.PricedOp{{Operator: "context-guru.dedup", ChangeID: "c1", Saved: cf.TierTokens{CacheRead: 1000}}},
		CacheEffects: []cf.CacheEffect{{Kind: cf.EffectRescue, ChangeID: "ka",
			Saved: cf.TierTokens{CacheWrite5m: 2000}, Caused: cf.TierTokens{CacheRead: 2000}}},
	})
	legacy, cfUSD := shadowDiff(e, p)
	if want := 0.0003 + 0.0001 + 0.0002; !near(legacy, want) {
		t.Errorf("legacy = %v, want %v", legacy, want)
	}
	if want := 0.0003 + 0.0069; !near(cfUSD, want) {
		t.Errorf("cf = %v, want %v", cfUSD, want)
	}
}

func TestShadowDiff_NoResult(t *testing.T) {
	e := &dash.Event{BaselineCostUSD: 1, CostUSD: 0.5}
	if l, c := shadowDiff(e, modelinfo.Price{}); l != 0 || c != 0 {
		t.Errorf("no Result: got (%v, %v), want (0, 0)", l, c)
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestShadowLogLinePerRequest(t *testing.T) {
	for _, mode := range []string{"shadow", "counterfactual", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			var buf syncBuf
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			up := fakeUpstream(t)
			defer up.Close()
			h, rec, _ := cfHandler(t, up.URL, mode)
			cfPost(t, h, "sess-shadow")
			cfPost(t, h, "sess-shadow")
			waitForRows(t, rec, 2)

			n := strings.Count(buf.String(), `"msg":"cf_shadow"`)
			if mode != "shadow" {
				if n != 0 {
					t.Errorf("%s mode logged %d cf_shadow lines", mode, n)
				}
				return
			}
			if n != 2 {
				t.Fatalf("shadow mode logged %d cf_shadow lines, want 2:\n%s", n, buf.String())
			}
			for _, k := range []string{`"legacy_usd"`, `"cf_usd"`, `"delta_usd"`, `"session":"sess-shadow"`, `"components"`, `"unpriced"`} {
				if !strings.Contains(buf.String(), k) {
					t.Errorf("cf_shadow line lacks %s", k)
				}
			}
		})
	}
}
