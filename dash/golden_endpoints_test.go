package dash

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// The dashboard's heavy endpoints were rewritten for speed (id-sliced concurrent aggregates, one
// pass over tool_declarations, window functions for the replay ceiling, a response cache, a shared
// dataset read). The rewrite is only correct if every number is unchanged, so this test captures
// each endpoint's JSON on a fixed fixture and compares it, number by number, to a golden file made
// by the code BEFORE the rewrite (git show origin/main's dash/ on this fixture; see goldenUpdate).
//
//	CG_GOLDEN_UPDATE=1 go test ./dash -run TestGoldenEndpoints   rewrites dash/testdata/golden/*.json
//
// Floats are compared to a relative 1e-9: a sum over a different slice order differs in the last
// bits and that is not a different answer. Everything else must be equal.

type stubPrices struct{}

func (stubPrices) Price(_ context.Context, model string) (modelinfo.Price, bool) {
	switch model {
	case "m-opus":
		return modelinfo.Price{Input: 15e-6, CacheRead: 1.5e-6, CacheWrite: 18.75e-6, Output: 75e-6}, true
	case "m-sonnet":
		return modelinfo.Price{Input: 3e-6, CacheRead: 3e-7, CacheWrite: 3.75e-6, Output: 15e-6}, true
	}
	return modelinfo.Price{}, false // m-unpriced: exercises every "not priced" branch
}

var goldenBase = time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC).UnixMilli()

// goldenAPI builds the fixture: 2 tenants, 60 sessions of 6-14 turns over ~9 days, three models
// (one unpriced), cache hits / TTL expiries / compaction, keep-alive pings, component gates and
// events (one row of a shape the fast counter parser must refuse), and a tool inventory per
// session. Everything is a function of the loop indices, never of the clock or a random source.
func goldenAPI(t *testing.T) *API {
	t.Helper()
	a, rec := newTestAPI(t, Options{CaptureContent: true})
	a.SetPricer(stubPrices{})
	models := []string{"m-sonnet", "m-opus", "m-unpriced"}
	var evs []*Event
	var msgs []invMsg
	for s := 0; s < 60; s++ {
		tenant := fmt.Sprintf("t%d", 1+s%2)
		session := fmt.Sprintf("%s:sess-%02d", tenant, s)
		model := models[s%3]
		ts := goldenBase + int64(s)*3*3600*1000 + int64(s%7)*600_000
		turns := 6 + s%9
		for k := 0; k < turns; k++ {
			gap := int64(20_000 + (s*k%11)*40_000) // 20 s .. 7.3 min: straddles the 5-minute TTL
			if k%5 == 4 {
				gap = 40 * 60_000
			}
			ts += gap
			before := 20_000 + 3_000*k + s*37
			after := before
			if k%3 == 1 {
				after = before - 2_500 - s
			}
			e := mkEvent(ts, session, model, before, after)
			e.TenantID, e.Tools = tenant, 3+s%4
			e.Agent = []string{"claude-code", "bob"}[s%2]
			e.CacheRead, e.CacheWrite, e.FreshInput = int64(before-600), 600, 40
			e.CacheMissReason = CacheHit
			e.CostUSD = 0.002 * float64(1+k%4)
			e.BaselineCostUSD = e.CostUSD + 0.0005*float64(before-after)/1000
			if gap > 300_000 {
				e.CacheMissReason = "ttl_expiry"
				e.CacheRead, e.CacheWrite = 0, int64(before)
			}
			e.UpstreamMs = float64(300 + 17*k + s)
			e.CGLatencyMs = float64(2 + k%5)
			e.SavedUnique = before - after
			e.Components = []CompRow{
				{Component: "extract", Kind: "offload", Acted: before > after, Mutated: before > after,
					SavedGross: before - after, SavedUnique: before - after, DurationMs: 1.5,
					Gates: map[string]int{"below_min_size": k % 4, "not_frozen": 1}, Events: map[string]int{"offloaded": before - after}},
				{Component: "cacheinject", Kind: "reformat", Mutated: true, DurationMs: 0.1,
					Gates: map[string]int{"already_set": 1}},
			}
			if s%13 == 5 && k == 2 { // a row the flat-integer fast path must refuse
				e.Components[0].Gates = map[string]int{"weird\"key": 3}
			}
			evs = append(evs, e)
			if k == turns-1 && s%4 == 0 { // a keep-alive ping after the last turn
				p := mkEvent(ts+280_000, session, model, 0, 0)
				p.TenantID, p.KeepAlive, p.Tools = tenant, true, e.Tools
				p.CacheRead, p.CacheWrite, p.CostUSD, p.Components = e.CacheRead+e.CacheWrite, 0, 0.0004, nil
				evs = append(evs, p)
			}
		}
		decls := []Decl{
			{Kind: KindTool, Name: "Bash", Tokens: 120},
			{Kind: KindMCPTool, Name: "mcp__alpha__read", Server: "alpha", Tokens: 300 + s},
			{Kind: KindMCPTool, Name: "mcp__alpha__write", Server: "alpha", Tokens: 410},
			{Kind: KindSkill, Name: "skill-" + fmt.Sprint(s%5), Tokens: 220},
			{Kind: KindSkillListing, Server: SkillsOK, Tokens: 900},
		}
		if s < 40 { // a server that went away: later sessions no longer declare it
			decls = append(decls, Decl{Kind: KindMCPTool, Name: "mcp__beta__x", Server: "beta", Tokens: 500})
		}
		used := []Used{{Name: "Bash", Calls: 4}}
		if s%3 == 0 {
			used = append(used, Used{Name: "mcp__alpha__read", Calls: 2})
		}
		msgs = append(msgs, invMsg{tenant: tenant, session: session, ts: ts - 3600_000,
			inv: &Inventory{Digest: "d" + fmt.Sprint(s), Decls: decls, Used: used, UseFingerprint: uint64(s + 1)}})
	}
	seed(t, rec, evs...)
	w := &invWriter{db: rec.DB(), seen: map[string]*invSession{}}
	if err := w.write(msgs); err != nil {
		t.Fatal(err)
	}
	return a
}

func goldenGet(t *testing.T, a *API, path string) []byte {
	t.Helper()
	m := http.NewServeMux()
	a.Mount(m)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, w.Code, w.Body.String())
	}
	return w.Body.Bytes()
}

// goldenPaths: name -> URL. Absent on purpose: /api/capture (db size), /api/keepalive/live (clock).
func goldenPaths() map[string]string {
	hold := fmt.Sprintf("train_since=%d&train_until=%d&test_since=%d&test_until=%d",
		goldenBase, goldenBase+4*24*3600*1000, goldenBase+4*24*3600*1000+1, goldenBase+10*24*3600*1000)
	return map[string]string{
		"stats":           "/api/stats",
		"stats_lean":      "/api/stats?lean=1",
		"series_hour":     "/api/series?bucket=3600000",
		"series_day":      "/api/series?bucket=86400000",
		"breakdown_model": "/api/breakdown?dim=model",
		"breakdown_agent": "/api/breakdown?dim=agent",
		"facets":          "/api/facets",
		"sessions":        "/api/sessions?limit=500",
		"components":      "/api/components",
		"compaction":      "/api/components/compaction-episodes",
		"keepalive":       "/api/keepalive",
		"ka_behaviour":    "/api/keepalive/behaviour?x=280&k=2",
		"ka_sessions":     "/api/keepalive/sessions",
		"ka_calc":         "/api/keepalive/calc?x=280&k=2",
		"ka_recommend":    "/api/keepalive/recommend",
		"tools":           "/api/tools",
		"toolfilter":      "/api/toolfilter",
		"prompt":          "/api/prompt",
		"kvcache":         "/api/kvcache",
		"kv_rows":         "/api/kvcache/rows?limit=100",
		"kv_rows_sorted":  "/api/kvcache/rows?limit=25&offset=10&sort=idle&dir=asc",
		"kv_simulate":     "/api/kvcache/simulate",
		"kv_pricing":      "/api/kvcache/pricing",
		"kv_suggest":      "/api/kvcache/suggest",
		"kv_holdout":      "/api/kvcache/suggest/holdout?" + hold,
		"kv_has_next":     "/api/kvcache?has_next=yes&bucket=afternoon",
		"stats_windowed":  fmt.Sprintf("/api/stats?since=%d&until=%d", goldenBase+2*24*3600*1000, goldenBase+5*24*3600*1000),
		"tools_windowed":  fmt.Sprintf("/api/tools?since=%d&until=%d", goldenBase+2*24*3600*1000, goldenBase+5*24*3600*1000),
	}
}

// tieKeys are top-level fields the fixture leaves tied, so which of several equal candidates is
// reported depends on map iteration order in the code under test (and was already different
// run to run before the rewrite). Their VALUES (worst_net_usd, ...) are still compared.
var tieKeys = map[string][]string{"keepalive": {"worst_session"}}

func dropTies(name string, v any) any {
	if m, ok := v.(map[string]any); ok {
		for _, k := range tieKeys[name] {
			delete(m, k)
		}
	}
	return v
}

func TestGoldenEndpoints(t *testing.T) {
	a := goldenAPI(t)
	dir := filepath.Join("testdata", "golden")
	update := os.Getenv("CG_GOLDEN_UPDATE") != ""
	if update {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	names := make([]string, 0)
	for n := range goldenPaths() {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		path := goldenPaths()[name]
		t.Run(name, func(t *testing.T) {
			body := goldenGet(t, a, path)
			file := filepath.Join(dir, name+".json")
			if update {
				var v any
				if err := json.Unmarshal(body, &v); err != nil {
					t.Fatal(err)
				}
				out, _ := json.MarshalIndent(dropTies(name, v), "", " ")
				if err := os.WriteFile(file, append(out, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("no golden file (run with CG_GOLDEN_UPDATE=1 on the pre-change code): %v", err)
			}
			var w, g any
			if err := json.Unmarshal(want, &w); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(body, &g); err != nil {
				t.Fatal(err)
			}
			if diffs := jsonDiff("", dropTies(name, w), dropTies(name, g), nil); len(diffs) > 0 {
				if len(diffs) > 12 {
					diffs = diffs[:12]
				}
				t.Fatalf("%s differs from its golden file:\n  %s", name, strings.Join(diffs, "\n  "))
			}
		})
	}
}

func jsonDiff(p string, want, got any, out []string) []string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return append(out, p+": not an object")
		}
		keys := map[string]bool{}
		for k := range w {
			keys[k] = true
		}
		for k := range g {
			keys[k] = true
		}
		ks := make([]string, 0, len(keys))
		for k := range keys {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			wv, wok := w[k]
			gv, gok := g[k]
			switch {
			case !wok:
				out = append(out, p+"/"+k+": unexpected field")
			case !gok:
				out = append(out, p+"/"+k+": missing field")
			default:
				out = jsonDiff(p+"/"+k, wv, gv, out)
			}
		}
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return append(out, fmt.Sprintf("%s: length %d, want %d", p, len(g), len(w)))
		}
		for i := range w {
			out = jsonDiff(fmt.Sprintf("%s[%d]", p, i), w[i], g[i], out)
		}
	case float64:
		g, ok := got.(float64)
		if !ok || (g != w && math.Abs(g-w) > 1e-9*math.Max(1, math.Max(math.Abs(g), math.Abs(w)))) {
			out = append(out, fmt.Sprintf("%s: %v, want %v", p, got, want))
		}
	default:
		if fmt.Sprint(want) != fmt.Sprint(got) {
			out = append(out, fmt.Sprintf("%s: %v, want %v", p, got, want))
		}
	}
	return out
}
