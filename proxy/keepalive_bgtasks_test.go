package proxy

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/waitcause"
	"github.com/rossoctl/context-guru/keepalive"
)

const bgStartBody = `{"model":"claude-sonnet-5-5","max_tokens":100,"messages":[
{"role":"user","content":"build"},
{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"Bash","input":{"command":"make","run_in_background":true}}]},
{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"Command running in background with ID: bash1."}]}]}`

// #1225, end to end: the keeper plans a turn that starts a background task as a wait for a person
// (3 pings, the counts are those of the request before). The dashboard writer reads the body in
// order, finds "background" and tells the keeper, which raises the entry to base + 3 = 5. Without the
// writer's call the entry keeps 3.
func TestWriterRaisesThePingPlanOfATurnThatStartsBackgroundWork(t *testing.T) {
	for _, tc := range []struct {
		name  string
		wire  bool
		want  int
		wantR int // the row's ka_planned_pings
	}{
		{"writer tells the keeper", true, 5, 5},
		{"no call from the writer (the old behavior)", false, 3, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, _, clock := testKeeper(t, Limits{})
			h := k.h
			rec, err := dash.NewRecorder(dash.Options{DBPath: filepath.Join(t.TempDir(), "d.db"), BatchSize: 1, FlushInterval: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { rec.Close() })
			h.rec, h.waits, h.keeper = rec, waitcause.New(), k
			pol := CachePolicy{KeepAlive: true, WaitPlan: true, Idle: 270 * time.Second, MaxPings: 2, MinPrefixTokens: 20000}
			recordResp(t, k, pol, "", clock.now(), Usage{StopReason: "end_turn"})
			key := keepalive.Key{Tenant: "t1", Session: "sess-1"}
			if v, ok := k.Keeper.Entry(key); !ok || v.Policy.MaxPings != 3 {
				t.Fatalf("planned at response time: held=%v pings=%+v, want 3 (the count of the request before)", ok, v.Policy)
			}
			c := &capture{rec: rec, waits: h.waits, tenant: "t1", provider: "anthropic", model: "claude-sonnet-5-5",
				route: "/v1/messages", start: time.Now(), status: 200, trace: apply.Trace{Session: "sess-1"}}
			if tc.wire {
				c.raisePlan = h.raiseWaitPlan
			}
			c.noteWaitBody([]byte(bgStartBody))
			c.finish(Usage{FreshInput: 10, CacheRead: 48576, CacheWrite: 100, StopReason: "end_turn"}, true, false, 0, 0)
			waitForRows(t, rec, 1)
			if v, ok := k.Keeper.Entry(key); !ok || v.Policy.MaxPings != tc.want {
				t.Fatalf("after the writer: held=%v pings=%d, want %d", ok, v.Policy.MaxPings, tc.want)
			}
			page, err := rec.DB().Requests(dash.Filter{Tenant: "t1"}, 0, 1)
			if err != nil || len(page.Requests) != 1 {
				t.Fatalf("row: %v %v", page, err)
			}
			row := page.Requests[0]
			if row.WaitingFor != "background" {
				t.Fatalf("waiting_for = %q, want background", row.WaitingFor)
			}
			if tc.wire && (row.KAPlannedPings == nil || *row.KAPlannedPings != tc.wantR) {
				t.Errorf("ka_planned_pings = %v, want %d", row.KAPlannedPings, tc.wantR)
			}
		})
	}
}
