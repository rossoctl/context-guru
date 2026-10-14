package main

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/dash"
)

// TestDashEmitterKeepAlivePingCarriesNoWaitCause pins issue #424's own requirement: a
// keep-alive ping — plain (record1) or the cache_aware_summarizer's summary-substitute
// (#402, recordSummarySubstitute) — must never get a waiting_for/wake_cause label, and must
// never touch internal/waitcause's per-thread state.
//
// dashEmitter.KeepAlivePing is the ONLY place a ping's dash.Event is built (see its own doc
// comment); proxy/keepalive.go's record1 and recordSummarySubstitute both reach it through
// the SAME components.KeepAliveEmitter.KeepAlivePing call, built from a components.
// KeepAliveReport that carries no wait-cause fields at all — there is no path from either
// ping sender to the dash.Event's WaitingFor/WakeCause/OpenBackground/OpenTeammate/
// MonitorExpiryMs fields, which is what this test fixes in place.
//
// It also proves the "no tracker touch" half structurally rather than by assertion:
// dashEmitter holds no *waitcause.Tracker at all (see its struct, two fields: rec, prices),
// so there is nothing for KeepAlivePing to call Observe/RecordOutcome on. A later change that
// added such a field and called it would have to do so visibly, in this same file.
func TestDashEmitterKeepAlivePingCarriesNoWaitCause(t *testing.T) {
	rec, err := dash.NewRecorder(dash.Options{
		DBPath:        filepath.Join(t.TempDir(), "dash.db"),
		BatchSize:     1,
		FlushInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	em := &dashEmitter{rec: rec}
	em.KeepAlivePing(components.KeepAliveReport{
		Tenant: "t1", Session: "s1", Thread: "a:sub1", Model: "aws/claude-sonnet-5",
		Provider: "anthropic", Route: "/v1/messages", Pings: 1,
		CacheRead: 48576, Status: 200, DurationMs: 120, TS: time.Now().UnixMilli(),
	})

	page, err := waitForRow(t, rec)
	if err != nil {
		t.Fatal(err)
	}
	e := page.Requests[0]
	if !e.KeepAlive {
		t.Fatalf("the written row does not even read as a ping (KeepAlive=false): %+v", e)
	}
	if e.WaitingFor != "" || e.WaitTool != "" || e.WakeCause != "" ||
		e.OpenBackground != 0 || e.OpenTeammate != 0 || e.MonitorExpiryMs != 0 {
		t.Fatalf("a ping row must carry no wait-cause fields at all, got waiting_for=%q "+
			"wait_tool=%q wake_cause=%q open_background=%d open_teammate=%d monitor_expiry_ms=%d",
			e.WaitingFor, e.WaitTool, e.WakeCause, e.OpenBackground, e.OpenTeammate, e.MonitorExpiryMs)
	}
}

// TestDashEmitterSummarySubstitutePingCarriesNoWaitCause is the same assertion for the
// cache_aware_summarizer's keep-alive-substitute ping (#402): recordSummarySubstitute reaches
// the identical KeepAlivePing call, with the identical report shape, so there is only one
// place this could ever regress and this test is it.
func TestDashEmitterSummarySubstitutePingCarriesNoWaitCause(t *testing.T) {
	rec, err := dash.NewRecorder(dash.Options{
		DBPath:        filepath.Join(t.TempDir(), "dash.db"),
		BatchSize:     1,
		FlushInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	em := &dashEmitter{rec: rec}
	// recordSummarySubstitute's own report: MaxTokens 0 (a full summary, not a 1-token read),
	// no Strategy set — the two fields that distinguish it from record1's report. Neither
	// bears on wait-cause, which is the point: this report shape has no field that could.
	em.KeepAlivePing(components.KeepAliveReport{
		Tenant: "t1", Session: "s1", Thread: "", Model: "aws/claude-sonnet-5",
		Provider: "anthropic", Route: "/v1/messages", Pings: 1,
		CacheRead: 9000, Status: 200, DurationMs: 900, TS: time.Now().UnixMilli(), MaxTokens: 0,
	})

	page, err := waitForRow(t, rec)
	if err != nil {
		t.Fatal(err)
	}
	e := page.Requests[0]
	if e.WaitingFor != "" || e.WakeCause != "" {
		t.Fatalf("a summary-substitute ping row must carry no wait-cause fields, got waiting_for=%q wake_cause=%q",
			e.WaitingFor, e.WakeCause)
	}
}

// waitForRow polls the recorder's own database for the one row this test just wrote. The
// writer goroutine is asynchronous even with BatchSize 1 (it still goes through the capture
// channel), so a direct query right after Record/KeepAlivePing can race it.
func waitForRow(t *testing.T, rec *dash.Recorder) (*dash.Page, error) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		page, err := rec.DB().Requests(dash.Filter{TenantAll: true, WithKeepAlive: true}, 0, 10)
		if err != nil {
			return nil, err
		}
		if len(page.Requests) == 1 {
			return page, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for the writer to flush the ping row (have %d rows, want 1)",
				len(page.Requests))
		}
		time.Sleep(5 * time.Millisecond)
	}
}
