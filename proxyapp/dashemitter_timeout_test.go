package proxyapp

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/testutil"
)

// #754. A timed-out ping's row is marked timeout_estimated. The keep-alive ledger counts it as a
// ping and as spend, counts it in pings_timed_out, and does not count it as a ping that read
// nothing: a timeout says nothing about the cache.
func TestDashEmitterBooksATimedOutPingAsEstimatedSpend(t *testing.T) {
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
		Route: "/v1/messages", CostUSD: 0.015, DurationMs: 60_000,
		TS: time.Now().UnixMilli(), TimedOut: true,
	})

	var page *dash.Page
	testutil.WaitForState(t, "the writer to flush 1 row", func() bool {
		page, err = rec.DB().Requests(dash.Filter{TenantAll: true, WithKeepAlive: true}, 0, 10)
		return err != nil || len(page.Requests) == 1
	}, func() string { return fmt.Sprintf("rows=%d err=%v", len(page.Requests), err) })
	if err != nil {
		t.Fatal(err)
	}
	row := page.Requests[0]
	if !row.KeepAlive || row.StopReason != dash.StopTimeoutEstimated || row.CostUSD != 0.015 {
		t.Errorf("row = keepalive %v stop_reason %q cost %v, want true, %q, 0.015",
			row.KeepAlive, row.StopReason, row.CostUSD, dash.StopTimeoutEstimated)
	}
	led, err := rec.DB().KeepAliveLedger(dash.Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if led.Pings != 1 || led.PingUSD != 0.015 || led.PingsTimedOut != 1 || led.PingsThatReadNothing != 0 {
		t.Errorf("ledger pings %d ping_usd %v timed_out %d read_nothing %d, want 1, 0.015, 1, 0",
			led.Pings, led.PingUSD, led.PingsTimedOut, led.PingsThatReadNothing)
	}
}
