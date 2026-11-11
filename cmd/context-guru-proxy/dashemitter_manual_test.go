package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/dash"
)

// A manual ping is written as a keepalive row flagged ManualPing, with the write it avoided beside
// the cost it paid, so the dashboard counts it as its own category and no strategy ledger does.
func TestDashEmitterWritesAManualPingRow(t *testing.T) {
	rec, err := dash.NewRecorder(dash.Options{
		DBPath: filepath.Join(t.TempDir(), "dash.db"), BatchSize: 1, FlushInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	em := &dashEmitter{rec: rec}
	em.KeepAlivePing(components.KeepAliveReport{
		Tenant: "t1", Session: "s1", Model: "aws/claude-sonnet-5", Provider: "anthropic",
		Route: "/v1/messages", CacheRead: 48576, CostUSD: 0.0098, Status: 200,
		TS: time.Now().UnixMilli(), Manual: true, Strategy: "manual", RewriteUSD: 0.18,
	})
	em.KeepAlivePing(components.KeepAliveReport{
		Tenant: "t1", Session: "s1", Model: "aws/claude-sonnet-5", Provider: "anthropic",
		Route: "/v1/messages", CacheRead: 48576, CostUSD: 0.0098, Status: 200,
		TS: time.Now().UnixMilli(), Strategy: "strat-1",
	})

	var page *dash.Page
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		page, err = rec.DB().Requests(dash.Filter{TenantAll: true, WithKeepAlive: true}, 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Requests) == 2 {
			break
		}
	}
	if len(page.Requests) != 2 {
		t.Fatalf("rows = %d, want 2", len(page.Requests))
	}
	var manual, auto *dash.Event
	for _, e := range page.Requests {
		if e.ManualPing {
			manual = e
		} else {
			auto = e
		}
	}
	if manual == nil || auto == nil {
		t.Fatalf("want one manual and one automatic ping row: %+v", page.Requests)
	}
	if !manual.KeepAlive || manual.ManualSavedUSD != 0.18 || manual.CostUSD != 0.0098 {
		t.Errorf("manual row = keepalive %v saved %v cost %v", manual.KeepAlive, manual.ManualSavedUSD, manual.CostUSD)
	}
	if auto.ManualSavedUSD != 0 {
		t.Errorf("an automatic ping carries a manual credit: %v", auto.ManualSavedUSD)
	}
	led, err := rec.DB().KeepAliveLedger(dash.Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if led.Pings != 1 {
		t.Errorf("keep-alive ledger counts %d pings, want only the automatic one", led.Pings)
	}
}
