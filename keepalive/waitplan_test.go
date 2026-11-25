package keepalive

import (
	"net/http"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/internal/testutil"

	"github.com/rossoctl/context-guru/store"
)

// resp is one finished response of a thread: the tool names it called and its stop reason.
type resp struct {
	names []string
	stop  string
}

var (
	toolCall = resp{[]string{"Bash"}, "tool_use"}
	endTurn  = resp{nil, "end_turn"}
)

// planPolicy is kaPolicy with the per-wait plan on and its shipped defaults.
func planPolicy() Policy {
	p := kaPolicy()
	p.WaitPlan = true
	return p
}

// recordSeq records the responses in order on key, one second apart, the last at `at`.
func recordSeq(k *Keeper, key Key, pol Policy, body string, at time.Time, seq ...resp) {
	for i, r := range seq {
		ts := at.Add(-time.Duration(len(seq)-1-i) * time.Second)
		k.Record(Record{Key: key, StartedAt: ts, Body: []byte(body), Provider: bschemas.Anthropic,
			Route: "/v1/messages", Status: http.StatusOK,
			Usage:        Usage{CacheRead: 48576, CacheWrite: 1200, StopReason: r.stop},
			ToolUseNames: r.names, UsageOK: true, Policy: pol, Header: kaHeader(), Hold: fakeOf(k).hold(key)})
	}
}

// plannedPings is the ping count the entry on key was planned for.
func plannedPings(t *testing.T, k *Keeper, key Key) int {
	t.Helper()
	v, ok := k.Entry(key)
	if !ok {
		t.Fatalf("no live entry for %+v", key)
	}
	return v.Policy.MaxPings
}

// Every row of the issue's table, at the shipped defaults (base 2).
func TestWaitPlanPingBudgetPerWaitCause(t *testing.T) {
	sub := Key{Tenant: "t1", Session: "sess-1", Thread: "a:abc"}
	for _, tc := range []struct {
		name      string
		key       Key
		seq       []resp
		bg, tm    int
		want      int
		wantEntry bool
	}{
		{"main, own tool", sess1, []resp{toolCall, toolCall}, 0, 0, 8, true},
		{"tool stop, no names (spliced window)", sess1, []resp{toolCall, {nil, "tool_use"}}, 0, 0, 8, true},
		{"tool stop, no names, open background", sess1, []resp{toolCall, {nil, "tool_use"}}, 1, 0, 8, true},
		{"subagent, own tool", sub, []resp{toolCall, toolCall}, 0, 0, 8, true},
		{"main, end_turn, background", sess1, []resp{toolCall, endTurn}, 1, 0, 5, true},
		{"main, end_turn, a person", sess1, []resp{toolCall, endTurn}, 0, 0, 3, true},
		{"main, ask-human tool", sess1, []resp{toolCall, {[]string{"AskUserQuestion"}, "tool_use"}}, 0, 0, 3, true},
		{"main, end_turn, teammate", sess1, []resp{toolCall, endTurn}, 0, 1, 2, true},
		{"subagent, end_turn", sub, []resp{toolCall, endTurn}, 0, 0, 2, true},
		{"subagent, SubagentHandback", sub, []resp{toolCall, {[]string{"SubagentHandback"}, "tool_use"}}, 0, 0, 2, true},
		{"subagent, end_turn, background", sub, []resp{toolCall, endTurn}, 1, 0, 2, true},
		{"done", sess1, []resp{toolCall, {[]string{"StructuredOutput"}, "tool_use"}}, 0, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, _, clock := testKeeperWith(t, func(o *Options) {
				o.OpenTasks = func(string, string, string) (int, int) { return tc.bg, tc.tm }
			})
			recordSeq(k, tc.key, planPolicy(), kaBody, clock.now(), tc.seq...)
			if !tc.wantEntry {
				if _, ok := k.Entry(tc.key); ok {
					t.Fatal("a done thread is held")
				}
				return
			}
			if got := plannedPings(t, k, tc.key); got != tc.want {
				t.Errorf("planned pings = %d, want %d", got, tc.want)
			}
		})
	}
}

// A tool wait is pinged until the result comes, and stops at 8.
func TestWaitPlanToolWaitIsPingedUntilTheResultComesAtMostEight(t *testing.T) {
	k, fd, clock := testKeeper(t)
	recordSeq(k, sess1, planPolicy(), kaBody, clock.now(), toolCall, toolCall)
	for i := 1; i <= 12; i++ {
		k.Sweep(clock.advance(281 * time.Second))
		if i == 5 {
			if n := fd.n(); n != 5 {
				t.Fatalf("after 5 intervals: %d pings, want 5 (the base is 2)", n)
			}
		}
	}
	if n := fd.n(); n != 8 {
		t.Fatalf("pings = %d, want 8 (the cap)", n)
	}
}

func TestWaitPlanResultArrivingStopsThePings(t *testing.T) {
	k, fd, clock := testKeeper(t)
	recordSeq(k, sess1, planPolicy(), kaBody, clock.now(), toolCall, toolCall)
	for i := 0; i < 4; i++ {
		k.Sweep(clock.advance(281 * time.Second))
	}
	k.Arrive(sess1) // the tool result comes back
	if _, ok := k.Entry(sess1); ok {
		t.Fatal("entry still held after the next request arrived")
	}
	k.Sweep(clock.advance(281 * time.Second))
	if n := fd.n(); n != 4 {
		t.Errorf("pings = %d, want 4", n)
	}
}

// The base is the floor: a configured tool-wait cap under the base never shortens it.
func TestWaitPlanToolWaitNeverBelowTheBase(t *testing.T) {
	k, _, clock := testKeeper(t)
	pol := planPolicy()
	pol.MaxPings, pol.ToolWaitMaxPings = 10, 3
	recordSeq(k, sess1, pol, kaBody, clock.now(), toolCall, toolCall)
	if got := plannedPings(t, k, sess1); got != 10 {
		t.Errorf("planned = %d, want the base 10", got)
	}
}

// Configured extra pings replace the defaults.
func TestWaitPlanConfiguredCounts(t *testing.T) {
	k, _, clock := testKeeperWith(t, func(o *Options) {
		o.OpenTasks = func(string, string, string) (int, int) { return 1, 0 }
	})
	pol := planPolicy()
	pol.BackgroundExtraPings, pol.ToolWaitMaxPings = 5, 4
	recordSeq(k, sess1, pol, kaBody, clock.now(), toolCall, endTurn)
	if got := plannedPings(t, k, sess1); got != 7 {
		t.Errorf("background = %d, want 7", got)
	}
	k2, _, clock2 := testKeeper(t)
	recordSeq(k2, sess1, pol, kaBody, clock2.now(), toolCall, toolCall)
	if got := plannedPings(t, k2, sess1); got != 4 {
		t.Errorf("tool wait = %d, want 4", got)
	}
}

// A plan result never passes 11 unless the base itself is higher.
func TestWaitPlanCapsAtEleven(t *testing.T) {
	p := planPolicy()
	p.MaxPings, p.ToolWaitMaxPings = 9, 50
	if got := p.WaitPings("own_tool", false); got != 11 {
		t.Errorf("got %d, want 11", got)
	}
	p.MaxPings = 20
	if got := p.WaitPings("background", false); got != 20 {
		t.Errorf("got %d, want the base 20", got)
	}
}

// A thread with no tool call yet gets no pings. After one tool call it does.
func TestWaitPlanNoToolCallYetGetsNoPings(t *testing.T) {
	k, fd, clock := testKeeper(t)
	recordSeq(k, sess1, planPolicy(), kaBody, clock.now(), endTurn, endTurn)
	if n := k.Sweep(clock.advance(281 * time.Second)); n != 0 || fd.n() != 0 {
		t.Fatalf("a thread with no tool call was pinged (%d, %d)", n, fd.n())
	}
	k2, fd2, clock2 := testKeeper(t)
	recordSeq(k2, sess1, planPolicy(), kaBody, clock2.now(), toolCall, endTurn)
	if n := k2.Sweep(clock2.advance(281 * time.Second)); n != 1 || fd2.n() != 1 {
		t.Fatalf("a thread that called a tool was not pinged (%d, %d)", n, fd2.n())
	}
}

// The thread remembers a tool call from an earlier turn.
func TestWaitPlanEarlierToolCallCounts(t *testing.T) {
	k, fd, clock := testKeeper(t)
	recordSeq(k, sess1, planPolicy(), kaBody, clock.now().Add(-10*time.Second), toolCall)
	recordSeq(k, sess1, planPolicy(), kaBody, clock.now(), endTurn, endTurn)
	if n := k.Sweep(clock.advance(281 * time.Second)); n != 1 || fd.n() != 1 {
		t.Fatalf("a thread with an earlier tool call was not pinged (%d, %d)", n, fd.n())
	}
}

// The off switch: today's behavior. A fixed MaxPings for every wait, and the no-tool guard is off.
func TestWaitPlanOffIsTodaysBehavior(t *testing.T) {
	k, fd, clock := testKeeper(t)
	recordSeq(k, sess1, kaPolicy(), kaBody, clock.now(), toolCall, toolCall)
	if got := plannedPings(t, k, sess1); got != 2 {
		t.Fatalf("planned = %d, want the fixed 2", got)
	}
	for i := 0; i < 6; i++ {
		k.Sweep(clock.advance(281 * time.Second))
	}
	if fd.n() != 2 {
		t.Errorf("pings = %d, want 2", fd.n())
	}
	k2, fd2, clock2 := testKeeper(t)
	recordSeq(k2, sess1, kaPolicy(), kaBody, clock2.now(), endTurn, endTurn)
	if n := k2.Sweep(clock2.advance(281 * time.Second)); n != 1 || fd2.n() != 1 {
		t.Fatalf("with the plan off, a thread with no tool call is pinged as before (%d, %d)", n, fd2.n())
	}
}

// A 1-hour thread keeps its own bounded schedule: the plan does not stretch the hold.
func TestWaitPlanDoesNotChangeAOneHourThread(t *testing.T) {
	k, _, clock := testKeeper(t)
	body := ttlBody1h
	for i, ts := range []time.Time{clock.now().Add(-time.Second), clock.now()} {
		k.Record(Record{Key: sess1, StartedAt: ts, Body: []byte(body), Provider: bschemas.Anthropic,
			Route: "/v1/messages", Status: http.StatusOK,
			Usage:        Usage{CacheRead: 48576, CacheWrite: 1200, CacheWrite1h: 1200, StopReason: "tool_use"},
			ToolUseNames: []string{"Bash"}, UsageOK: true, Policy: planPolicy(), Header: kaHeader(),
			Hold: fakeOf(k).hold(sess1)})
		_ = i
	}
	v, ok := k.Entry(sess1)
	if !ok || !v.TTL1h {
		t.Fatalf("not a 1-hour entry: %+v ok=%v", v, ok)
	}
	if v.Policy.MaxPings > 3 {
		t.Errorf("1-hour thread planned %d pings, want at most 3", v.Policy.MaxPings)
	}
}

// The sweep question is not asked while a thread waits for its own tool. It takes the planned ping on
// a tool_result span that began with end_turn.
func TestWaitPlanNoSweepQuestionDuringAToolWait(t *testing.T) {
	k, fs, clock := testKeeper(t)
	st := store.NewMemory(store.Options{MaxEntries: 400})
	asker := &sweepAsker{cacheRead: 48576}
	registerSweep(t, st, asker, "claude-sonnet-5", sweepConversation(3))
	recordSeq(k, sess1, planPolicy(), kaToolBody, clock.now(), toolCall, toolCall)
	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
	if asker.calls.Load() != 0 || fs.n() != 1 {
		t.Fatalf("tool wait: asks %d, plain %d; want 0 and 1", asker.calls.Load(), fs.n())
	}
}

func TestWaitPlanSweepQuestionOnAnEndTurnSpan(t *testing.T) {
	k, fs, clock := testKeeper(t)
	st := store.NewMemory(store.Options{MaxEntries: 400})
	asker := &sweepAsker{cacheRead: 48576}
	registerSweep(t, st, asker, "claude-sonnet-5", sweepConversation(3))
	recordSeq(k, sess1, planPolicy(), kaToolBody, clock.now(), toolCall, endTurn)
	k.Sweep(clock.advance(281 * time.Second))
	waitSweepOutcome(t, k, fs)
	waitPings(t, k, 1)
	if asker.calls.Load() != 1 || fs.n() != 0 {
		t.Fatalf("end_turn span: asks %d, plain %d; want 1 and 0", asker.calls.Load(), fs.n())
	}
}

// The extra slot after the last planned ping: no question on a tool wait, one on an end_turn span.
func TestWaitPlanNoExtraQuestionAfterAToolWaitButOneAfterEndTurn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		seq   []resp
		pings int
		asks  int64
	}{
		{"tool wait", []resp{toolCall, toolCall}, 2, 0},
		{"end_turn", []resp{toolCall, endTurn}, 3, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, fs, clock := testKeeper(t)
			st := store.NewMemory(store.Options{MaxEntries: 400})
			asker := &sweepAsker{cacheRead: 48576}
			registerSweep(t, st, asker, "claude-sonnet-5", sweepConversation(3))
			pol := planPolicy()
			pol.ToolWaitMaxPings = 2 // the tool-wait cap, kept at the base so the slot comes soon
			recordSeq(k, sess1, pol, kaBody, clock.now(), tc.seq...)
			for i := 1; i <= tc.pings; i++ {
				k.Sweep(clock.advance(281 * time.Second))
				waitPings(t, k, int64(i))
			}
			k.Sweep(clock.advance(250 * time.Second))
			// The asks == 0 case is exact only because testKeeper runs Options.Go inline.
			if tc.asks > 0 {
				testutil.WaitFor(t, "the extra question", func() bool { return asker.calls.Load() >= tc.asks })
			}
			if asker.calls.Load() != tc.asks || fs.n() != tc.pings {
				t.Errorf("asks %d, plain %d; want %d and %d", asker.calls.Load(), fs.n(), tc.asks, tc.pings)
			}
		})
	}
}

func TestWindowsApply(t *testing.T) {
	for _, tc := range []struct {
		cause string
		sub   bool
		want  bool
	}{
		{"human", false, true}, {"human", true, false}, {"own_tool", false, false}, {"own_tool", true, false},
		{"background", false, false}, {"teammate", false, false}, {"done", false, true}, {"unknown", false, true},
		{"retry", false, true},
	} {
		if got := WindowsApply(tc.cause, tc.sub); got != tc.want {
			t.Errorf("WindowsApply(%q, sub=%v) = %v, want %v", tc.cause, tc.sub, got, tc.want)
		}
	}
}

// The plan the dashboard reads (Plans: pings_left, next ping) is the per-wait one. The hold of the
// longest plan stays inside the strategy bound: (8+1) x 240 s = 36 min; at the cap of 11 and 299 s, 60 min.
func TestWaitPlanShowsInThePlanTheDashboardReads(t *testing.T) {
	k, _, clock := testKeeper(t)
	start := clock.now()
	recordSeq(k, sess1, planPolicy(), kaBody, start, toolCall, toolCall)
	pl := k.Plans("t1", false, "sess-1")[""]
	if pl.PingsLeft != 8 || !pl.NextPing.Equal(start.Add(280*time.Second)) {
		t.Fatalf("plan = %+v, want 8 pings left", pl)
	}
	if h := time.Duration(MaxWaitPings+1) * 299 * time.Second; h > time.Hour {
		t.Errorf("longest plan holds %v, over the 1 h override ceiling", h)
	}
	k2, _, clock2 := testKeeper(t)
	recordSeq(k2, sess1, planPolicy(), kaBody, clock2.now(), endTurn, endTurn)
	if len(k2.Plans("t1", false, "sess-1")) != 0 {
		t.Error("a thread with no tool call yet shows a plan")
	}
}

// A response whose name list may be incomplete (a spliced sniff window) counts as a tool call: the thread is
// pinged as before #858, not refused as "no tool call yet".
func TestWaitPlanIncompleteNamesCountAsAToolCall(t *testing.T) {
	k, fd, clock := testKeeper(t)
	pol := planPolicy()
	for i, ts := range []time.Time{clock.now().Add(-time.Second), clock.now()} {
		k.Record(Record{Key: sess1, StartedAt: ts, Body: []byte(kaBody), Provider: bschemas.Anthropic,
			Route: "/v1/messages", Status: http.StatusOK, Usage: Usage{CacheRead: 48576, CacheWrite: 1200, StopReason: "end_turn"},
			ToolUseIncomplete: i == 1, UsageOK: true, Policy: pol, Header: kaHeader(), Hold: fakeOf(k).hold(sess1)})
	}
	if !k.ToolSeen(sess1) {
		t.Fatal("an incomplete name list did not count as a tool call")
	}
	if n := k.Sweep(clock.advance(281 * time.Second)); n != 1 || fd.n() != 1 {
		t.Fatalf("pings = %d/%d, want 1", n, fd.n())
	}
}

// #1225, #1227: counts the host read from this turn's own body join the plan at record time. A 50k turn
// that starts background work gets the background count and the background floor at once.
func TestRecordPlansFromTheHostsOwnBodyCounts(t *testing.T) {
	person := planPolicy()
	person.MinPrefixTokens = 80000 // 20k for the rest (the shipped split)
	rec := func(k *Keeper, at time.Time, open *TaskCounts) {
		for i, r := range []resp{toolCall, endTurn} {
			k.Record(Record{Key: sess1, StartedAt: at.Add(-time.Duration(1-i) * time.Second), Body: []byte(kaBody),
				Provider: bschemas.Anthropic, Route: "/v1/messages", Status: http.StatusOK,
				Usage:        Usage{CacheRead: 48576, CacheWrite: 1200, StopReason: r.stop},
				ToolUseNames: r.names, UsageOK: true, Policy: person, Header: kaHeader(),
				Hold: fakeOf(k).hold(sess1), OpenTasks: open})
		}
	}
	t.Run("own-body background: 5 pings and the 20k floor", func(t *testing.T) {
		k, fd, clock := testKeeper(t)
		rec(k, clock.now(), &TaskCounts{Background: 1})
		if got := plannedPings(t, k, sess1); got != 5 {
			t.Fatalf("planned %d, want 5", got)
		}
		for i := 0; i < 8; i++ {
			k.Sweep(clock.advance(281 * time.Second))
		}
		if fd.n() != 5 {
			t.Fatalf("pings sent = %d, want 5", fd.n())
		}
	})
	t.Run("no counts: a 50k person wait is not held, as before", func(t *testing.T) {
		k, _, clock := testKeeper(t)
		rec(k, clock.now(), nil)
		if _, ok := k.Entry(sess1); ok {
			t.Fatal("held without the counts")
		}
	})
	t.Run("zero counts from the body: a person wait, as before", func(t *testing.T) {
		k, _, clock := testKeeper(t)
		rec(k, clock.now(), &TaskCounts{})
		if _, ok := k.Entry(sess1); ok {
			t.Fatal("held for a person wait under the person floor")
		}
	})
	t.Run("never fewer: the tracker's larger count wins", func(t *testing.T) {
		k, _, clock := testKeeperWith(t, func(o *Options) {
			o.OpenTasks = func(string, string, string) (int, int) { return 2, 0 }
		})
		rec(k, clock.now(), &TaskCounts{Background: 1})
		if got := plannedPings(t, k, sess1); got != 5 {
			t.Fatalf("planned %d, want 5", got)
		}
	})
	t.Run("a teammate count in the body is not merged: 3 pings stay 3, the floor stays", func(t *testing.T) {
		// Merged, human (base+1 = 3) would become teammate (base = 2): one ping fewer.
		k, _, clock := testKeeper(t)
		rec(k, clock.now(), &TaskCounts{Teammate: 1})
		if _, ok := k.Entry(sess1); ok {
			t.Fatal("held: the teammate count changed the floor of a 50k person wait")
		}
		k2, _, clock2 := testKeeper(t)
		p := person
		p.MinPrefixTokens = 20000 // held either way: compare the counts
		for i, r := range []resp{toolCall, endTurn} {
			k2.Record(Record{Key: sess1, StartedAt: clock2.now().Add(-time.Duration(1-i) * time.Second), Body: []byte(kaBody),
				Provider: bschemas.Anthropic, Route: "/v1/messages", Status: http.StatusOK,
				Usage:        Usage{CacheRead: 48576, CacheWrite: 1200, StopReason: r.stop},
				ToolUseNames: r.names, UsageOK: true, Policy: p, Header: kaHeader(),
				Hold: fakeOf(k2).hold(sess1), OpenTasks: &TaskCounts{Teammate: 1}})
		}
		if got := plannedPings(t, k2, sess1); got != 3 {
			t.Fatalf("planned %d, want 3 (a person wait keeps its count)", got)
		}
	})
	t.Run("never fewer: a background extra below the human extra keeps the larger count", func(t *testing.T) {
		k, _, clock := testKeeper(t)
		p := person
		p.HumanExtraPings, p.BackgroundExtraPings = 5, 1 // human 7, background 3
		p.MinPrefixTokens = 20000
		for i, r := range []resp{toolCall, endTurn} {
			k.Record(Record{Key: sess1, StartedAt: clock.now().Add(-time.Duration(1-i) * time.Second), Body: []byte(kaBody),
				Provider: bschemas.Anthropic, Route: "/v1/messages", Status: http.StatusOK,
				Usage:        Usage{CacheRead: 48576, CacheWrite: 1200, StopReason: r.stop},
				ToolUseNames: r.names, UsageOK: true, Policy: p, Header: kaHeader(),
				Hold: fakeOf(k).hold(sess1), OpenTasks: &TaskCounts{Background: 1}})
		}
		if got := plannedPings(t, k, sess1); got != 7 {
			t.Fatalf("planned %d, want 7 (the person count, the larger)", got)
		}
	})
	t.Run("a hold today's path makes is never refused", func(t *testing.T) {
		// 90k prefix clears the 80k person floor: held with or without the scan, with at least 3 pings.
		for _, open := range []*TaskCounts{nil, {Background: 1}, {Teammate: 1}, {}} {
			k, _, clock := testKeeper(t)
			for i, r := range []resp{toolCall, endTurn} {
				k.Record(Record{Key: sess1, StartedAt: clock.now().Add(-time.Duration(1-i) * time.Second), Body: []byte(kaBody),
					Provider: bschemas.Anthropic, Route: "/v1/messages", Status: http.StatusOK,
					Usage:        Usage{CacheRead: 88576, CacheWrite: 1200, StopReason: r.stop},
					ToolUseNames: r.names, UsageOK: true, Policy: person, Header: kaHeader(),
					Hold: fakeOf(k).hold(sess1), OpenTasks: open})
			}
			if v, ok := k.Entry(sess1); !ok || v.Policy.MaxPings < 3 {
				t.Fatalf("open=%+v: held=%v pings=%d, want held with at least 3", open, ok, v.Policy.MaxPings)
			}
		}
	})
	t.Run("the 1-hour tier and OpenAI do not use the body reading", func(t *testing.T) {
		k, _, clock := testKeeper(t)
		for i, ts := range []time.Time{clock.now().Add(-time.Second), clock.now()} {
			k.Record(Record{Key: sess1, StartedAt: ts, Body: []byte(ttlBody1h), Provider: bschemas.Anthropic,
				Route: "/v1/messages", Status: http.StatusOK,
				Usage:        Usage{CacheRead: 48576, CacheWrite: 1200, CacheWrite1h: 1200, StopReason: []string{"tool_use", "end_turn"}[i]},
				ToolUseNames: [][]string{{"Bash"}, nil}[i], UsageOK: true, Policy: person, Header: kaHeader(),
				Hold: fakeOf(k).hold(sess1), OpenTasks: &TaskCounts{Background: 1}})
		}
		if _, ok := k.Entry(sess1); ok {
			t.Fatal("a 1-hour 50k person wait was held on the strength of the body reading")
		}
		k2, _, _ := testKeeper(t)
		pol := person
		pol.Idle = modelinfo.OpenAIDefaultKeepAliveIdle
		for i := 0; i < 2; i++ {
			k2.Record(Record{Key: sess1, StartedAt: time.Now(), Provider: bschemas.OpenAI, Route: "/v1/responses",
				Body:   []byte(`{"model":"azure/gpt-5.6-luna","max_output_tokens":2048,"input":[]}`),
				Status: http.StatusOK, Usage: Usage{CacheRead: 48576, StopReason: "end_turn"}, UsageOK: true, Policy: pol,
				Header: kaHeader(), Hold: fakeOf(k2).hold(sess1), OpenTasks: &TaskCounts{Background: 1}})
		}
		if _, ok := k2.Entry(sess1); ok {
			t.Fatal("a 50k OpenAI person wait was held on the strength of the body reading")
		}
	})
}
