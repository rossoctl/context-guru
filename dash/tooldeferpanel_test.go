package dash

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
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

// Tool deferral's net saving is its own lever on the Savings page (#888): a request row with
// defer_net_usd > 0 raises the total by exactly that, in every path that computes a total, and shows
// as its own series. An account that never deferred has no deferral figure and no series.
func TestDeferNetIsItsOwnLeverInTheTotal(t *testing.T) {
	plain := func(db *DB) {
		if err := db.InsertEvents([]*Event{mkEvent(20000*DayMs+1000, "s", "m", 100, 80)}); err != nil {
			t.Fatal(err)
		}
	}
	base := openTestDB(t)
	plain(base)
	with := openTestDB(t)
	plain(with)
	d := mkEvent(20000*DayMs+2000, "s", "m", 100, 100)
	d.DeferTokens, d.DeferGrossUSD, d.DeferNetUSD = 5000, 0.010, 0.004
	if err := with.InsertEvents([]*Event{d}); err != nil {
		t.Fatal(err)
	}
	// The same row without deferral, to compare against: its own compaction and cost terms.
	ref := openTestDB(t)
	plain(ref)
	if err := ref.InsertEvents([]*Event{mkEvent(20000*DayMs+2000, "s", "m", 100, 100)}); err != nil {
		t.Fatal(err)
	}
	f := Filter{TenantAll: true}
	for name, read := range map[string]func(*DB) (float64, float64, error){
		"overview": func(db *DB) (float64, float64, error) {
			o, err := db.Overview(f)
			if err != nil {
				return 0, 0, err
			}
			return o.TotalSavedUSD, o.DeferNetUSD, nil
		},
		"lean": func(db *DB) (float64, float64, error) {
			o, err := db.OverviewLean(f, nil, nil)
			if err != nil {
				return 0, 0, err
			}
			return o.TotalSavedUSD, o.DeferNetUSD, nil
		},
		"savings totals": func(db *DB) (float64, float64, error) {
			s, err := db.SavingsTotals(f)
			if err != nil {
				return 0, 0, err
			}
			return s.TotalSavedUSD, s.DeferNetUSD, nil
		},
	} {
		want, none, err := read(ref)
		if err != nil || none != 0 {
			t.Fatalf("%s: no-deferral account: defer = %g, err %v", name, none, err)
		}
		got, def, err := read(with)
		if err != nil {
			t.Fatal(err)
		}
		if math.Abs(def-0.004) > 1e-12 || math.Abs(got-want-0.004) > 1e-12 {
			t.Errorf("%s: defer = %g, total moved by %g, want exactly 0.004", name, def, got-want)
		}
	}

	// The waterfall holds the lever as its own step, and it adds up to the total.
	o, err := with.Overview(f)
	if err != nil {
		t.Fatal(err)
	}
	var step float64
	var seen bool
	for _, s := range o.Waterfall {
		if s.Key == "defer_net" {
			step, seen = -s.DeltaUSD, true
		}
	}
	if !seen || math.Abs(step-0.004) > 1e-12 {
		t.Errorf("waterfall defer_net step = %g (seen %v), want 0.004", step, seen)
	}

	// The daily series: its own field on the day that deferred, absent from the JSON otherwise.
	b, err := with.Series(f, DayMs)
	if err != nil || len(b) != 1 || math.Abs(b[0].DeferNetUSD-0.004) > 1e-12 {
		t.Fatalf("series = %+v, err %v", b, err)
	}
	if js, _ := json.Marshal(b[0]); !strings.Contains(string(js), "defer_net_usd") {
		t.Errorf("deferral day has no defer_net_usd series: %s", js)
	}
	rb, err := ref.Series(f, DayMs)
	if err != nil || len(rb) != 1 {
		t.Fatalf("ref series = %+v, err %v", rb, err)
	}
	if js, _ := json.Marshal(rb[0]); strings.Contains(string(js), "defer_net_usd") {
		t.Errorf("an account with no deferral has a deferral series: %s", js)
	}
}
