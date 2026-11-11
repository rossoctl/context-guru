package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/thread"
	"github.com/rossoctl/context-guru/metrics"
)

// returnAfterPings prices one warm return that followed one keep-alive ping, the way a host does. The
// host decides how it wires the ping side: pingStore is what the capture reads (nil = Options.Pings
// unset), and callPingSavings says whether the host priced the ping at all.
func returnAfterPings(t *testing.T, pingStore *PingStore, callPingSavings bool) (*savingsTracker, *PingStore) {
	t.Helper()
	body := []byte(`{"model":"m","system":[{"type":"text","text":"sys"}],"messages":[{"role":"user","content":"hi"}]}`)
	rec, err := dash.NewRecorder(dash.Options{DBPath: filepath.Join(t.TempDir(), "d.db"), BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	if callPingSavings {
		if _, ok := PingSavings(okReport(), pingStore); !ok {
			t.Fatal("the ping was not priced")
		}
	}
	sv := newSavingsTracker()
	c := &capture{rec: rec, savings: sv, pings: pingStore, tenant: "t1", provider: "anthropic",
		model: "m", route: "/v1/messages", start: time.Now(), status: 200, trace: apply.Trace{Session: "s"}}
	c.noteKeepAlive(kaArrival{pings: 1, refreshed: 100_000, refreshedAt: time.Now()})
	c.accBegin(accountant.RequestMeta{RequestID: "r1", Session: "s", Model: "m", At: time.Now()}, body)
	c.accEnd(thread.Result{ID: thread.Primary}, body, nil)
	c.finish(Usage{FreshInput: 10, CacheRead: 100}, true, false, 0, 0)
	return sv, pingStore
}

// A host wired like cmd/context-guru-proxy prices the ping and shares the store: the return finds the
// facts, so nothing is missing and the ping counts as priced.
func TestWiredHostMissesNoPingFacts(t *testing.T) {
	sv, st := returnAfterPings(t, NewPingStore(), true)
	if got := sv.RescueFactsMissing(); got != 0 {
		t.Errorf("facts missing = %d, want 0", got)
	}
	if got := st.Priced(); got != 1 {
		t.Errorf("pings priced = %d, want 1", got)
	}
}

// The three ways a host fails to wire the ping side all show as one missing-facts count.
func TestUnwiredHostCountsMissingPingFacts(t *testing.T) {
	for name, tc := range map[string]struct {
		store *PingStore
		call  bool
	}{
		"no store (Options.Pings nil)": {nil, true},
		"PingSavings never called":     {NewPingStore(), false},
	} {
		t.Run(name, func(t *testing.T) {
			sv, _ := returnAfterPings(t, tc.store, tc.call)
			if got := sv.RescueFactsMissing(); got != 1 {
				t.Errorf("facts missing = %d, want 1", got)
			}
		})
	}
}

// A return with no pings in its gap has nothing to find, so it is not counted.
func TestNoPingsInTheGapIsNotMissing(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	rec, err := dash.NewRecorder(dash.Options{DBPath: filepath.Join(t.TempDir(), "d.db"), BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	sv := newSavingsTracker()
	c := &capture{rec: rec, savings: sv, tenant: "t1", provider: "anthropic", model: "m",
		route: "/v1/messages", start: time.Now(), status: 200, trace: apply.Trace{Session: "s"}}
	c.accBegin(accountant.RequestMeta{RequestID: "r1", Session: "s", Model: "m", At: time.Now()}, body)
	c.accEnd(thread.Result{ID: thread.Primary}, body, nil)
	c.finish(Usage{FreshInput: 10, CacheRead: 100}, true, false, 0, 0)
	if got := sv.RescueFactsMissing(); got != 0 {
		t.Errorf("facts missing = %d, want 0", got)
	}
}

// A ping that is not priced does not count, and a manual ping does.
func TestPricedCountsOnlyPricedPings(t *testing.T) {
	st := NewPingStore()
	bad := okReport()
	bad.Status = 500
	PingSavings(bad, st)
	if st.Priced() != 0 {
		t.Fatalf("a rejected ping counted: %d", st.Priced())
	}
	man := okReport()
	man.Manual = true
	PingSavings(man, st)
	PingSavings(okReport(), st)
	if st.Priced() != 2 {
		t.Errorf("priced = %d, want 2", st.Priced())
	}
	var nilStore *PingStore
	if nilStore.Priced() != 0 {
		t.Error("nil store must read 0")
	}
}

// /stats and /metrics carry both counters.
func TestStatsAndMetricsCarryTheCounters(t *testing.T) {
	st := NewPingStore()
	PingSavings(okReport(), st)
	h := New(nil, nil, metrics.NewAggregator(), Options{Pings: st})
	h.savings = newSavingsTracker()
	h.savings.noteRescueFactsMissing()
	h.savings.noteRescueFactsMissing()

	w := httptest.NewRecorder()
	h.stats(w, httptest.NewRequest("GET", "/stats", nil))
	var got map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if string(got["keepalive_pings_priced"]) != "1" || string(got["keepalive_rescue_facts_missing"]) != "2" {
		t.Errorf("stats = priced %s, missing %s; want 1 and 2", got["keepalive_pings_priced"], got["keepalive_rescue_facts_missing"])
	}
	if m := h.renderMetrics(); !strings.Contains(m, "cg_keepalive_rescue_facts_missing_total 2") ||
		!strings.Contains(m, "cg_keepalive_pings_priced_total 1") {
		t.Errorf("metrics lack the counter:\n%s", m)
	}
}
