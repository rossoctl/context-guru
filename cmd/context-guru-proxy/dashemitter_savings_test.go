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

// tapped runs one ping through a dashEmitter and returns the recorded event as the recorder's tap
// saw it (the tap, unlike the store, keeps the accountant Result).
func tapped(t *testing.T) dash.Event {
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
	em := &dashEmitter{rec: rec, pings: proxy.NewPingStore()}
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
		t.Fatalf("events = %d", len(got))
	}
	return got[0]
}

func TestDashEmitterAttachesTheAccountantResultToAPing(t *testing.T) {
	ev := tapped(t)
	res, ok := ev.AccountantResult()
	if !ok || len(res.Ops) != 1 || res.Ops[0].Call == nil {
		t.Fatalf("result %+v ok=%v, want one spend with its call", res, ok)
	}
	// The override changes components and the baseline of a REQUEST; a ping is not one, so the
	// legacy figures stay as Price set them.
	if ev.CostUSD != pingReport().CostUSD || ev.CacheRead != pingReport().CacheRead {
		t.Errorf("a ping's cost or cache read changed: cost %v read %v", ev.CostUSD, ev.CacheRead)
	}
}
