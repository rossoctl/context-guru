package keepalive

import (
	"testing"
	"time"

	"github.com/rossoctl/context-guru/internal/waitcause"
	"github.com/rossoctl/context-guru/tenant"
)

// closedStrategy covers every tenant but its window is shut at t0 (12:00 UTC).
func closedStrategy(idle, pings int) tenant.Strategy {
	s := allDay("s", idle, pings, t0)
	s.Windows = []tenant.Window{{Start: "01:00", End: "02:00", TZ: "UTC"}}
	return s
}

func windowsIn(account Policy, strategies []tenant.Strategy, cause func(*TaskCounts) string, open *TaskCounts, calls *[]bool) WindowsInput {
	return WindowsInput{
		Account: account, Tracked: true, Open: open, Prefix: 50000,
		CauseOf:     cause,
		OneHourTier: func() bool { return false },
		Resolve: func(p Policy, ignore bool) (Policy, string) {
			*calls = append(*calls, ignore)
			return Resolve(p, ResolveInput{TenantID: "a", Now: t0, Strategies: strategies, IgnoreWindows: ignore})
		},
	}
}

func TestResolveWithWindowsAToolWaitIgnoresTheWindows(t *testing.T) {
	account := Policy{WaitPlan: true, MaxPings: 2, Idle: 240 * time.Second, MinPrefixTokens: 1000}
	var calls []bool
	in := windowsIn(account, []tenant.Strategy{closedStrategy(100, 5)},
		func(*TaskCounts) string { return waitcause.WaitingOwnTool }, nil, &calls)
	pol, tag, _ := ResolveWithWindows(in)
	if tag != "s" || pol.MaxPings != 5 || len(calls) != 1 || !calls[0] {
		t.Fatalf("a tool wait ignores the closed window: tag=%q pings=%d calls=%v", tag, pol.MaxPings, calls)
	}
}

func TestResolveWithWindowsAPersonWaitKeepsTheWindows(t *testing.T) {
	account := Policy{WaitPlan: true, MaxPings: 2, Idle: 240 * time.Second, MinPrefixTokens: 1000}
	var calls []bool
	in := windowsIn(account, []tenant.Strategy{closedStrategy(100, 5)},
		func(*TaskCounts) string { return waitcause.WaitingHuman }, nil, &calls)
	pol, tag, _ := ResolveWithWindows(in)
	if tag != "" || pol.MaxPings != 2 || len(calls) != 1 || calls[0] {
		t.Fatalf("a person wait respects the closed window: tag=%q pings=%d calls=%v", tag, pol.MaxPings, calls)
	}
}

func TestResolveWithWindowsNeverPlansFewerPingsThanToday(t *testing.T) {
	// The tracker says "person" (today: windows apply, account policy), the merged cause says
	// "background" (wide: windows ignored, a strategy with 1 ping). The wide reading plans fewer
	// pings than today's, so today's wins.
	account := Policy{KeepAlive: true, WaitPlan: true, MaxPings: 3, Idle: 240 * time.Second, MinPrefixTokens: 1000}
	open := &TaskCounts{}
	cause := func(o *TaskCounts) string {
		if o != nil {
			return waitcause.WaitingBackground
		}
		return waitcause.WaitingHuman
	}
	var calls []bool
	in := windowsIn(account, []tenant.Strategy{closedStrategy(100, 1)}, cause, open, &calls)
	pol, tag, gotOpen := ResolveWithWindows(in)
	if len(calls) != 2 || !calls[0] || calls[1] {
		t.Fatalf("both readings must be resolved (wide, then today): %v", calls)
	}
	if tag != "" || pol.MaxPings != 3 || gotOpen != open {
		t.Fatalf("today's policy must win over a wide one that plans fewer pings: tag=%q pings=%d", tag, pol.MaxPings)
	}
	// With a strategy that plans MORE pings, the wide reading stands.
	calls = nil
	in = windowsIn(account, []tenant.Strategy{closedStrategy(100, 6)}, cause, open, &calls)
	if pol, tag, _ = ResolveWithWindows(in); tag != "s" || pol.MaxPings != 6 {
		t.Fatalf("a wide reading that plans more pings must stand: tag=%q pings=%d", tag, pol.MaxPings)
	}
}

func TestResolveWithWindowsAOneHourIdleDropsTheReading(t *testing.T) {
	// The wide reading chose a 1-hour strategy (Idle >= 300 s): it does not take the plan, so the
	// reading does not apply, today's policy stands and the open counts are dropped.
	account := Policy{KeepAlive: true, WaitPlan: true, MaxPings: 3, Idle: 240 * time.Second, MinPrefixTokens: 1000}
	open := &TaskCounts{}
	cause := func(o *TaskCounts) string {
		if o != nil {
			return waitcause.WaitingBackground
		}
		return waitcause.WaitingHuman
	}
	var calls []bool
	in := windowsIn(account, []tenant.Strategy{closedStrategy(3000, 2)}, cause, open, &calls)
	pol, tag, gotOpen := ResolveWithWindows(in)
	if tag != "" || pol.MaxPings != 3 || gotOpen != nil {
		t.Fatalf("tag=%q pings=%d open=%v, want today's policy and nil open", tag, pol.MaxPings, gotOpen)
	}
}

func TestResolveWithWindowsOneHourTierSkipsTheMergedCause(t *testing.T) {
	account := Policy{WaitPlan: true, MaxPings: 2, Idle: 240 * time.Second}
	var calls []bool
	causeCalls := 0
	in := windowsIn(account, nil, func(o *TaskCounts) string {
		causeCalls++
		if o != nil {
			t.Fatal("the merged cause must not be read on the 1-hour tier")
		}
		return waitcause.WaitingHuman
	}, &TaskCounts{}, &calls)
	in.OneHourTier = func() bool { return true }
	ResolveWithWindows(in)
	if causeCalls != 1 {
		t.Fatalf("only the tracker-only cause is read, got %d reads", causeCalls)
	}
}

func TestCanKeepAliveReadsToolSeenOnlyWithThePlan(t *testing.T) {
	seen := func() bool { t.Fatal("toolSeen read without the plan"); return false }
	if !CanKeepAlive(Policy{KeepAlive: true, Idle: time.Minute, MaxPings: 1}, seen) {
		t.Fatal("an on policy without the plan can ping")
	}
	if CanKeepAlive(Policy{}, seen) {
		t.Fatal("an off policy cannot ping")
	}
	on := Policy{KeepAlive: true, Idle: time.Minute, MaxPings: 1, WaitPlan: true}
	if CanKeepAlive(on, func() bool { return false }) {
		t.Fatal("with the plan, a thread with no tool call yet is not held")
	}
	if !CanKeepAlive(on, func() bool { return true }) {
		t.Fatal("with the plan and a tool call seen, the thread is held")
	}
}

func TestOneHourFirstForGatesBeforeResolving(t *testing.T) {
	k := newTestKeeper()
	resolve := func() Policy { t.Fatal("policy resolved for a request that is never relabelled"); return Policy{} }
	for _, c := range []struct {
		session, thread string
		tools           bool
	}{{"s", "sub1", true}, {"s", "", false}, {"", "", true}} {
		if k.OneHourFirstFor(resolve, "t", c.session, c.thread, "anthropic", "e", "r", "m", c.tools) {
			t.Fatalf("%+v must be false", c)
		}
	}
	// A resolved policy with neither 1-hour mode on is false without asking the keeper.
	if k.OneHourFirstFor(func() Policy { return Policy{} }, "t", "s", "", "anthropic", "e", "r", "m", true) {
		t.Fatal("no 1-hour mode: false")
	}
}

func TestNormalizeSignalClosedSets(t *testing.T) {
	cases := []struct {
		event, kind, want string
		ok                bool
	}{
		{"SessionEnd", "clear", "clear", true},
		{"SessionEnd", "a-new-reason", "other", true}, // a newer client's reason still ends the session
		{"SessionStart", "compact", "compact", true},
		{"SessionStart", "a-new-source", "a-new-source", false}, // only SessionEnd is read as "other"
		{"Notification", "idle_prompt", "idle_prompt", true},
		{"Bogus", "", "", false},
	}
	for _, c := range cases {
		got, ok := NormalizeSignal(c.event, c.kind)
		if got != c.want || ok != c.ok {
			t.Errorf("%s/%s: got %q %v, want %q %v", c.event, c.kind, got, ok, c.want, c.ok)
		}
	}
}

func TestApplyClientSignal(t *testing.T) {
	k := newTestKeeper()
	now := t0
	var seen []SignalResult
	hook := func(r SignalResult) { seen = append(seen, r) }

	if r := k.ApplyClientSignal("t", "s", "Notification", "idle_prompt", now, hook); r.Applied || len(seen) != 0 {
		t.Fatal("a notification is accepted and never acted on")
	}
	if r := k.ApplyClientSignal("t", "s", "SessionStart", "startup", now, hook); r.Applied {
		t.Fatal("a plain SessionStart is never acted on")
	}
	r := k.ApplyClientSignal("t", "s", "SessionStart", "compact", now, hook)
	if !r.Applied || r.Ended || !r.Cutoff.Equal(now.Add(-CompactRaceMargin)) || len(seen) != 1 {
		t.Fatalf("compact: %+v hook=%d", r, len(seen))
	}
	if k.EndedAfter("t", "s", now) {
		t.Fatal("a compaction must not mark the session ended")
	}
	r = k.ApplyClientSignal("t", "s", "SessionEnd", "clear", now, hook)
	if !r.Applied || !r.Ended || !r.Cutoff.IsZero() || len(seen) != 2 {
		t.Fatalf("end: %+v hook=%d", r, len(seen))
	}
	if !k.EndedAfter("t", "s", now) || k.EndedAfter("t", "other", now) {
		t.Fatal("SessionEnd must mark exactly the named session ended")
	}
	if (*Keeper)(nil).ApplyClientSignal("t", "s", "SessionEnd", "", now, nil).Applied {
		t.Fatal("a nil keeper applies nothing")
	}
}

func TestManualPingBlock(t *testing.T) {
	last, ttl := t0, 5*time.Minute
	if got := ManualPingBlock(false, last, ttl, last.Add(4*time.Minute)); got != "" {
		t.Fatalf("a warm entry may be pinged, got %q", got)
	}
	if got := ManualPingBlock(false, last, ttl, last.Add(5*time.Minute)); got != ManualBlockCold {
		t.Fatalf("at the TTL edge the entry is cold, got %q", got)
	}
	if got := ManualPingBlock(true, last, ttl, last.Add(10*time.Minute)); got != ManualBlockAtOneHour {
		t.Fatalf("a live 1-hour touch comes before the cold gate, got %q", got)
	}
}

func TestManualSavedUSD(t *testing.T) {
	if got := ManualSavedUSD(1.0, ManualResult{CostUSD: 0.1, AvoidedUSD: 1.0}); got != 0.9 {
		t.Fatalf("got %v, want 0.9", got)
	}
	if got := ManualSavedUSD(1.0, ManualResult{CostUSD: 0.1}); got != 0 {
		t.Fatalf("a ping that read nothing saved nothing, got %v", got)
	}
	if got := ManualSavedUSD(0.05, ManualResult{CostUSD: 0.1, AvoidedUSD: 0.05}); got != 0 {
		t.Fatalf("a ping that cost more than the write saved nothing, got %v", got)
	}
}

func TestOverrideWorstCasePingsIsAPingPerIdleNotPerSpan(t *testing.T) {
	o := Override{Pol: Policy{Idle: 240 * time.Second, MaxPings: 2}, Until: t0.Add(time.Hour)}
	if got := o.WorstCasePings(t0); got != 15 { // 3600 / 240, not (K+1)-span counting
		t.Fatalf("got %d, want 15", got)
	}
	if got := o.WorstCasePings(t0.Add(2 * time.Hour)); got != 1 {
		t.Fatalf("an expired override still shows at least 1, got %d", got)
	}
	if got := (Override{}).WorstCasePings(t0); got != 0 {
		t.Fatalf("a zero idle gives 0, got %d", got)
	}
	if got := PingUSD(1000, 0.000001, 0.01); got != 0.011 {
		t.Fatalf("got %v", got)
	}
}

func newTestKeeper() *Keeper {
	fd := newFake(func(DispatchRequest) (Usage, int, error) { return Usage{}, 200, nil })
	return New(Options{Dispatcher: fd, Now: func() time.Time { return t0 }, Go: func(f func()) { f() }})
}
