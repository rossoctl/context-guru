package keepalive

import (
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/tenant"
)

// A strategy that is active at every hour and targets every tenant.
func allDay(id string, idle int, pings int, updated time.Time) tenant.Strategy {
	return tenant.Strategy{ID: id, Active: true, Target: tenant.Target{Mode: tenant.TargetAll},
		Windows:     []tenant.Window{{Start: "00:00", End: "23:59", TZ: "UTC"}},
		IdleSeconds: idle, MaxPings: pings, UpdatedAt: updated}
}

var t0 = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func TestBetterStrategyPrefersAListTargetThenTheNewest(t *testing.T) {
	list := allDay("list", 200, 2, t0)
	list.Target = tenant.Target{Mode: tenant.TargetList, TenantIDs: []string{"a"}}
	all := allDay("all", 100, 2, t0.Add(time.Hour))
	if got := BestStrategy([]tenant.Strategy{all, list}, "a", t0, false); got == nil || got.ID != "list" {
		t.Fatalf("a list target must beat a newer all target, got %+v", got)
	}
	old, newer := allDay("old", 100, 2, t0), allDay("new", 100, 2, t0.Add(time.Minute))
	if got := BestStrategy([]tenant.Strategy{old, newer}, "a", t0, false); got.ID != "new" {
		t.Fatalf("among equals the newest wins, got %s", got.ID)
	}
}

func TestBestStrategyIgnoreWindowsFindsAClosedWindow(t *testing.T) {
	s := allDay("s", 200, 2, t0)
	s.Windows = []tenant.Window{{Start: "01:00", End: "02:00", TZ: "UTC"}} // closed at 12:00
	list := []tenant.Strategy{s}
	if BestStrategy(list, "a", t0, false) != nil {
		t.Fatal("a closed window must not match")
	}
	if got := BestStrategy(list, "a", t0, true); got == nil || got.ID != "s" {
		t.Fatalf("ignoring windows must find the strategy, got %v", got)
	}
	s.Active = false
	if BestStrategy([]tenant.Strategy{s}, "a", t0, true) != nil {
		t.Fatal("an inactive strategy never applies, windows or not")
	}
}

func TestApplyStrategyOnlyTurnsOneHourOn(t *testing.T) {
	best := allDay("s", 200, 3, t0)
	best.MinPrefixTokens, best.MaxUSDPerPing = 500, 0.05
	pol, id := ApplyStrategy(Policy{OneHourOnWait: true, OneHourAlways: true, WaitPlan: true}, &best)
	if id != "s" || !pol.KeepAlive || pol.Idle != 200*time.Second || pol.MaxPings != 3 ||
		pol.MinPrefixTokens != 500 || pol.MaxUSDPerPing != 0.05 {
		t.Fatalf("strategy fields not applied: %+v id=%q", pol, id)
	}
	if !pol.OneHourOnWait || !pol.OneHourAlways || !pol.WaitPlan {
		t.Fatalf("a strategy must never turn an account's 1-hour or wait plan off: %+v", pol)
	}
	if got, id := ApplyStrategy(Policy{MaxPings: 9}, nil); id != "" || got.MaxPings != 9 {
		t.Fatalf("nil strategy must be a no-op, got %+v %q", got, id)
	}
}

func TestResolveHeadTTLOnlyTurnsItOn(t *testing.T) {
	s := allDay("s", 3000, 2, t0)
	s.HeadTTL1h, s.HeadTTLMinTokens = true, 1000
	on, min := ResolveHeadTTL([]tenant.Strategy{s}, "a", t0, false, 0)
	if !on || min != 4000 {
		t.Fatalf("got %v %d, want on and 4000 bytes (1000 tokens x 4)", on, min)
	}
	s.HeadTTL1h = false
	if on, min := ResolveHeadTTL([]tenant.Strategy{s}, "a", t0, true, 77); !on || min != 77 {
		t.Fatalf("a strategy without the 1h head must leave the account's value, got %v %d", on, min)
	}
}

func TestResolveOrder(t *testing.T) {
	account := Policy{KeepAlive: true, Idle: 240 * time.Second, MaxPings: 2, WaitPlan: true, OpenAIIdle: 28 * time.Minute}
	strat := allDay("s", 100, 5, t0)
	camp := CampaignDecision{Active: true, Tag: "campaign:c", Apply: func(p Policy) Policy { p.MaxPings = 7; return p }}
	ov := Override{Pol: Policy{KeepAlive: true, Idle: 120 * time.Second, MaxPings: 4}, Until: t0.Add(time.Hour)}
	closed := strat
	closed.Windows = []tenant.Window{{Start: "01:00", End: "02:00", TZ: "UTC"}}

	cases := []struct {
		name      string
		in        ResolveInput
		wantTag   string
		wantPings int
		wantIdle  time.Duration
		wantPlan  bool
	}{
		{"account only", ResolveInput{Now: t0}, "", 2, 240 * time.Second, true},
		{"strategy over account", ResolveInput{Now: t0, Strategies: []tenant.Strategy{strat}}, "s", 5, 100 * time.Second, true},
		{"strategy beats campaign", ResolveInput{Now: t0, Strategies: []tenant.Strategy{strat}, Campaign: camp}, "s", 5, 100 * time.Second, true},
		{"campaign owns the count and drops the plan", ResolveInput{Now: t0, Campaign: camp}, "campaign:c", 7, 240 * time.Second, false},
		{"closed window with campaign: campaign wins", ResolveInput{Now: t0, Strategies: []tenant.Strategy{closed}, Campaign: camp, IgnoreWindows: true}, "campaign:c", 7, 240 * time.Second, false},
		{"closed window, no campaign, windows ignored: strategy", ResolveInput{Now: t0, Strategies: []tenant.Strategy{closed}, IgnoreWindows: true}, "s", 5, 100 * time.Second, true},
		{"closed window, windows kept: account", ResolveInput{Now: t0, Strategies: []tenant.Strategy{closed}}, "", 2, 240 * time.Second, true},
		{"override wins over strategy and drops the plan", ResolveInput{Now: t0, Strategies: []tenant.Strategy{strat}, Override: &ov}, "s", 4, 120 * time.Second, false},
	}
	for _, c := range cases {
		c.in.TenantID = "a"
		got, tag := Resolve(account, c.in)
		if tag != c.wantTag || got.MaxPings != c.wantPings || got.Idle != c.wantIdle || got.WaitPlan != c.wantPlan {
			t.Errorf("%s: got tag=%q pings=%d idle=%v plan=%v, want tag=%q pings=%d idle=%v plan=%v",
				c.name, tag, got.MaxPings, got.Idle, got.WaitPlan, c.wantTag, c.wantPings, c.wantIdle, c.wantPlan)
		}
	}
}

func TestResolveOpenAISkipsStrategyAndCampaign(t *testing.T) {
	account := Policy{KeepAlive: true, Idle: 240 * time.Second, MaxPings: 2, OpenAIIdle: 28 * time.Minute}
	camp := CampaignDecision{Active: true, Tag: "campaign:c", Apply: func(p Policy) Policy { p.MaxPings = 7; return p }}
	got, tag := Resolve(account, ResolveInput{TenantID: "a", OpenAI: true, Now: t0,
		Strategies: []tenant.Strategy{allDay("s", 100, 5, t0)}, Campaign: camp})
	if tag != "" || got.MaxPings != 2 || got.Idle != 28*time.Minute {
		t.Fatalf("OpenAI must take its own idle and no strategy or campaign: %+v %q", got, tag)
	}
	got, _ = Resolve(Policy{}, ResolveInput{TenantID: "a", OpenAI: true, Now: t0})
	if got.Idle <= 0 {
		t.Fatal("an unset OpenAI idle must fall back to the default")
	}
	// An override holds the credential for at most MaxOverrideHold, whatever the account configured.
	ov := Override{Pol: Policy{MinPrefixTokens: 9, MaxPings: 11}, Until: t0.Add(time.Hour)}
	got, _ = Resolve(account, ResolveInput{TenantID: "a", OpenAI: true, Now: t0, Override: &ov})
	if !got.KeepAlive || got.MinPrefixTokens != 9 || time.Duration(got.MaxPings+1)*got.Idle > MaxOverrideHold {
		t.Fatalf("OpenAI override must enable, set the floor and bound the hold: %+v", got)
	}
}

func TestOverrideApplyBoundsTheOneHourCount(t *testing.T) {
	ov := Override{Pol: Policy{Idle: 120 * time.Second, MaxPings: 2}}
	got := ov.Apply(Policy{MaxUSDPerPing: 0.01})
	if got.MaxUSDPerPing != 0.01 {
		t.Fatal("an override must never widen or reset the per-ping ceiling")
	}
	if got.MaxPings1h <= 0 || got.MaxPings1h > 2 {
		t.Fatalf("the 1-hour count must be bounded by the override's count, got %d", got.MaxPings1h)
	}
}

func TestValidOverrideBounds(t *testing.T) {
	now := t0
	ok := func(session string, idle time.Duration, pings, minPrefix int, until time.Time) error {
		_, err := ValidOverride(session, idle, pings, minPrefix, until, now, "me")
		return err
	}
	if err := ok("s", 240*time.Second, 2, 0, now.Add(time.Hour)); err != nil {
		t.Fatalf("a valid override was refused: %v", err)
	}
	bad := map[string]error{
		"empty session":     ok("", 240*time.Second, 2, 0, now.Add(time.Hour)),
		"foreign session":   ok("other:x", 240*time.Second, 2, 0, now.Add(time.Hour)),
		"expired":           ok("s", 240*time.Second, 2, 0, now),
		"too long":          ok("s", 240*time.Second, 2, 0, now.Add(13*time.Hour)),
		"idle too low":      ok("s", 59*time.Second, 2, 0, now.Add(time.Hour)),
		"idle too high":     ok("s", 291*time.Second, 2, 0, now.Add(time.Hour)),
		"no pings":          ok("s", 240*time.Second, 0, 0, now.Add(time.Hour)),
		"too many pings":    ok("s", 240*time.Second, 12, 0, now.Add(time.Hour)),
		"negative floor":    ok("s", 240*time.Second, 2, -1, now.Add(time.Hour)),
		"NUL in session id": ok("s\x00x", 240*time.Second, 2, 0, now.Add(time.Hour)),
	}
	for name, err := range bad {
		if err == nil {
			t.Errorf("%s: want a refusal, got nil", name)
		}
	}
	// 11 pings x 290 s = 58 min: the hold ceiling is not reachable, so the largest pair passes.
	if err := ok("s", 290*time.Second, 11, 0, now.Add(time.Hour)); err != nil {
		t.Errorf("the largest valid pair was refused: %v", err)
	}
}

func TestStrategyAndOverrideShareTheIdleBand(t *testing.T) {
	// One band check serves both, so the 5-minute message names the same bounds.
	_, oerr := ValidOverride("s", 30*time.Second, 2, 0, t0.Add(time.Hour), t0, "me")
	serr := ValidStrategyBounds(30*time.Second, 2, 0, 0, false, 0)
	if oerr == nil || serr == nil {
		t.Fatal("both must refuse an idle of 30 s")
	}
	if !strings.Contains(oerr.Error(), "between 60 and 290 seconds") || !strings.Contains(serr.Error(), "between 60 and 290 seconds") {
		t.Fatalf("band message differs: %q / %q", oerr, serr)
	}
	if err := ValidStrategyBounds(3590*time.Second, 3, 0, 0, true, 1000); err != nil {
		t.Fatalf("the 1-hour band maximum was refused: %v", err)
	}
	if ValidStrategyBounds(3590*time.Second, 4, 0, 0, true, 1000) == nil {
		t.Fatal("4 pings at the 1-hour tier must be refused")
	}
	if ValidStrategyBounds(240*time.Second, 2, 0, 0, true, 0) == nil {
		t.Fatal("a 1-hour head needs a positive token floor")
	}
}

func TestEffectiveWaitPlan(t *testing.T) {
	account := Policy{WaitPlan: true, ToolWaitMaxPings: 3, KeepAlive: true, MaxPings: 9}
	long := allDay("h", 3000, 2, t0)
	short := allDay("s", 200, 2, t0)
	if got := EffectiveWaitPlan(account, nil, "a", t0, false); !got.WaitPlan || got.ToolWaitMaxPings != 3 || got.MaxPings != 0 {
		t.Fatalf("no strategy, no campaign: the account plan only, got %+v", got)
	}
	if got := EffectiveWaitPlan(account, nil, "a", t0, true); got.WaitPlan {
		t.Fatal("an active campaign owns the count: the plan is off")
	}
	if got := EffectiveWaitPlan(account, []tenant.Strategy{long}, "a", t0, false); got.WaitPlan {
		t.Fatal("a 1-hour-band strategy turns the plan off")
	}
	if got := EffectiveWaitPlan(account, []tenant.Strategy{short}, "a", t0, true); !got.WaitPlan {
		t.Fatal("a covering short strategy skips the campaign and keeps the plan")
	}
}

func TestFromConfigParkedBlockKeepsOnlyThePlan(t *testing.T) {
	got := FromConfig(config.CacheConfig{})
	if got.On() || got.Idle != 0 || got.MaxPings != 0 || got.HeadTTLMinBytes != 0 {
		t.Fatalf("a parked block must not come out looking enabled: %+v", got)
	}
	on := FromConfig(config.CacheConfig{KeepAlive: true})
	if !on.On() || on.Idle <= 0 || on.MaxPings <= 0 {
		t.Fatalf("an enabled block must resolve its defaults: %+v", on)
	}
	if h := FromConfig(config.CacheConfig{KeepAlive: true}); h.HeadTTLMinBytes != 0 {
		t.Fatalf("a head TTL gate must stay 0 while HeadTTL1h is off: %d", h.HeadTTLMinBytes)
	}
}
