package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/testutil"
	"github.com/rossoctl/context-guru/internal/waitcause"
	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/context-guru/tenant"
)

// recordBgTurn records two turns on sess-1: a tool call, then a response with the usage u.
func recordBgTurn(t *testing.T, k *keeper, pol CachePolicy, at time.Time, u Usage) {
	t.Helper()
	tn := &Tenancy{ID: "t1", Cache: pol}
	for i, ts := range []time.Time{at.Add(-time.Second), at} {
		r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
		r.Header.Set("Authorization", "Bearer sk-caller-secret")
		use := Usage{CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, StopReason: "tool_use", ToolUseNames: []string{"Bash"}}
		if i == 1 {
			use = u
		}
		k.record(tn, "sess-1", "", ts, []byte(kaBody), upstream{base: "http://up", path: "/v1/messages"}, r,
			bschemas.Anthropic, "/v1/messages", http.StatusOK, use, true)
	}
}

// #1227, end to end with the real timers: a 30k-prefix turn that starts background work is under the
// person floor (80k), so it is not held at response time. The dashboard writer reports "background"; the
// keeper then holds it with the background plan (floor 20k, 5 pings), and the pings are SENT before
// the real retention timer, armed for (5+1) x Idle, retires the entry.
func TestWriterPromotesABackgroundTurnUnderThePersonFloor(t *testing.T) {
	for _, wire := range []bool{true, false} {
		name := "writer tells the keeper"
		if !wire {
			name = "no call from the writer (the old behavior)"
		}
		t.Run(name, func(t *testing.T) {
			fs := &fakeSender{}
			h := &Handler{opts: Options{}, limiter: NewLimiter(Limits{}), rec: &dash.Recorder{},
				client: &http.Client{Transport: fs}}
			k := newKeeperWith(h, keepalive.Options{Go: inline}) // the real clock, the real timers
			rec, err := dash.NewRecorder(dash.Options{DBPath: filepath.Join(t.TempDir(), "d.db"), BatchSize: 1, FlushInterval: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { rec.Close(); k.Stop() })
			h.rec, h.waits, h.keeper = rec, waitcause.New(), k
			const idle = 200 * time.Millisecond
			pol := CachePolicy{KeepAlive: true, WaitPlan: true, Idle: idle, MaxPings: 2, MinPrefixTokens: 80000,
				ToolWaitMinPrefix: 20000, MaxUSDPerPing: 0.25}
			start := time.Now()
			recordBgTurn(t, k, pol, start, Usage{CacheRead: 29000, CacheWrite: 1000, StopReason: "end_turn"})
			key := keepalive.Key{Tenant: "t1", Session: "sess-1"}
			if _, ok := k.Keeper.Entry(key); ok {
				t.Fatal("a 30k person wait is held at response time: the case proves nothing")
			}
			c := &capture{rec: rec, waits: h.waits, tenant: "t1", provider: "anthropic", model: "claude-sonnet-5-5",
				route: "/v1/messages", start: time.Now(), status: 200, trace: apply.Trace{Session: "sess-1"}}
			if wire {
				c.raisePlan = h.raiseWaitPlan
			}
			c.noteWaitBody([]byte(bgStartBody))
			c.finish(Usage{FreshInput: 10, CacheRead: 29000, CacheWrite: 1000, StopReason: "end_turn"}, true, false, 0, 0)
			waitForRows(t, rec, 1)
			if !wire {
				testutil.WaitFor(t, "an Idle has passed", func() bool { return time.Since(start) > 2*idle })
				k.Keeper.Sweep(time.Now())
				if fs.n() != 0 {
					t.Fatalf("pings = %d without the writer's call, want 0", fs.n())
				}
				return
			}
			v, ok := k.Keeper.Entry(key)
			if !ok || v.Policy.MaxPings != 5 {
				t.Fatalf("after the writer: held=%v policy=%+v, want held with 5 pings", ok, v.Policy)
			}
			testutil.WaitForState(t, "5 pings sent", func() bool {
				k.Keeper.Sweep(time.Now())
				return fs.n() >= 5
			}, func() string { return fmt.Sprintf("pings sent: %d", fs.n()) })
			testutil.WaitFor(t, "the entry retires at its own deadline", func() bool {
				_, ok := k.Keeper.Entry(key)
				return !ok
			})
			if fs.n() != 5 {
				t.Fatalf("pings sent = %d, want 5", fs.n())
			}
		})
	}
}

// #1227, the time windows: a wait for a person is outside the manager's window and the account has
// keep-alive off, so nothing is held. The writer reports background: the wait ignores the windows, and the
// keeper holds the thread with the strategy's policy and the background count (4 + 3).
func TestWriterPromotesABackgroundTurnOutsideTheWindow(t *testing.T) {
	k, fs, clock := testKeeper(t, Limits{})
	now := clock.now()
	k.setStrategies([]tenant.Strategy{{
		ID: "s", Active: true, IdleSeconds: 240, MaxPings: 4, MinPrefixTokens: 20000, MaxUSDPerPing: 0.5,
		Windows: []tenant.Window{{Start: "00:00", End: "00:01"}},
		Target:  tenant.Target{Mode: tenant.TargetList, TenantIDs: []string{"t1"}}, UpdatedAt: now,
	}})
	key := keepalive.Key{Tenant: "t1", Session: "sess-1"}
	recordBgTurn(t, k, CachePolicy{WaitPlan: true}, now, Usage{CacheRead: 48576, CacheWrite: 1200, StopReason: "end_turn"})
	if _, ok := k.Keeper.Entry(key); ok {
		t.Fatal("a person wait outside the window is held: the case proves nothing")
	}
	if n, ok := k.Keeper.RaiseWaitPlan(key, waitcause.WaitingBackground); !ok || n != 7 {
		t.Fatalf("raise = %d %v, want 7 true", n, ok)
	}
	v, ok := k.Keeper.Entry(key)
	if !ok || v.Tag != "s" || v.Policy.Idle != 240*time.Second {
		t.Fatalf("entry %+v ok=%v, want the strategy's policy", v, ok)
	}
	for i := 0; i < 10; i++ {
		k.Keeper.Sweep(clock.advance(241 * time.Second))
	}
	if fs.n() != 7 {
		t.Fatalf("pings sent = %d, want 7", fs.n())
	}
}

// A held thread whose window shortened its plan: the account (keep-alive on, base 2) is the policy at a
// closed window; the writer's report widens it to the strategy's.
func TestWriterWidensAHeldTurnOutsideTheWindow(t *testing.T) {
	k, fs, clock := testKeeper(t, Limits{})
	now := clock.now()
	k.setStrategies([]tenant.Strategy{{
		ID: "s", Active: true, IdleSeconds: 240, MaxPings: 4, MinPrefixTokens: 20000, MaxUSDPerPing: 0.5,
		Windows: []tenant.Window{{Start: "00:00", End: "00:01"}},
		Target:  tenant.Target{Mode: tenant.TargetList, TenantIDs: []string{"t1"}}, UpdatedAt: now,
	}})
	key := keepalive.Key{Tenant: "t1", Session: "sess-1"}
	account := CachePolicy{KeepAlive: true, WaitPlan: true, Idle: 280 * time.Second, MaxPings: 2, MinPrefixTokens: 20000, MaxUSDPerPing: 0.25}
	recordBgTurn(t, k, account, now, Usage{CacheRead: 48576, CacheWrite: 1200, StopReason: "end_turn"})
	if v, ok := k.Keeper.Entry(key); !ok || v.Policy.MaxPings != 3 || v.Tag != "" {
		t.Fatalf("held=%v %+v, want the account's person plan of 3", ok, v)
	}
	if n, ok := k.Keeper.RaiseWaitPlan(key, waitcause.WaitingBackground); !ok || n != 7 {
		t.Fatalf("raise = %d %v, want 7 true", n, ok)
	}
	if v, _ := k.Keeper.Entry(key); v.Tag != "s" || v.Policy.Idle != 240*time.Second {
		t.Fatalf("policy %+v tag %q, want the strategy's", v.Policy, v.Tag)
	}
	for i := 0; i < 10; i++ {
		k.Keeper.Sweep(clock.advance(241 * time.Second))
	}
	if fs.n() != 7 {
		t.Fatalf("pings sent = %d, want 7", fs.n())
	}
}
