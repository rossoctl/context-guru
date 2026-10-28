package main

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/proxy"
)

func pingReport() components.KeepAliveReport {
	now := time.Now()
	return components.KeepAliveReport{Tenant: "t", Session: "s1", Model: "aws/claude-sonnet-5",
		Provider: "anthropic", Route: "/v1/messages", CacheRead: 48576, CostUSD: 0.0098, Status: 200,
		TS: now.UnixMilli(), Window: now.Add(-4 * time.Minute)}
}

// tapped runs one ping through a dashEmitter in the given mode and returns the recorded event as
// the recorder's tap saw it (the tap, unlike the store, keeps the counterfactual Result).
func tapped(t *testing.T, mode string) dash.Event {
	t.Helper()
	var mu sync.Mutex
	var got []dash.Event
	rec, err := dash.NewRecorder(dash.Options{DBPath: filepath.Join(t.TempDir(), "d.db"),
		BatchSize: 1, FlushInterval: time.Millisecond,
		Tap: func(e *dash.Event) { mu.Lock(); got = append(got, *e); mu.Unlock() }})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	em := &dashEmitter{rec: rec, savings: mode, pings: proxy.NewPingStore()}
	em.KeepAlivePing(pingReport())
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 1 {
			break
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("%s: events = %d", mode, len(got))
	}
	return got[0]
}

func TestDashEmitterPingSavingsByMode(t *testing.T) {
	legacy := tapped(t, proxy.SavingsLegacy)
	if legacy.HasCounterfactual() {
		t.Fatal("legacy mode must attach no counterfactual result")
	}
	for _, mode := range []string{proxy.SavingsShadow, proxy.SavingsCounterfactual} {
		ev := tapped(t, mode)
		res, ok := ev.Counterfactual()
		if !ok || len(res.Ops) != 1 || res.Ops[0].Call == nil {
			t.Fatalf("%s: result %+v ok=%v", mode, res, ok)
		}
		if ev.CostUSD != legacy.CostUSD || ev.CacheRead != legacy.CacheRead ||
			ev.BaselineCostUSD != legacy.BaselineCostUSD || ev.KeepAliveSavedUSD != legacy.KeepAliveSavedUSD {
			t.Errorf("%s changed the legacy figures", mode)
		}
	}
}
