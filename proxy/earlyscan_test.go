package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/testutil"
	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
	"github.com/rossoctl/context-guru/tenant"
)

// A request body whose last tool result starts a background task: the turn that answers it ends with
// end_turn and waits for background work (#1225).
const bgStartBody = `{"model":"claude-sonnet-5-5","max_tokens":100,"messages":[
{"role":"user","content":"build"},
{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"Bash","input":{"command":"make","run_in_background":true}}]},
{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"Command running in background with ID: bash1."}]}]}`

// scanUpstream answers request 1 with a tool call and every later one with end_turn, with a 30k prefix
// (29,000 read + 1,000 written): under the person floor (80k), over the tool-wait floor (20k).
func scanUpstream(t *testing.T, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		n := hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		content, stop := `[{"type":"text","text":"ok"}]`, "end_turn"
		if n == 1 {
			content, stop = `[{"type":"tool_use","id":"c0","name":"Bash","input":{"command":"ls"}}]`, "tool_use"
		}
		fmt.Fprintf(w, `{"id":"msg_1","type":"message","role":"assistant","content":%s,"stop_reason":%q,`+
			`"usage":{"input_tokens":10,"output_tokens":34,"cache_read_input_tokens":29000,"cache_creation_input_tokens":1000}}`, content, stop)
	}))
}

// scanHandler is a real handler with the keep-alive on: person floor 80k, tool-wait floor 20k, Idle 200 ms.
func scanHandler(t *testing.T, upURL string) *Handler {
	t.Helper()
	cfg, err := config.LoadBytes([]byte("preset: codesafe\n"))
	if err != nil {
		t.Fatal(err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := dash.NewRecorder(dash.Options{DBPath: filepath.Join(t.TempDir(), "d.db"), BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{
		AnthropicUpstream: upURL, OpenAIUpstream: upURL, Preset: cfg.Preset, Dashboard: rec, Prices: fixedPricer{},
		Cache: CachePolicy{KeepAlive: true, WaitPlan: true, Idle: 200 * time.Millisecond, MaxPings: 2,
			MinPrefixTokens: 80000, ToolWaitMinPrefix: 20000, MaxUSDPerPing: 0.25},
	})
	t.Cleanup(h.Close)
	h.tracker.SetStartedAt(time.Now().Add(-2 * time.Hour).UnixMilli())
	return h
}

func scanPost(t *testing.T, srv *httptest.Server) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages", strings.NewReader(bgStartBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claude-cli/2.0.14 (external, cli)")
	req.Header.Set("x-context-guru-session", "sess-scan")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("proxy returned %d", resp.StatusCode)
	}
}

// The fast path, end to end with the real timers: the scan is ready at response end, the turn is planned
// as background work (the 20k floor, 5 pings) at once, and the 5 pings are SENT before the retention timer
// ((5+1) x Idle) retires the entry. No late call, no pending hold.
func TestEarlyScanPlansABackgroundTurnAtResponseTime(t *testing.T) {
	var hits atomic.Int64
	up := scanUpstream(t, &hits)
	defer up.Close()
	h := scanHandler(t, up.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	scanPost(t, srv) // request 1: a tool call
	scanPost(t, srv) // request 2: end_turn after a background start
	k := h.keeper.Keeper
	if h.scan.used.Load() == 0 {
		t.Fatalf("scan counters used=%d notReady=%d unknown=%d: the keeper never used the scan",
			h.scan.used.Load(), h.scan.notReady.Load(), h.scan.unknown.Load())
	}
	if k.Stats().Live != 1 {
		t.Fatalf("live = %d, want the 30k background turn held at response time", k.Stats().Live)
	}
	testutil.WaitForState(t, "5 pings sent", func() bool {
		k.Sweep(time.Now())
		return k.Stats().Pings >= 5
	}, func() string { return fmt.Sprintf("pings=%d live=%d", k.Stats().Pings, k.Stats().Live) })
	testutil.WaitFor(t, "the entry retires on its own timer", func() bool { return k.Stats().Live == 0 })
	if p := k.Stats().Pings; p != 5 {
		t.Fatalf("pings = %d, want 5", p)
	}
	if h.scan.writerEarly.Load() == 0 {
		t.Errorf("the writer did not reuse the scan: early=%d own=%d", h.scan.writerEarly.Load(), h.scan.writerOwn.Load())
	}
}

// The not-ready path: the scan is held by a test hook. The handler returns without waiting for it, the
// plan is today's (a 30k person wait is not held), and the writer scans for itself.
func TestEarlyScanNotReadyDoesNotHoldTheResponse(t *testing.T) {
	var hits atomic.Int64
	up := scanUpstream(t, &hits)
	defer up.Close()
	h := scanHandler(t, up.URL)
	release := make(chan struct{})
	var once sync.Once
	releaseScan := func() { once.Do(func() { close(release) }) }
	defer releaseScan()
	ctxs := make(chan context.Context, 8)
	h.scanGate = func(ctx context.Context) {
		ctxs <- ctx
		<-release // the scan cannot finish until the test says so
	}
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	scanPost(t, srv) // returns while the scan is held: the response does not wait for it
	scanPost(t, srv)
	k := h.keeper.Keeper
	if h.scan.notReady.Load() == 0 || h.scan.used.Load() != 0 {
		t.Fatalf("scan counters used=%d notReady=%d: want not ready", h.scan.used.Load(), h.scan.notReady.Load())
	}
	if k.Stats().Live != 0 {
		t.Fatalf("live = %d: a 30k person wait was held without the scan", k.Stats().Live)
	}
	// The request is over, so the scan's context is cancelled: the goroutine is told to stop.
	ctx := <-ctxs
	testutil.WaitFor(t, "the scan context ends with the request", func() bool { return ctx.Err() != nil })
	releaseScan()
}

// The scan goroutine ends when its context ends, whatever it is doing, and says "unknown".
func TestEarlyScanEndsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	s := startEarlyScan(ctx, bschemas.Anthropic, []byte(bgStartBody), func(c context.Context) { <-release })
	if s.ready() {
		t.Fatal("ready before the hook let it go")
	}
	if got := s.openTasks(&scanStats{}); got != nil {
		t.Fatalf("a held scan gave counts %+v", got)
	}
	cancel()
	close(release)
	testutil.WaitFor(t, "the scan goroutine ends", s.ready)
	var st scanStats
	if s.result() != nil || s.openTasks(&st) != nil || st.unknown.Load() != 1 {
		t.Fatalf("a cancelled scan: result=%v unknown=%d, want nil and 1", s.result(), st.unknown.Load())
	}
}

// A finished result survives stop(): the writer reads it after the handler ended.
func TestEarlyScanResultSurvivesStop(t *testing.T) {
	s := startEarlyScan(context.Background(), bschemas.Anthropic, []byte(bgStartBody), nil)
	testutil.WaitFor(t, "the scan finishes", s.ready)
	s.stop()
	var st scanStats
	got := s.openTasks(&st)
	if got == nil || got.Background != 1 || st.used.Load() != 1 {
		t.Fatalf("counts = %+v used=%d, want 1 background task", got, st.used.Load())
	}
}

// Over the work bound, the scan says unknown and the plan is today's.
func TestEarlyScanOverTheWorkBoundIsUnknown(t *testing.T) {
	big := []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("x", 9<<20) + `"}]}`)
	s := startEarlyScan(context.Background(), bschemas.Anthropic, big, nil)
	testutil.WaitFor(t, "the scan finishes", s.ready)
	var st scanStats
	if s.openTasks(&st) != nil || st.unknown.Load() != 1 {
		t.Fatalf("over the bound: unknown=%d, want 1", st.unknown.Load())
	}
}

// A panic in the scan goroutine (here in the hook) fails open: done closes, no result, no crash.
func TestEarlyScanPanicFailsOpen(t *testing.T) {
	s := startEarlyScan(context.Background(), bschemas.Anthropic, []byte(bgStartBody), func(context.Context) { panic("boom") })
	testutil.WaitFor(t, "the scan goroutine ends", s.ready)
	if s.result() != nil {
		t.Fatal("a panicking scan returned a result")
	}
}

// Nil-safe: no scan (dashboard off, empty body) is today's path and counts nothing.
func TestEarlyScanNilIsToday(t *testing.T) {
	var s *earlyScan
	var st scanStats
	if s.openTasks(&st) != nil || s.ready() || s.result() != nil || st.notReady.Load()+st.unknown.Load()+st.used.Load() != 0 {
		t.Fatal("a nil scan changed something")
	}
	s.stop()
	_ = keepalive.TaskCounts{}
}

// #1227: the windows are decided from the same counts as the plan. With the account's keep-alive off and the
// strategy's window shut, a wait for a person is not held; the same turn with the scan's background count
// ignores the window and is held with the strategy's policy and the background count (4 + 3).
func TestEarlyScanCountsIgnoreTheWindowForBackgroundWork(t *testing.T) {
	for _, tc := range []struct {
		name     string
		open     *keepalive.TaskCounts
		wantHeld bool
	}{
		{"no counts: a person wait, the window shut", nil, false},
		{"scan: background work, no window", &keepalive.TaskCounts{Background: 1}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, fs, clock := testKeeper(t, Limits{})
			now := clock.now()
			k.setStrategies([]tenant.Strategy{{
				ID: "s", Active: true, IdleSeconds: 240, MaxPings: 4, MinPrefixTokens: 20000, MaxUSDPerPing: 0.5,
				Windows: []tenant.Window{{Start: "00:00", End: "00:01"}},
				Target:  tenant.Target{Mode: tenant.TargetList, TenantIDs: []string{"t1"}}, UpdatedAt: now,
			}})
			tn := &Tenancy{ID: "t1", Cache: CachePolicy{WaitPlan: true}}
			for i, ts := range []time.Time{now.Add(-time.Second), now} {
				r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
				r.Header.Set("Authorization", "Bearer sk-caller-secret")
				use := Usage{CacheRead: 48576, CacheWrite: 1200, StopReason: "tool_use", ToolUseNames: []string{"Bash"}}
				if i == 1 {
					use = Usage{CacheRead: 48576, CacheWrite: 1200, StopReason: "end_turn"}
				}
				k.recordTouch("", tn, "sess-1", "", ts, []byte(kaBody), upstream{base: "http://up", path: "/v1/messages"}, r,
					bschemas.Anthropic, "/v1/messages", http.StatusOK, use, true, tc.open)
			}
			v, held := k.Keeper.Entry(keepalive.Key{Tenant: "t1", Session: "sess-1"})
			if held != tc.wantHeld {
				t.Fatalf("held = %v, want %v", held, tc.wantHeld)
			}
			if !held {
				return
			}
			if v.Tag != "s" || v.Policy.MaxPings != 7 {
				t.Fatalf("tag %q pings %d, want the strategy's policy with 7", v.Tag, v.Policy.MaxPings)
			}
			for i := 0; i < 10; i++ {
				k.Keeper.Sweep(clock.advance(241 * time.Second))
			}
			if fs.n() != 7 {
				t.Fatalf("pings sent = %d, want 7", fs.n())
			}
		})
	}
}

// #1227: the body reading decides the windows only where the plan changes the count. On the 1-hour tier
// and for OpenAI, a shut window still applies to a wait that reads "a person" (the account's policy, no
// tag); on the 5-minute tier the same counts ignore it (the strategy's tag).
func TestEarlyScanCountsDoNotMoveTheWindowOfThe1HourTierOrOpenAI(t *testing.T) {
	account := CachePolicy{KeepAlive: true, WaitPlan: true, Idle: 280 * time.Second, MaxPings: 2,
		MinPrefixTokens: 20000, MaxUSDPerPing: 0.25}
	for _, tc := range []struct {
		name     string
		provider bschemas.ModelProvider
		body     string
		use      Usage
		wantTag  string
	}{
		{"5-minute tier: window ignored", bschemas.Anthropic, kaBody, Usage{CacheRead: 48576, CacheWrite: 1200}, "s"},
		{"1-hour tier: window applies", bschemas.Anthropic, ttlBody1h, Usage{CacheRead: 48576, CacheWrite: 1200, CacheWrite1h: 1200}, ""},
		{"OpenAI: window applies", bschemas.OpenAI, `{"model":"azure/gpt-5.6-luna","max_output_tokens":2048,"input":[]}`,
			Usage{CacheRead: 48576}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, _, clock := testKeeper(t, Limits{})
			now := clock.now()
			k.setStrategies([]tenant.Strategy{{
				ID: "s", Active: true, IdleSeconds: 240, MaxPings: 4, MinPrefixTokens: 20000, MaxUSDPerPing: 0.5,
				Windows: []tenant.Window{{Start: "00:00", End: "00:01"}},
				Target:  tenant.Target{Mode: tenant.TargetList, TenantIDs: []string{"t1"}}, UpdatedAt: now,
			}})
			tn := &Tenancy{ID: "t1", Cache: account}
			route := "/v1/messages"
			if tc.provider == bschemas.OpenAI {
				route = "/v1/responses"
			}
			for i, ts := range []time.Time{now.Add(-time.Second), now} {
				r := httptest.NewRequest(http.MethodPost, route, strings.NewReader(""))
				r.Header.Set("Authorization", "Bearer sk-caller-secret")
				use := tc.use
				use.StopReason = "end_turn"
				if i == 0 {
					use.StopReason, use.ToolUseNames = "tool_use", []string{"Bash"}
				}
				k.recordTouch("", tn, "sess-1", "", ts, []byte(tc.body), upstream{base: "http://up", path: route}, r,
					tc.provider, route, http.StatusOK, use, true, &keepalive.TaskCounts{Background: 1})
			}
			v, held := k.Keeper.Entry(keepalive.Key{Tenant: "t1", Session: "sess-1"})
			if !held {
				t.Fatal("not held")
			}
			if v.Tag != tc.wantTag {
				t.Fatalf("tag = %q, want %q", v.Tag, tc.wantTag)
			}
		})
	}
}

// Ignoring the windows must never refuse a hold today's decision makes. The account's floor is 20k and the
// shut-window strategy's is 100k, and a background wait takes the lower of the floor and the tool-wait floor (60k
// on the account, which the strategy inherits): the strategy's floor for it is 60k. A 50k background turn is held with the account's policy, not refused.
// When the strategy's policy holds too, it is used (it plans at least as many pings).
func TestEarlyScanCountsNeverRefuseAHoldTodayMakes(t *testing.T) {
	account := CachePolicy{KeepAlive: true, WaitPlan: true, Idle: 280 * time.Second, MaxPings: 2,
		MinPrefixTokens: 20000, ToolWaitMinPrefix: 60000, MaxUSDPerPing: 0.25}
	for _, tc := range []struct {
		name     string
		floor    int
		strategy int // MaxPings of the strategy
		wantTag  string
		wantPing int
	}{
		{"strategy floor 100k: today's policy holds", 100000, 4, "", 5},
		{"strategy floor 20k, more pings: the strategy", 20000, 4, "s", 7},
		{"strategy floor 20k, fewer pings: today's policy", 20000, 1, "", 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, _, clock := testKeeper(t, Limits{})
			now := clock.now()
			k.setStrategies([]tenant.Strategy{{
				ID: "s", Active: true, IdleSeconds: 240, MaxPings: tc.strategy, MinPrefixTokens: tc.floor, MaxUSDPerPing: 0.5,
				Windows: []tenant.Window{{Start: "00:00", End: "00:01"}},
				Target:  tenant.Target{Mode: tenant.TargetList, TenantIDs: []string{"t1"}}, UpdatedAt: now,
			}})
			tn := &Tenancy{ID: "t1", Cache: account}
			for i, ts := range []time.Time{now.Add(-time.Second), now} {
				r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
				r.Header.Set("Authorization", "Bearer sk-caller-secret")
				use := Usage{CacheRead: 48576, CacheWrite: 1200, StopReason: "end_turn"}
				if i == 0 {
					use.StopReason, use.ToolUseNames = "tool_use", []string{"Bash"}
				}
				k.recordTouch("", tn, "sess-1", "", ts, []byte(kaBody), upstream{base: "http://up", path: "/v1/messages"}, r,
					bschemas.Anthropic, "/v1/messages", http.StatusOK, use, true, &keepalive.TaskCounts{Background: 1})
			}
			v, held := k.Keeper.Entry(keepalive.Key{Tenant: "t1", Session: "sess-1"})
			if !held {
				t.Fatal("a hold today's path makes was refused")
			}
			if v.Tag != tc.wantTag || v.Policy.MaxPings != tc.wantPing {
				t.Fatalf("tag %q pings %d, want %q and %d", v.Tag, v.Policy.MaxPings, tc.wantTag, tc.wantPing)
			}
		})
	}
}
