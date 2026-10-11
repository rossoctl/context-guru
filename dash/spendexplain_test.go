package dash

import (
	"context"
	"math"
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

type fixedPricer struct{}

// $1/Mtok fresh, 0.1 read, 1.25 write, 5 out.
func (fixedPricer) Price(_ context.Context, model string) (modelinfo.Price, bool) {
	if model == "free" {
		return modelinfo.Price{}, false
	}
	return modelinfo.Price{Input: 1e-6, Output: 5e-6, CacheRead: 1e-7, CacheWrite: 1.25e-6}, true
}

func spendEv(ts int64, sess string, msgs int, fresh, read, write, w1h int64) *Event {
	e := mkEvent(ts, sess, "m", 100, 100)
	e.Messages, e.Tools, e.SystemBlocks = msgs, 5, 2
	e.FreshInput, e.CacheRead, e.CacheWrite, e.CacheWrite1h, e.OutputTokens = fresh, read, write, w1h, 100
	e.CostUSD = float64(fresh)*1e-6 + float64(read)*1e-7 + float64(write)*1.25e-6 + 100*5e-6
	e.BaselineCostUSD = e.CostUSD
	e.Components = nil
	return e
}

func TestSpendExplainClassifiesRewrites(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	a.SetPricer(fixedPricer{})
	const m = int64(60000)
	t0 := int64(1_700_000_000_000) + 7*3600_000 // an arbitrary instant
	seed(t, rec,
		spendEv(t0, "s", 3, 5, 0, 10000, 0),            // cold start, 10k written
		spendEv(t0+1*m, "s", 5, 5, 10000, 300, 0),      // warm hit: not a rewrite
		spendEv(t0+11*m, "s", 7, 5, 0, 10300, 0),       // 10 min gap, 5m TTL: idle expiry
		spendEv(t0+12*m, "s", 4, 5, 0, 10300, 0),       // message count fell: history rewrite
		spendEv(t0+13*m, "s", 6, 5, 0, 10300, 0),       // warm, longer, still rewritten: prefix change
		spendEv(t0+14*m, "s", 8, 5, 10300, 100, 0),     // hit
		spendEv(t0+15*m, "other", 2, 5, 0, 8000, 8000), // another session cold start on the 1h tier
	)
	_, body := get(t, a, "/api/spend/explain?now="+itoa(t0+16*m), "127.0.0.1:1")
	rw := body["rewrites"].(map[string]any)
	want := map[string]float64{"cold_start": 2, "idle_expiry": 1, "history_rewrite": 1, "prefix_change": 1}
	for k, n := range want {
		c, ok := rw[k].(map[string]any)
		if !ok || c["n"].(float64) != n {
			t.Errorf("cause %s = %v; want n=%v (all: %v)", k, rw[k], n, rw)
		}
	}
	// idle expiry: 10300 tokens x (1.25 - 0.1) $/Mtok = $0.011845
	if got := rw["idle_expiry"].(map[string]any)["usd"].(float64); math.Abs(got-0.011845) > 1e-9 {
		t.Errorf("idle_expiry usd = %v; want 0.011845", got)
	}
	// the 10-minute pause lands in the 10-30m band
	gaps := body["idle_gaps"].([]any)
	if g := gaps[1].(map[string]any); g["band"] != "10-30m" || g["n"].(float64) != 1 {
		t.Errorf("idle_gaps = %v; want the 10-minute pause in 10-30m", gaps)
	}
	// the causes partition the cache-write line: cold(2) + idle + history + prefix writes
	wsum := 0.0
	for _, c := range rw {
		wsum += c.(map[string]any)["write_usd"].(float64)
	}
	if want := (10000+8000)*1.25e-6 + 8000*(2e-6-1.25e-6) + 3*10300*1.25e-6; math.Abs(wsum-want) > 1e-9 {
		t.Errorf("sum of write_usd = %v; want %v", wsum, want)
	}
	usd := body["usd"].(map[string]any)
	stored := body["stored_cost_usd"].(float64)
	if math.Abs(usd["total"].(float64)-stored-8000*(2e-6-1.25e-6)) > 1e-9 { // the 1h premium is the only difference
		t.Errorf("class dollars %v do not reconcile with stored cost %v", usd["total"], stored)
	}
	live := body["live"].([]any)
	if len(live) != 2 {
		t.Fatalf("live = %v; want both sessions", live)
	}
	for _, l := range live {
		l := l.(map[string]any)
		if l["session"] == "other" && !(l["ttl_1h"].(bool)) {
			t.Errorf("1h tier not detected: %v", l)
		}
	}
}

func TestSpendExplainSkipsUnpricedAndKeepsWarmup(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	a.SetPricer(fixedPricer{})
	const m = int64(60000)
	t0 := int64(1_700_000_000_000)
	free := spendEv(t0+2*m, "f", 2, 5, 0, 9000, 0)
	free.Model = "free"
	seed(t, rec, free,
		spendEv(t0+0*m, "s", 3, 5, 0, 10000, 0),
		spendEv(t0+2*m, "s", 5, 5, 0, 10300, 0), // inside the window, 2 min after a request OUTSIDE it
	)
	_, body := get(t, a, "/api/spend/explain?since="+itoa(t0+m), "127.0.0.1:1")
	if body["unpriced_requests"].(float64) != 1 || body["requests"].(float64) != 2 || body["priced_requests"].(float64) != 1 {
		t.Errorf("want 2 requests in the window, 1 of them unpriced: %v", body)
	}
	rw := body["rewrites"].(map[string]any)
	if _, cold := rw["cold_start"]; cold {
		t.Errorf("warm-up request ignored: first counted request misread as a cold start: %v", rw)
	}
	if rw["prefix_change"] == nil {
		t.Errorf("want prefix_change (2 min gap, longer history, fully rewritten): %v", rw)
	}
}

func TestToolsLastUsed(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	_, err := rec.DB().sql.Exec(`INSERT INTO tool_uses(tenant_id, session_id, name, server, skill, calls, first_ts, last_ts)
		VALUES ('', 's1', 'mcp__x__a', 'x', '', 3, 10, 50), ('', 's2', 'mcp__x__a', 'x', '', 1, 60, 90)`)
	if err != nil {
		t.Fatal(err)
	}
	_, body := get(t, a, "/api/tools/last-used", "127.0.0.1:1")
	ts := body["tools"].([]any)
	if len(ts) != 1 || ts[0].(map[string]any)["last_ts"].(float64) != 90 || ts[0].(map[string]any)["calls"].(float64) != 4 {
		t.Errorf("tools = %v", ts)
	}
}
