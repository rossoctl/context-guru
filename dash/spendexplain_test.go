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

// causeCounts runs the pass over rows and returns the rewrite-cause counts.
func causeCounts(t *testing.T, evs ...*Event) map[string]float64 {
	t.Helper()
	a, rec := newTestAPI(t, Options{})
	a.SetPricer(fixedPricer{})
	seed(t, rec, evs...)
	_, body := get(t, a, "/api/spend/explain?now="+itoa(evs[len(evs)-1].TS+60000), "127.0.0.1:1")
	got := map[string]float64{}
	for k, v := range body["rewrites"].(map[string]any) {
		got[k] = v.(map[string]any)["n"].(float64)
	}
	return got
}

func threadEv(ts int64, model, thread string, tools int) *Event {
	e := spendEv(ts, "s", 3, 5, 0, 10000, 0)
	e.Model, e.ThreadID, e.Tools = model, thread, tools
	return e
}

func TestSpendExplainOtherModelInSeenSessionIsNewThread(t *testing.T) {
	t0 := int64(1_700_000_000_000)
	got := causeCounts(t,
		threadEv(t0, "opus", "", 5),
		threadEv(t0+60000, "sonnet", "p:side", 5),
	)
	if got["cold_start"] != 1 || got["new_thread"] != 1 {
		t.Errorf("causes = %v; want one cold_start (main) and one new_thread (side request)", got)
	}
}

func TestSpendExplainSubagentsWithSameShapeAreSeparateThreads(t *testing.T) {
	t0 := int64(1_700_000_000_000)
	got := causeCounts(t,
		threadEv(t0, "opus", "", 5),
		threadEv(t0+60000, "opus", "a:one", 9),
		threadEv(t0+120000, "opus", "a:two", 9),
	)
	if got["cold_start"] != 1 || got["new_thread"] != 2 || len(got) != 2 {
		t.Errorf("causes = %v; want cold_start 1, new_thread 2", got)
	}
}

func TestSpendExplainSeenThreadGoingIdleStaysIdleExpiry(t *testing.T) {
	t0 := int64(1_700_000_000_000)
	got := causeCounts(t,
		threadEv(t0, "opus", "a:one", 9),
		threadEv(t0+60000, "opus", "", 5),
		threadEv(t0+20*60000, "opus", "a:one", 9),
	)
	if got["idle_expiry"] != 1 || got["cold_start"] != 1 || got["new_thread"] != 1 {
		t.Errorf("causes = %v; want cold_start 1, new_thread 1, idle_expiry 1", got)
	}
}

func TestSpendExplainEmptyThreadIDKeepsOldBehaviour(t *testing.T) {
	t0 := int64(1_700_000_000_000)
	got := causeCounts(t,
		threadEv(t0, "opus", "", 5),
		threadEv(t0+60000, "opus", "", 9), // different tool count, old rows: new_thread
		threadEv(t0+20*60000, "opus", "", 5),
	)
	if got["cold_start"] != 1 || got["new_thread"] != 1 || got["idle_expiry"] != 1 {
		t.Errorf("causes = %v; want cold_start 1, new_thread 1, idle_expiry 1", got)
	}
}

// A "p:" id is a fingerprint of one request, not a stable thread name (a side request that grows
// gets a new id each turn), so it must not split a thread: a long pause stays idle_expiry.
func TestSpendExplainSideRequestIDsDoNotSplitThreads(t *testing.T) {
	t0 := int64(1_700_000_000_000)
	got := causeCounts(t,
		threadEv(t0, "opus", "", 5),
		threadEv(t0+60000, "sonnet", "p:aaa", 0),
		threadEv(t0+20*60000, "sonnet", "p:bbb", 0),
	)
	if got["cold_start"] != 1 || got["new_thread"] != 1 || got["idle_expiry"] != 1 {
		t.Errorf("causes = %v; want cold_start 1, new_thread 1, idle_expiry 1", got)
	}
}

// The ping's cost sits on the ping row, its saving on the agent row it rescued. Summing the
// saving over ping rows gave a ping_saved_usd of 0 on every window (#676), so the home screen
// reported every ping as a loss.
func TestSpendExplainLedgerCreditsPingSavingFromTheRescuedRow(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	a.SetPricer(fixedPricer{})
	const m = int64(60000)
	t0 := int64(1_700_000_000_000)
	rescued := spendEv(t0+8*m, "s", 5, 5, 10000, 100, 0)
	rescued.KeepAliveSavedUSD = 0.05
	// a credit with no ping in the 5 minutes before it: kaSaved refuses it
	stray := spendEv(t0+8*m, "u", 3, 5, 10000, 100, 0)
	stray.KeepAliveSavedUSD = 0.07
	seed(t, rec,
		spendEv(t0, "s", 3, 5, 0, 10000, 0),
		kaPing(t0+4*m, "s", 0.002, 10000, 0),
		rescued,
		spendEv(t0, "u", 2, 5, 0, 10000, 0),
		stray,
	)
	_, body := get(t, a, "/api/spend/explain?now="+itoa(t0+9*m), "127.0.0.1:1")
	l := body["ledger"].(map[string]any)
	if got := l["ping_usd"].(float64); math.Abs(got-0.002) > 1e-9 {
		t.Errorf("ping_usd = %v; want 0.002", got)
	}
	if got := l["ping_saved_usd"].(float64); math.Abs(got-0.05) > 1e-9 {
		t.Errorf("ping_saved_usd = %v; want 0.05 (the rescued row only, not the unreachable 0.07)", got)
	}
}
