package proxy

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/kvcache"
)

// CampaignCellOutcome IS the create route's decision. For every kind of cell an upload can carry,
// the exported function and a real POST /api/keepalive/campaigns must agree on which cells are
// activated, cell for cell — or an offline tool that reports "N enforceable" would be reporting a
// different rule from the one the dashboard runs.
func TestCampaignCellOutcomeAgreesWithTheCreateRoute(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, _ := f.signUpJar(t, "boss@ibm.com")
	_, a := f.signUpJar(t, "a@ibm.com")
	_, b := f.signUpJar(t, "b@ibm.com")

	cells := []dash.KVCacheSuggestion{
		{User: a, HourUTC: 9, Requests: 40, BestStrategy: kvcache.StrategyKeepAlive5m, SavingUSD: 1},
		{User: a, HourUTC: 10, Requests: 40, BestStrategy: kvcache.StrategyFixed5m}, // baseline arm
		{User: a, HourUTC: 11, Requests: 3, BestStrategy: kvcache.StrategyKeepAlive5m, InsufficientData: true},
		{User: b, HourUTC: 9, Requests: 40, BestStrategy: kvcache.StrategyStickySession1h}, // simulation-only
		{User: b, HourUTC: 10, Requests: 40, BestStrategy: kvcache.StrategyKeepAlive5m, UnpricedRequests: 2},
		{User: b, HourUTC: 11, Requests: 40, BestStrategy: kvcache.StrategyKeepAlive1h}, // needs the 1h tier
		{User: b, HourUTC: 30, Requests: 40, BestStrategy: kvcache.StrategyKeepAlive5m}, // hour out of range
	}
	noOneHonors1h := func(string) bool { return false }
	want := map[[2]any]bool{}
	for _, c := range cells {
		ok, _ := CampaignCellOutcome(c, noOneHonors1h)
		want[[2]any{c.User, float64(c.HourUTC)}] = ok
	}

	suggest := dash.KVCacheSuggestions{Baseline: kvcache.StrategyFixed5m,
		Weekdays: []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday"}, Cells: cells}
	body, _ := json.Marshal(map[string]any{"name": "agree", "source": "upload", "suggest": suggest})
	w, out := f.do(t, "POST", "/api/keepalive/campaigns", string(body), mgrJar)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	got, _ := out["cells"].([]any)
	if len(got) != len(cells) {
		t.Fatalf("route returned %d cells, want %d", len(got), len(cells))
	}
	for _, x := range got {
		c := x.(map[string]any)
		k := [2]any{c["tenant_id"], c["hour_utc"]}
		if _, known := want[k]; !known {
			t.Fatalf("route cell %v matches no uploaded cell: the response keys changed (%v)", k, c)
		}
		if act, _ := c["activatable"].(bool); act != want[k] {
			t.Errorf("cell %v: route activatable=%v, CampaignCellOutcome=%v (route reason %q)", k, act, want[k], c["skip_reason"])
		}
	}
}

// AN UPLOAD WITHOUT CANDIDATES IS ENOUGH. Offline tools drop each cell's per-arm Candidates to fit
// the control-plane body limit (a 121-cell payload with them was 509 KiB against 256). The route must
// activate exactly the same cells with or without them, because it reads only the winner and flags.
func TestCampaignCreateDoesNotNeedCandidates(t *testing.T) {
	cells := func(f *mgrFixture, with bool) []dash.KVCacheSuggestion {
		_, a := f.signUpJar(t, "a@ibm.com")
		c := []dash.KVCacheSuggestion{
			{User: a, HourUTC: 9, Requests: 40, BestStrategy: kvcache.StrategyKeepAlive5m, SavingUSD: 1, Valued: true},
			{User: a, HourUTC: 10, Requests: 40, BestStrategy: kvcache.StrategyKeepAlive5mOnce, SavingUSD: 2, Valued: true},
		}
		if with {
			for i := range c {
				c[i].Candidates = []kvcache.Savings{{Strategy: kvcache.StrategyFixed5m}, {Strategy: c[i].BestStrategy}}
			}
		}
		return c
	}
	activated := func(with bool) int {
		f := newMgrFixture(t)
		jar, _ := f.signUpJar(t, "boss@ibm.com")
		sug := dash.KVCacheSuggestions{Baseline: kvcache.StrategyFixed5m,
			Weekdays: []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday"}, Cells: cells(f, with)}
		body, _ := json.Marshal(map[string]any{"name": "lean", "source": "upload", "suggest": sug})
		w, out := f.do(t, "POST", "/api/keepalive/campaigns", string(body), jar)
		if w.Code != http.StatusCreated {
			t.Fatalf("create (candidates=%v) = %d %s", with, w.Code, w.Body)
		}
		n := 0
		for _, x := range out["cells"].([]any) {
			if x.(map[string]any)["activatable"] == true {
				n++
			}
		}
		return n
	}
	if with, without := activated(true), activated(false); with != without || with == 0 {
		t.Errorf("activated %d cells with candidates and %d without; the route must not need them", with, without)
	}
}
