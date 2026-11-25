package campaign

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dispatch"
	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/context-guru/kvcache"
	"github.com/rossoctl/context-guru/predictive"
	"github.com/rossoctl/context-guru/waitcause"
)

func rowsOnly(reqs []*kvcache.Request, price func(model string, promptTokens int64) (pIn, pOut, pRead, pWrite float64, ok bool), sessionEnd bool, untilMs int64) []predictive.AutoRow {
	rows, _ := Rows(reqs, price, sessionEnd, untilMs)
	return rows
}

func TestRowFactsDefaultGates(t *testing.T) {
	ok := RowFacts{Turn: 1, Prefix: 80000, Ping: 0.6, Miss: 5} // a $0.60 ping that protects a $5 miss: the value rule lets it through
	if ok.DefaultK() != 2 {
		t.Fatalf("an eligible row gets %d pings, want 2", ok.DefaultK())
	}
	for name, f := range map[string]RowFacts{"first request": {Turn: 0, Prefix: 80000}, "small prefix": {Turn: 1, Prefix: 79999}, "dear ping": {Turn: 1, Prefix: 80000, Ping: 0.51, Miss: 0.2}, "two pings cost more than the miss": {Turn: 1, Prefix: 80000, Ping: 0.6, Miss: 1}} {
		if f.DefaultK() != 0 {
			t.Errorf("%s: %d pings, want 0", name, f.DefaultK())
		}
	}
	// gateway dollars are scale 1: a 400 s return rescued by the first ping is worth one rewrite minus one read, minus the ping
	f := RowFacts{U: 2, Gap: 400, Returned: true, ToEnd: 3600, Stake: 0.3375, Ping: 0.03}
	if g := f.Gain(2, AutoX, true, 1, 1); math.Abs(g-(0.3375-0.03)) > 1e-12 {
		t.Errorf("K2 on a 400 s return = %v", g)
	}
	// SessionEnd cancels the pings after a session's last response
	last := RowFacts{U: 2, ToEnd: 0, Ping: 0.03}
	if g := last.Gain(3, AutoX, true, 1, 1); g != 0 {
		t.Errorf("with SessionEnd the last response costs %v, want 0", g)
	}
	if g := last.Gain(3, AutoX, false, 1, 1); math.Abs(g+0.09) > 1e-12 {
		t.Errorf("without SessionEnd the last response costs %v, want -0.09", g)
	}
}

// segRows: everyone else favours K3. The target's "tool_use" responses come back after 1000 s (K3 rescues them) and its "end_turn" ones after
// three hours (every ping is wasted), so no single plan suits it and a plan per stop reason does.
func segRows(target string) ([]predictive.AutoRow, []RowFacts) {
	const t0 = int64(1_786_000_000_000)
	var rows []predictive.AutoRow
	var facts []RowFacts
	add := func(scope string, sessions int, gapFor func(i int) (float64, string)) {
		for s := 0; s < sessions; s++ {
			for i := 0; i < 40; i++ {
				gap, stop := gapFor(i)
				f := RowFacts{U: 2, Gap: gap, Returned: true, ToEnd: math.Inf(1), Stake: 0.3, Ping: 0.03, Turn: i, Prefix: 100000, Stop: stop, Model: "m"}
				g := make([]float64, AutoK+1)
				for k := 1; k <= AutoK; k++ {
					g[k] = f.Gain(k, AutoX, true, BenefitScale, PingScale)
				}
				rows = append(rows, predictive.AutoRow{Scope: scope, Session: fmt.Sprintf("%s-%d", scope, s), EndMs: t0 + int64(i)*3600e3 + int64(s)*1000, Gain: g})
				facts = append(facts, f)
			}
		}
	}
	for u := 0; u < 4; u++ {
		add(fmt.Sprintf("fleet-%d", u), 30, func(int) (float64, string) { return 1000, "tool_use" })
	}
	add(target, 50, func(i int) (float64, string) {
		if i%2 == 0 {
			return 1000, "tool_use"
		}
		return 3 * 3600, "end_turn"
	})
	return rows, facts
}

func TestAutoSpecSegmentedShipsAPlanPerSegment(t *testing.T) {
	rows, facts := segRows("me")
	until := int64(1_786_000_000_000) + 50*3600e3
	spec, rec := BuildSpec(rows, facts, "me", 0, until, true, "")
	if rec == "ship" {
		t.Fatalf("one plan for a tenant that needs two cannot beat K3 on held-out data: %v", spec["evidence"])
	}
	spec, rec = BuildSpec(rows, facts, "me", 0, until, true, "stop_reason")
	if rec != "ship" {
		t.Fatalf("a plan per stop reason: rec %s, evidence %v", rec, spec["evidence"])
	}
	b, _ := json.Marshal(spec)
	ps, err := predictive.ParseSpec(b)
	if err != nil {
		t.Fatal(err)
	}
	for stop, want := range map[string]int{"tool_use": 3, "end_turn": 0, "max_tokens": 3} { // an unseen value takes the fleet plan
		k, _ := ps.PlanOf(ps.RuleFor(&predictive.Facts{StopReason: stop}))
		if k < want || (want == 0 && k != 0) {
			t.Errorf("stop_reason %s runs %d pings, want %d", stop, k, want)
		}
	}
	// the default is in the gate
	found := false
	for _, p := range spec["evidence"].(map[string]any)["paired_vs_baselines"].([]any) {
		found = found || p.(map[string]any)["vs"] == "default(K2+SessionEnd)"
	}
	if !found {
		t.Error("the shipped default is not one of the gate's baselines")
	}
}

func TestSegmentPredicatesSelectTheirRowsLive(t *testing.T) {
	f := RowFacts{Hour: 7, Dow: 2, Prefix: 90000, Model: "m1", Agent: "claude-cli", Stop: "end_turn"}
	live := predictive.Facts{HourUTC: 7, DowUTC: 2, PrefixTok: 90000, Model: "m1", Agent: "claude-cli", StopReason: "end_turn"}
	other := predictive.Facts{HourUTC: 8, DowUTC: 3, PrefixTok: 10000, Model: "m2", Agent: "codex", StopReason: "tool_use"}
	for name, seg := range segFeatures {
		_, when := seg(&f)
		spec := map[string]any{"schema": predictive.SpecSchema, "menu": map[string]any{"K1": map[string]any{"gaps_s": []float64{270}}},
			"policy": map[string]any{"kind": "rules", "segments": []any{map[string]any{"id": "x", "when": when, "rule": map[string]any{"kind": "plan", "plan": "K1"}}}}}
		b, _ := json.Marshal(spec)
		ps, err := predictive.ParseSpec(b)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if k, _ := ps.PlanOf(ps.RuleFor(&live)); k != 1 {
			t.Errorf("%s: the segment does not select its own request live", name)
		}
		if k, _ := ps.PlanOf(ps.RuleFor(&other)); k != 0 {
			t.Errorf("%s: the segment selects another request", name)
		}
	}
}

// SessionEnd cancels every ping after the session's last response, whichever model's conversation the ping belongs to.
func TestSessionEndStopsPingsAcrossModels(t *testing.T) {
	const t0 = int64(1_786_000_000_000)
	a := &kvcache.Request{ID: 1, User: "u", ConversationID: "s", Model: "m1", TS: t0, UpstreamMs: 1000, CachedContext: 100000, CostKnown: true}
	b := &kvcache.Request{ID: 2, User: "u", ConversationID: "s", Model: "m2", TS: t0 + 300e3, UpstreamMs: 1000, CachedContext: 100000, CostKnown: true}
	_, facts := Rows([]*kvcache.Request{a, b}, flatPrice, true, t0+10*3600e3)
	// a's model never comes back; the session's last response ends 300 s after a's: only a's first ping (at ~270 s) is sent
	if fired, _ := facts[0].Outcome(3, AutoX, true); fired != 1 {
		t.Errorf("a: %d pings with SessionEnd, want 1", fired)
	}
	if fired, _ := facts[0].Outcome(3, AutoX, false); fired != 3 {
		t.Errorf("a: %d pings without SessionEnd, want 3", fired)
	}
	if fired, _ := facts[1].Outcome(3, AutoX, true); fired != 0 {
		t.Errorf("b, the last response: %d pings with SessionEnd, want 0", fired)
	}
}

func TestExpectedReplayChargesFailuresAndOtherSessionsHits(t *testing.T) {
	f := RowFacts{U: 2, Gap: 400, Returned: true, ToEnd: 3600, Span: math.Inf(1), Prefix: 100000, NextPrefix: 60000, PRead: 3e-7, PWrite: 3.75e-6, Ping: 0.03, Model: "claude-sonnet-x"}
	p := &FidParams{H: 0.2, Head: map[string]float64{"claude-sonnet-x": 10000}}
	rescue, ping, pings, rescues := f.Expected(1, AutoX, true, p)
	stake := (60000 - 10000) * (3.75e-6 - 3e-7) // the next request reuses 60K, of which a miss would still read the 10K head
	if want := stake * (1 - 0.013 - 0.2); math.Abs(rescue-want) > 1e-12 || math.Abs(rescues-(1-0.013-0.2)) > 1e-12 {
		t.Errorf("rescue %v (%v), want %v", rescue, rescues, want)
	}
	if want := 0.03 + FidPFirst*100000*(3.75e-6-3e-7); math.Abs(ping-want) > 1e-12 || pings != 1 {
		t.Errorf("ping %v x %v, want %v x 1", ping, pings, want)
	}
	// a return past the cliff of the last touch is no rescue; one before the cliff of the response needs no ping
	for _, gap := range []float64{271 + 2 + 290, 200} {
		g := f
		g.Gap = gap
		if r, _, _, _ := g.Expected(1, AutoX, true, p); r != 0 {
			t.Errorf("gap %v: rescue %v, want 0", gap, r)
		}
	}
}

// The ship gate is one rule for the build and the report: tying blanket K3 fails it even when the plan beats everything else.
func TestShipGateNeedsToBeatK3TheFleetAndTheDefault(t *testing.T) {
	sess := make([]string, 200)
	sub, worse := make([]float64, 200), make([]float64, 200)
	for i := range sub {
		sess[i] = fmt.Sprintf("s%d", i%40)
		sub[i], worse[i] = 1, 0.5
	}
	if ok, _ := ShipGate(sub, GateLegs(worse, worse, worse, 3), sess); !ok {
		t.Error("a plan that beats every leg must pass")
	}
	for leg := 0; leg < 3; leg++ {
		v := [3][]float64{worse, worse, worse}
		v[leg] = sub
		if ok, failed := ShipGate(sub, GateLegs(v[0], v[1], v[2], 3), sess); ok || len(failed) != 1 {
			t.Errorf("tying leg %d passes (failed %v)", leg, failed)
		}
	}
	if n := len(GateLegs(worse, worse, nil, 3)); n != 2 {
		t.Errorf("without a default the gate has %d legs, want K3 and the fleet plan", n)
	}
}

// The keeper holds one entry per thread of a session: a request on another thread does not end this thread's pings.
func TestPingSpanIsPerThread(t *testing.T) {
	const t0 = int64(1_786_000_000_000)
	a := &kvcache.Request{ID: 1, User: "u", ConversationID: "s", Model: "m", TS: t0, UpstreamMs: 1000, CachedContext: 100000, CostKnown: true}
	b := &kvcache.Request{ID: 2, User: "u", ConversationID: "s", ThreadID: "a:sub", Model: "m", TS: t0 + 100e3, UpstreamMs: 1000, CachedContext: 100000, CostKnown: true}
	c := &kvcache.Request{ID: 3, User: "u", ConversationID: "s", Model: "m", TS: t0 + 2000e3, UpstreamMs: 1000, CachedContext: 100000, CostKnown: true}
	_, facts := Rows([]*kvcache.Request{a, b, c}, flatPrice, false, t0+10*3600e3)
	if facts[0].Span != 1999 {
		t.Errorf("the main thread's span = %v s, want 1999 (its own next request, not the sub-agent's at 99 s)", facts[0].Span)
	}
	p := &FidParams{Head: map[string]float64{}}
	if _, _, pings, _ := facts[0].Expected(3, AutoX, false, p); pings < 2.9 {
		t.Errorf("the sub-agent's request stopped the main thread's pings: %v", pings)
	}
}

// The adapt replay gives a row the floor of its wait, as the keeper does. It reads the row's stored
// wait cause and stop reason through keepalive.Policy.FloorFor: 20k for a tool, background or teammate
// wait and for a subagent, the general floor for a person or an unknown wait, and the general floor only
// for OpenAI (the keeper plans no wait for OpenAI). A tool stop stored as human, done or retry keeps that
// cause, so it gets the general floor.
func TestRowFloorFollowsTheWaitAndTheProvider(t *testing.T) {
	pol := keepalive.Policy{WaitPlan: true, MinPrefixTokens: 80000}
	for _, c := range []struct {
		name string
		f    RowFacts
		want int
	}{
		{"main tool stop", RowFacts{Stop: "tool_use"}, 20000},
		{"main end_turn", RowFacts{Stop: "end_turn"}, 80000},
		{"subagent end_turn", RowFacts{Stop: "end_turn", Subagent: true}, 20000},
		{"main background wait", RowFacts{Stop: "end_turn", Cause: "background"}, 20000},
		{"main teammate wait", RowFacts{Stop: "end_turn", Cause: "teammate"}, 20000},
		{"main own-tool wait", RowFacts{Stop: "end_turn", Cause: "own_tool"}, 20000},
		{"subagent human wait", RowFacts{Stop: "end_turn", Cause: "human", Subagent: true}, 20000},
		{"main person wait", RowFacts{Stop: "end_turn", Cause: "human"}, 80000},
		{"main unknown wait", RowFacts{Stop: "end_turn", Cause: "unknown"}, 80000},
		{"old row, no cause, no stop", RowFacts{}, 80000},
		{"old row on a subagent", RowFacts{Subagent: true}, 20000},
		{"tool stop stored as human stays human", RowFacts{Stop: "tool_use", Cause: "human"}, 80000},
		{"tool stop stored as background", RowFacts{Stop: "tool_use", Cause: "background"}, 20000},
		{"openai background", RowFacts{Stop: "end_turn", Cause: "background", OpenAI: true}, 80000},
		{"openai tool stop", RowFacts{Stop: "tool_calls", OpenAI: true}, 80000},
		{"openai subagent", RowFacts{Stop: "end_turn", OpenAI: true, Subagent: true}, 80000},
	} {
		if got := RowFloor(pol, &c.f); got != c.want {
			t.Errorf("%s: floor %d, want %d", c.name, got, c.want)
		}
	}
}

func flatPrice(string, int64) (float64, float64, float64, float64, bool) {
	return 3e-6, 15e-6, 3e-7, 3.75e-6, true
}

func TestAutoRowsScoreEachPlanInClosedForm(t *testing.T) {
	const t0 = int64(1_786_000_000_000)
	a := &kvcache.Request{ID: 1, User: "u", ConversationID: "s", TS: t0, UpstreamMs: 2000, CachedContext: 100000, CostKnown: true, HasNext: true, NextTS: t0 + 2000 + 400e3}
	b := &kvcache.Request{ID: 2, User: "u", ConversationID: "s", TS: t0 + 2000 + 400e3, UpstreamMs: 1000, CachedContext: 100000, CostKnown: true}
	late := &kvcache.Request{ID: 3, User: "u", ConversationID: "s2", TS: t0 + 3*3600e3, UpstreamMs: 1000, CachedContext: 100000, CostKnown: true}
	until := t0 + 2*3600e3
	for _, se := range []bool{false, true} {
		rows := rowsOnly([]*kvcache.Request{a, b, late}, flatPrice, se, until)
		if len(rows) != 2 {
			t.Fatalf("session_end=%v: %d rows, want 2 (the row ending after until-2700s is not resolved)", se, len(rows))
		}
		stake, ping := 100000*(3.75e-6-3e-7), 100000*3e-7+7*3e-6+15e-6
		// a returns 400 s after its response: the first ping (279 s) keeps the entry alive to the return; the second would start after it
		for k := 1; k <= AutoK; k++ {
			if want := BenefitScale*stake - PingScale*ping; math.Abs(rows[0].Gain[k]-want) > 1e-12 {
				t.Errorf("K%d on a 400 s return = %v, want %v", k, rows[0].Gain[k], want)
			}
		}
		// b is the session's last response: without the signal every ping is wasted; with it none is sent
		for k := 1; k <= AutoK; k++ {
			want := -PingScale * float64(k) * ping
			if se {
				want = 0
			}
			if math.Abs(rows[1].Gain[k]-want) > 1e-12 {
				t.Errorf("session_end=%v: K%d on the last response = %v, want %v", se, k, rows[1].Gain[k], want)
			}
		}
	}
}

// synthRows: the fleet favours K3; the target tenant returns about 400 s after each response, so K1 is all it needs.
func synthRows(target string, targetSessions int) []predictive.AutoRow {
	rng := rand.New(rand.NewSource(7))
	const t0 = int64(1_786_000_000_000)
	var rows []predictive.AutoRow
	add := func(scope string, sessions int, best int) {
		for s := 0; s < sessions; s++ {
			for i := 0; i < 30; i++ {
				g := make([]float64, AutoK+1)
				for k := 1; k <= AutoK; k++ {
					g[k] = 0.2*rng.NormFloat64() - 0.1*float64(k)
					if k >= best {
						g[k] += 1
					}
				}
				rows = append(rows, predictive.AutoRow{Scope: scope, Session: fmt.Sprintf("%s-%d", scope, s), EndMs: t0 + int64(s*30+i)*60e3, Gain: g})
			}
		}
	}
	for u := 0; u < 8; u++ {
		add(fmt.Sprintf("fleet-%d", u), 80, 3)
	}
	add(target, targetSessions, 1)
	return rows
}

func TestAutoSpecPicksAndGatesThePlan(t *testing.T) {
	const t0 = int64(1_786_000_000_000)
	until := t0 + 80*30*60e3 + 3600e3
	spec, rec := BuildSpec(synthRows("me", 80), nil, "me", t0, until, false, "")
	ev := spec["evidence"].(map[string]any)
	if rec != "ship" || ev["subject"] != "auto(K1)" {
		t.Fatalf("a tenant that only needs K1: rec %s, subject %v, evidence %v", rec, ev["subject"], ev["paired_vs_baselines"])
	}
	seg := spec["policy"].(map[string]any)["segments"].([]any)[0].(map[string]any)["rule"].(map[string]any)
	if seg["plan"] != "K1" {
		t.Errorf("shipped rule %v, want plan K1", seg)
	}
	// a tenant that looks like the fleet gets the fleet plan and the baseline recommendation
	spec, rec = BuildSpec(synthRows("fleet-0", 0), nil, "fleet-0", t0, until, false, "")
	if rec != "baseline" {
		t.Errorf("a tenant like the fleet: rec %s, want baseline", rec)
	}
	// a tenant whose own plan failed the gate runs the fleet plan, whatever its own refit says: K1 suits its first 75% of sessions well and its
	// last 25% slightly worse than K3 (0.2 less), so its own whole-window plan is still K1, but K1 loses on the held-out last 30%
	me := synthRows("me", 80)
	for i := range me {
		if me[i].Scope == "me" && me[i].EndMs >= t0+60*30*60e3 {
			me[i].Gain[1] -= 0.2
		}
	}
	spec, rec = BuildSpec(me, nil, "me", t0, until, false, "")
	rule := spec["policy"].(map[string]any)["segments"].([]any)[0].(map[string]any)["rule"].(map[string]any)
	if rec == "ship" || rule["plan"] != "K3" {
		t.Errorf("a non-ship build must run the fleet plan K3: rec %s, rule %v, tuner %v", rec, rule, spec["evidence"].(map[string]any)["tuner"])
	}
	// too little history
	if _, rec = BuildSpec(synthRows("me", 3), nil, "me", t0, until, false, ""); rec != "insufficient_data" {
		t.Errorf("3 sessions: rec %s, want insufficient_data", rec)
	}
}

// No plan of an auto campaign, and no active campaign decision, schedules a ping later than the configured keep-alive interval.
func TestAutoPlansNeverPingLaterThanTheConfiguredInterval(t *testing.T) {
	const t0 = int64(1_786_000_000_000)
	spec, _ := BuildSpec(synthRows("me", 80), nil, "me", t0, t0+80*30*60e3+3600e3, false, "")
	for name, p := range spec["menu"].(map[string]any) {
		for _, g := range p.(map[string]any)["gaps_s"].([]float64) {
			if g > config.DefaultKeepAliveIdle {
				t.Errorf("plan %s has a %v s gap, above the configured %d s", name, g, config.DefaultKeepAliveIdle)
			}
		}
	}
	// the campaign sets the count; the spacing stays the tenant's interval (60 s stays 60 s, as without a campaign), the shipped 240 s if none
	for _, conf := range []time.Duration{0, 60 * time.Second, 240 * time.Second, 270 * time.Second} {
		want := conf
		if conf == 0 {
			want = config.DefaultKeepAliveIdle * time.Second
		}
		// Apply takes no idle: the spacing the decision was scored at cannot change the result, by the type.
		got := Apply(keepalive.Policy{KeepAlive: true, Idle: conf, MaxPings: 2}, 3, true)
		if got.Idle != want || got.MaxPings != 3 {
			t.Errorf("a campaign plan under a %v interval: %v x%d, want %v x3", conf, got.Idle, got.MaxPings, want)
		}
	}
}

func TestFidMissByFamily(t *testing.T) {
	for model, want := range map[string]float64{
		"claude-opus-5-5": 0.009, "claude-sonnet-5": 0.013, "claude-haiku-4-5": 0.017,
		"claude-opus-4-6": 0.028, "claude-fable-5-1": 0.028,
	} {
		if got := FidMiss(model); got != want {
			t.Errorf("FidMiss(%q) = %v, want %v", model, got, want)
		}
	}
}

// The shadow scorer's constants are the ones the build's replay was calibrated with.
func TestShadowConstantsMatchTheEngineCalibration(t *testing.T) {
	b, err := os.ReadFile("../scripts/campaign/engine_calibration.json")
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Benefit float64 `json:"benefit_scale"`
		Ping    float64 `json:"ping_scale"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	if c.Benefit != BenefitScale || c.Ping != PingScale {
		t.Fatalf("calibration file has %v / %v, campaign/replay.go has %v / %v", c.Benefit, c.Ping, BenefitScale, PingScale)
	}
}

// stubHost is the smallest dispatch.Dispatcher a real keeper needs to decide: it holds nothing and prices
// every model the same.
type stubHost struct{ price keepalive.Price }

func (stubHost) Do(context.Context, dispatch.Request) (dispatch.Response, error) {
	return dispatch.Response{}, dispatch.ErrNotHeld
}
func (stubHost) Release(dispatch.Ref) {}
func (h stubHost) Price(context.Context, string, string, int64) (keepalive.Price, bool) {
	return h.price, true
}

const stubBody = `{"model":"claude-sonnet-5","max_tokens":32000,"stream":true,` +
	`"tools":[{"name":"Bash","description":"run","input_schema":{"type":"object"}}],` +
	`"system":[{"type":"text","text":"you are claude code","cache_control":{"type":"ephemeral"}}],` +
	`"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`

// The replay's default gate and the live keeper's REAL record path (kaEntry.verdict through Record) agree
// on a grid of ping costs, with K = 2 and no wait cause. The replay builds its facts from the same
// prices and the same prefix. The live side sends no ping: it only decides whether it holds the thread.
func TestReplayDecidesLikeTheLiveKeeper(t *testing.T) {
	const prefix = 90_000
	base := keepalive.Price{Input: 3e-6, CacheRead: 0.3e-6, CacheWrite: 3.75e-6}
	for _, out := range []float64{1e-9, 1e-6, 20e-6, 100e-6, 1e-3, 1e-2} {
		price := base
		price.Output = out
		now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
		k := keepalive.New(keepalive.Options{Dispatcher: stubHost{price}, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
		key := keepalive.Key{Tenant: "t", Session: "s"}
		pol := keepalive.Policy{KeepAlive: true, Idle: 240 * time.Second, MaxPings: config.DefaultKeepAliveMaxPings, MinPrefixTokens: 80000}
		for _, d := range []time.Duration{-time.Second, 0} {
			h := http.Header{}
			h.Set("Authorization", "Bearer x")
			k.Record(keepalive.Record{Key: key, StartedAt: now.Add(d), Body: []byte(stubBody), Provider: "anthropic",
				Route: "/v1/messages", Status: http.StatusOK, Usage: keepalive.Usage{CacheRead: prefix},
				UsageOK: true, Policy: pol, Header: h,
				Hold: func() dispatch.Ref { return dispatch.Ref("r") }})
		}
		_, live := k.Entry(key)
		// The replay's facts: one ping reads the prefix and sends one output token; a miss is the write minus the read.
		f := RowFacts{Turn: 1, Prefix: prefix, Ping: prefix*price.CacheRead + out*1, Miss: Stake(prefix, price.CacheRead, price.CacheWrite)}
		if got := f.DefaultK() > 0; got != live {
			t.Errorf("output rate %v: replay fires %v, the live keeper holds %v (ping %.4f, miss %.4f)", out, got, live, f.Ping, f.Miss)
		}
	}
}

// With a wait cause the replay weighs the same planned count as the live wait plan, and it is never
// more permissive than live: the replay's chain is at most the keeper's.
func TestReplayChainNeverExceedsTheLiveWaitPlan(t *testing.T) {
	pol := keepalive.Policy{KeepAlive: true, MaxPings: 2, WaitPlan: true}
	for _, cause := range []string{"", waitcause.WaitingOwnTool, waitcause.WaitingBackground, waitcause.WaitingHuman} {
		for _, sub := range []bool{false, true} {
			f := &RowFacts{Cause: cause, Subagent: sub}
			if got, live := replayChain(pol, f), pol.WaitPings(cause, sub); got > live || (cause != "" && got != max(pol.MaxPings, live)) {
				t.Errorf("cause %q subagent %v: replay chain %d, live %d", cause, sub, got, live)
			}
		}
	}
	if got := replayChain(pol, &RowFacts{Cause: waitcause.WaitingOwnTool}); got != 8 {
		t.Errorf("a tool wait replays K=%d, want 8", got)
	}
}
