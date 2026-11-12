package dash

import (
	"net/http"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// bobPrice is Bob's flat rate from bob-plugin/context-guru/prices.yaml: 2.0 Bobcoins per million.
var bobPrice = modelinfo.Price{Input: 2e-6, Output: 2e-6, CacheRead: 2e-6, CacheWrite: 2e-6, Currency: "Bobcoins"}

// pricedEvent is one request priced the way the capture path prices it (Event.Price).
func pricedEvent(ts int64, session, agent string, p modelinfo.Price) *Event {
	e := mkEvent(ts, session, "premium", 1000, 1000)
	e.Agent, e.Components = agent, nil
	e.Price(p, true)
	return e
}

// A recorded row keeps the unit of the price that costed it, so a sum can tell Bobcoins from dollars.
func TestRecordedRowStoresTheCurrencyOfItsPrice(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UnixMilli()
	unpriced := mkEvent(now+2, "s3", "x", 1000, 1000)
	unpriced.Components = nil
	unpriced.Price(modelinfo.Price{}, true)
	// The proxy's capture path prices with PriceMode (proxy/dashcapture.go), the host's with Price.
	bob := mkEvent(now, "s1", "premium", 1000, 1000)
	bob.Agent, bob.Components = "bob", nil
	bob.PriceMode(bobPrice, true)
	if err := db.insertBatch([]*Event{
		bob,
		pricedEvent(now+1, "s2", "claude-code", ibmSonnet),
		unpriced,
	}); err != nil {
		t.Fatal(err)
	}
	p, err := db.Requests(Filter{}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range p.Requests {
		got[e.SessionID] = e.Currency
	}
	want := map[string]string{"s1": "Bobcoins", "s2": "USD", "s3": "USD"}
	for s, w := range want {
		if got[s] != w {
			t.Errorf("session %s: currency %q, want %q (all: %v)", s, got[s], w, got)
		}
	}
}

// A view has one unit. Every money reply says which: the unit of its priced rows, or mixed.
func TestMoneyRepliesSayTheViewsCurrency(t *testing.T) {
	now := time.Now().UnixMilli()
	cases := []struct {
		name string
		evs  func() []*Event
		want string
	}{
		{"bob only", func() []*Event { return []*Event{pricedEvent(now, "s1", "bob", bobPrice)} }, "Bobcoins"},
		{"claude only", func() []*Event { return []*Event{pricedEvent(now, "s1", "claude-code", ibmSonnet)} }, "USD"},
		{"both", func() []*Event {
			return []*Event{pricedEvent(now, "s1", "bob", bobPrice), pricedEvent(now+1, "s1", "claude-code", ibmSonnet)}
		}, "mixed"},
		{"none", func() []*Event { return nil }, "USD"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, rec := newTestAPI(t, Options{})
			if evs := c.evs(); len(evs) > 0 {
				seed(t, rec, evs...)
			}
			for _, path := range []string{"/api/stats", "/api/stats?lean=1&session=s1", "/api/series?bucket=60000",
				"/api/sessions", "/api/requests", "/api/cachestate?session=s1"} {
				w, body := get(t, a, path, "127.0.0.1:1")
				if w.Code != http.StatusOK {
					t.Errorf("%s -> %d: %s", path, w.Code, w.Body.String())
					continue
				}
				if body["currency"] != c.want {
					t.Errorf("%s: currency %v, want %q", path, body["currency"], c.want)
				}
			}
		})
	}
}

// The contract the Bob status bar reads (#1124): `currency` is a top-level string of /api/cachestate.
// It is "USD" also when no row is priced, "mixed" in lower case, and otherwise the price file's name.
func TestCacheStateCurrencyContract(t *testing.T) {
	now := time.Now().UnixMilli()
	unpriced := mkEvent(now, "s1", "premium", 1000, 1000)
	unpriced.Components = nil
	unpriced.Price(modelinfo.Price{}, true)
	for _, c := range []struct {
		name string
		evs  []*Event
		want string
	}{
		{"unpriced only", []*Event{unpriced}, "USD"},
		{"bob", []*Event{pricedEvent(now, "s1", "bob", bobPrice)}, "Bobcoins"},
		{"two units", []*Event{pricedEvent(now, "s1", "bob", bobPrice), pricedEvent(now+1, "s1", "claude-code", ibmSonnet)}, "mixed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, rec := newTestAPI(t, Options{})
			seed(t, rec, c.evs...)
			w, body := get(t, a, "/api/cachestate?session=s1", "127.0.0.1:1")
			if w.Code != http.StatusOK {
				t.Fatalf("-> %d: %s", w.Code, w.Body.String())
			}
			got, isString := body["currency"].(string)
			if !isString || got != c.want {
				t.Fatalf("top-level currency = %#v, want the string %q", body["currency"], c.want)
			}
		})
	}
}

// The agent filter scopes the unit too: on a mixed proxy, Bob's own view is in Bobcoins.
func TestCurrencyFollowsTheFilter(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	now := time.Now().UnixMilli()
	seed(t, rec, pricedEvent(now, "s1", "bob", bobPrice), pricedEvent(now+1, "s2", "claude-code", ibmSonnet))
	for path, want := range map[string]string{"/api/stats?agent=bob": "Bobcoins", "/api/stats?agent=claude-code": "USD"} {
		_, body := get(t, a, path, "127.0.0.1:1")
		if body["currency"] != want {
			t.Errorf("%s: currency %v, want %q", path, body["currency"], want)
		}
	}
}

// Every tab that shows money reads a route that says the unit, or the tab would keep the unit of the
// last page. So every route the UI reads money from is in currencyRoutes, and each says Bobcoins here.
func TestEveryMoneyRouteSaysTheUnit(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	seed(t, rec, pricedEvent(time.Now().UnixMilli(), "s1", "bob", bobPrice))
	for _, p := range []string{"/api/components", "/api/breakdown", "/api/components/compaction-episodes",
		"/api/keepalive", "/api/keepalive/behaviour", "/api/keepalive/calc", "/api/keepalive/live",
		"/api/keepalive/recommend", "/api/keepalive/sessions", "/api/tools", "/api/tool-defer", "/api/toolfilter",
		"/api/kvcache", "/api/kvcache/rows", "/api/kvcache/simulate", "/api/kvcache/pricing",
		"/api/kvcache/suggest", "/api/insights", "/api/spend/explain"} {
		if !currencyRoutes["GET "+p] {
			t.Errorf("%s shows money and is not in currencyRoutes", p)
			continue
		}
		w, body := get(t, a, p, "127.0.0.1:1")
		if w.Code != http.StatusOK || body["currency"] != "Bobcoins" {
			t.Errorf("%s -> %d, currency %v; want 200 and Bobcoins", p, w.Code, body["currency"])
		}
	}
}
