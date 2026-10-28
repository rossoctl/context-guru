package proxy

import (
	"fmt"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/components"
	cf "github.com/rossoctl/context-guru/counterfactual"
)

var pingAt = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

func okReport() components.KeepAliveReport {
	return components.KeepAliveReport{Session: "s", Thread: "", Model: "m", Route: "h", Status: 200,
		CacheRead: 5000, CacheWrite: 300, CacheWrite1h: 100, FreshInput: 10, Output: 1,
		CostUSD: 9.99, TS: pingAt.UnixMilli(), Window: pingAt.Add(-4 * time.Minute)}
}

func TestPingFromReportMapsEveryField(t *testing.T) {
	p := pingFromReport(okReport())
	want := cf.Ping{Session: "s", ThreadID: "", Model: "m", Host: "h",
		Usage: cf.CallUsage{Usage: cf.Usage{Input: 10, CacheRead: 5000, CacheWrite: 300, CacheWrite1h: 100}, Output: 1},
		At:    time.UnixMilli(pingAt.UnixMilli()), Window: pingAt.Add(-4 * time.Minute)}
	if p != want {
		t.Fatalf("got %+v want %+v", p, want)
	}
	if p.Usage.CostUSD != 0 {
		t.Fatal("the host prices a ping with its own rates")
	}
}

func TestPingSavingsPricesAndStoresFacts(t *testing.T) {
	st := NewPingStore()
	res, ok := PingSavings(okReport(), st)
	if !ok || len(res.Ops) != 1 || res.Ops[0].Call == nil {
		t.Fatalf("ok=%v res=%+v", ok, res)
	}
	f, ok := st.take(cf.ThreadKey("s", cf.Thread{ID: "main"}))
	if !ok || f.PingWindow == "" || !f.LastPing.Equal(time.UnixMilli(pingAt.UnixMilli())) {
		t.Fatalf("facts %+v ok=%v", f, ok)
	}
}

func TestPingSavingsSkipRules(t *testing.T) {
	cases := map[string]func(*components.KeepAliveReport){
		"zero window": func(r *components.KeepAliveReport) { r.Window = time.Time{} },
		"status 500":  func(r *components.KeepAliveReport) { r.Status = 500 },
		"status 0":    func(r *components.KeepAliveReport) { r.Status = 0 },
		"zero usage": func(r *components.KeepAliveReport) {
			r.CacheRead, r.CacheWrite, r.CacheWrite1h, r.FreshInput, r.Output = 0, 0, 0, 0, 0
		},
	}
	for name, mut := range cases {
		st := NewPingStore()
		r := okReport()
		mut(&r)
		if _, ok := PingSavings(r, st); ok {
			t.Errorf("%s: converted", name)
		}
		if len(st.m) != 0 {
			t.Errorf("%s: facts stored", name)
		}
	}
	for name, mut := range map[string]func(*components.KeepAliveReport){
		"manual": func(r *components.KeepAliveReport) { r.Manual = true; r.Strategy = "manual" },
		"sweep":  func(r *components.KeepAliveReport) { r.SweepAnswerUSD = 0.5 },
	} {
		r := okReport()
		mut(&r)
		if _, ok := PingSavings(r, NewPingStore()); !ok {
			t.Errorf("%s: not converted", name)
		}
	}
}

func TestPingSavingsNilStoreIsFine(t *testing.T) {
	if _, ok := PingSavings(okReport(), nil); !ok {
		t.Fatal("result must still be returned")
	}
}

func TestPingStoreTakeDeletesAndOverwriteKeepsLatest(t *testing.T) {
	s := NewPingStore()
	s.put("k", cf.PingFacts{PingWindow: "a"})
	s.put("k", cf.PingFacts{PingWindow: "b"})
	f, ok := s.take("k")
	if !ok || f.PingWindow != "b" {
		t.Fatalf("%+v %v", f, ok)
	}
	if _, ok := s.take("k"); ok {
		t.Fatal("take must delete")
	}
	var nilStore *PingStore
	nilStore.put("k", cf.PingFacts{})
	if _, ok := nilStore.take("k"); ok {
		t.Fatal("nil store")
	}
}

func TestPingStoreIsBounded(t *testing.T) {
	s := NewPingStore()
	for i := 0; i < pingStoreMax+500; i++ {
		s.put(fmt.Sprint("k", i), cf.PingFacts{PingWindow: "w"})
	}
	if len(s.m) > pingStoreMax {
		t.Fatalf("len %d > %d", len(s.m), pingStoreMax)
	}
	if _, ok := s.take(fmt.Sprint("k", pingStoreMax+499)); !ok {
		t.Fatal("the newest entry must survive")
	}
}

func TestEntryFactsConsumesPingFactsOnce(t *testing.T) {
	st := NewPingStore()
	PingSavings(okReport(), st)
	th := cf.Thread{ID: "main"}
	f := entryFacts(nil, st, "s", th)
	if f.LastPing.IsZero() || f.PingWindow == "" || f.PingRead != nil {
		t.Fatalf("%+v", f)
	}
	g := entryFacts(nil, st, "s", th)
	if !g.LastPing.IsZero() || g.PingWindow != "" {
		t.Fatalf("second request got %+v", g)
	}
	if entryFacts(nil, nil, "s", th).TTL != 5*time.Minute {
		t.Fatal("nil store changes nothing")
	}
}

func TestNamedThreadKeyRoundTrips(t *testing.T) {
	st := NewPingStore()
	r := okReport()
	r.Thread = "sub-1"
	if _, ok := PingSavings(r, st); !ok {
		t.Fatal("not converted")
	}
	if f := entryFacts(nil, st, "s", cf.Thread{ID: "main"}); !f.LastPing.IsZero() {
		t.Fatal("the main thread must not take a named thread's ping")
	}
	f := entryFacts(nil, st, "s", cf.Thread{ID: "sub-1"})
	if f.LastPing.IsZero() || f.PingWindow == "" {
		t.Fatalf("%+v", f)
	}
}
