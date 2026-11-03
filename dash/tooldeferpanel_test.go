package dash

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The panel sums deferral's per-request facts per account and per UTC day. A request deferral did
// not change is not a row of it, and net is gross minus the names list and the search costs.
func TestToolDeferReportPerAccountAndDay(t *testing.T) {
	db := openTestDB(t)
	day0 := int64(20000) * DayMs
	ev := func(ts int64, tenant string, tok, searches, loaded, wrong int, gross, net float64) *Event {
		e := mkEvent(ts, tenant+":s", "m", 100, 100)
		e.TenantID = tenant
		e.DeferTokens, e.DeferListTokens, e.DeferSearches = tok, 20, searches
		e.DeferLoaded, e.DeferWrongGuesses, e.DeferGrossUSD, e.DeferNetUSD = loaded, wrong, gross, net
		e.DeferDroppedSearches = searches / 2
		return e
	}
	if err := db.InsertEvents([]*Event{
		ev(day0+1000, "a", 5000, 0, 0, 0, 0.010, 0.009),
		ev(day0+2000, "a", 5000, 1, 1, 1, 0.010, 0.004),
		ev(day0+3000, "b", 3000, 0, 0, 0, 0.006, 0.005),
		ev(day0+DayMs+5, "a", 4000, 2, 2, 0, 0.008, 0.001),
		ev(day0+DayMs+6, "a", 0, 0, 0, 0, 0, 0),    // deferral did not change it
		ev(day0+DayMs+7, "a", 4000, 0, 0, 0, 0, 0), // unpriced model
	}); err != nil {
		t.Fatal(err)
	}

	rep, err := db.ToolDeferReportFor(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Days) != 3 {
		t.Fatalf("days = %+v, want 3 rows (a day 1, a day 0, b day 0)", rep.Days)
	}
	first := rep.Days[0]
	if first.Day != day0+DayMs || first.Tenant != "a" || first.Requests != 2 || first.TokensDeferred != 8000 ||
		first.Searches != 2 || first.Loaded != 2 || first.WrongGuesses != 0 || first.Unpriced != 1 {
		t.Errorf("newest row = %+v", first)
	}
	a0 := rep.Days[1]
	if a0.Tenant != "a" || a0.Requests != 2 || a0.TokensDeferred != 10000 || a0.Searches != 1 || a0.WrongGuesses != 1 ||
		math.Abs(a0.GrossUSD-0.020) > 1e-12 || math.Abs(a0.NetUSD-0.013) > 1e-12 || math.Abs(a0.CostUSD-0.007) > 1e-12 {
		t.Errorf("tenant a, day 0 = %+v", a0)
	}
	if rep.Total.Requests != 5 || rep.Total.TokensDeferred != 21000 || rep.Total.WrongGuesses != 1 || rep.Total.DroppedSearches != 1 ||
		math.Abs(rep.Total.NetUSD-0.019) > 1e-12 {
		t.Errorf("total = %+v", rep.Total)
	}

	// One account's scope sees only its own rows.
	own, err := db.ToolDeferReportFor(Filter{Tenant: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(own.Days) != 1 || own.Days[0].Tenant != "b" || own.Total.Requests != 1 {
		t.Errorf("tenant b = %+v", own)
	}
}

// The route answers with an empty list, not null, on a proxy that never deferred.
func TestToolDeferRouteEmpty(t *testing.T) {
	a, _ := newTestAPI(t, Options{})
	m := http.NewServeMux()
	a.Mount(m)
	req := httptest.NewRequest(http.MethodGet, "/api/tool-defer", nil)
	req.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/tool-defer: %d %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if days, ok := got["days"].([]any); !ok || len(days) != 0 {
		t.Errorf("days = %v, want []", got["days"])
	}
}
