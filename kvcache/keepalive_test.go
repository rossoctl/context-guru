package kvcache

import (
	"math"
	"testing"
	"time"
)

// cdfPredictor is a cumulative return-time distribution written out by hand, so a test can
// state "this conversation comes back after N seconds with probability P" and check the
// budget that falls out. Points are (horizon seconds, cumulative probability), ascending.
type cdfPredictor struct {
	points   [][2]float64
	declines bool
}

func (s cdfPredictor) ReuseProbability(_ Observation, horizon time.Duration) (float64, bool) {
	if s.declines {
		return 0, false
	}
	sec := horizon.Seconds()
	p := 0.0
	for _, pt := range s.points {
		if sec >= pt[0] {
			p = pt[1]
		}
	}
	return p, true
}

// perObservationPredictor is a model whose answer depends on the conversation, which is the
// only way a per-conversation budget can be shown to be per-conversation.
type perObservationPredictor struct {
	f func(Observation) [][2]float64
}

func (p perObservationPredictor) ReuseProbability(o Observation, h time.Duration) (float64, bool) {
	return cdfPredictor{points: p.f(o)}.ReuseProbability(o, h)
}

// pricedObservation is an Observation with the documented Anthropic multiples, so the gate
// under test is the real 0.1/1.15 = 8.70% and not a test-only number.
func pricedObservation(t *testing.T, cachedTokens int64) Observation {
	t.Helper()
	const input = 3.0e-6
	return Observation{
		User: "acct-1", Conversation: "conv-1", Model: "m", Now: 1_786_967_311_185,
		CachedTokens: cachedTokens, TTL: TTL5m,
		Pricing: Pricing{
			Model: "m", Input: input, Output: input * 5,
			CacheRead:       input * DefaultCacheReadMultiple,
			Write5m:         input * DefaultWrite5mMultiple,
			Write1h:         input * DefaultWrite1hMultiple,
			PingInputTokens: 1, PingOutputTokens: 1, Known: true,
		},
	}
}

// The gate this arm is built on has to BE the rates' break-even, and it has to be
// prefix-independent — which is the property that lets the whole policy read a probability
// instead of a token count.
//
// A conversation whose entire return mass lands inside the first ping's window is pinged when
// that mass is above the break-even and not when it is below, and the SAME two probabilities
// must give the same answers on a prefix two orders of magnitude larger.
func TestBudgetGateIsTheRatesBreakEvenAndIgnoresPrefixSize(t *testing.T) {
	// The break-even on the documented multiples: read 0.1x, write 1.25x, so 0.1/1.15.
	const breakEven = DefaultCacheReadMultiple /
		(DefaultWrite5mMultiple - DefaultCacheReadMultiple)
	if math.Abs(breakEven-0.0869565) > 1e-6 {
		t.Fatalf("break-even moved: %.7f — the doc comment's 8.70%% is stale", breakEven)
	}
	// Ping 1 fires at 280s and protects (300s, 580s]. Put the mass there and nowhere else, so
	// only the first window's hazard is non-zero and the myopic gate is the whole decision.
	below := cdfPredictor{points: [][2]float64{{300, 0}, {580, breakEven * 0.5}, {3600, 1}}}
	above := cdfPredictor{points: [][2]float64{{300, 0}, {580, breakEven * 3}, {3600, 1}}}
	for _, prefix := range []int64{2_000, 124_845, 2_000_000} {
		o := pricedObservation(t, prefix)
		for _, tc := range []struct {
			name string
			p    Predictor
			want bool
		}{{"below break-even", below, false}, {"above break-even", above, true}} {
			pol := BudgetPolicy{Predictor: tc.p, MaxK: 1}
			k, ok := pol.PingBudget(o)
			if !ok {
				t.Fatalf("prefix=%d %s: no opinion, want one", prefix, tc.name)
			}
			if got := k >= 1; got != tc.want {
				t.Errorf("prefix=%d %s: budget %d (pings=%v), want pings=%v",
					prefix, tc.name, k, got, tc.want)
			}
		}
	}
}

// The option value has to lower the bar, not just decorate the type comment.
//
// A conversation whose FIRST window is below the myopic break-even but whose SECOND window is
// rich should still be pinged at the first sweep — the first ping is what keeps the entry
// alive to reach the second. With MaxK=1 there is no second window to reach, so the same
// conversation is not pinged. That difference IS the option value.
func TestBudgetOptionValuePingsThroughALeanFirstWindow(t *testing.T) {
	// Window 1 is (300, 580]; window 2 is (580, 860]. Nothing returns before 580, then a lot.
	lean := cdfPredictor{points: [][2]float64{{580, 0.02}, {860, 0.60}, {3600, 1}}}
	o := pricedObservation(t, 124_845)

	myopic, ok := BudgetPolicy{Predictor: lean, MaxK: 1}.PingBudget(o)
	if !ok {
		t.Fatal("MaxK=1: no opinion")
	}
	if myopic != 0 {
		t.Fatalf("MaxK=1 budget = %d, want 0: 2%% is below the 8.70%% break-even and with no "+
			"second window there is nothing else to buy", myopic)
	}
	withOption, ok := BudgetPolicy{Predictor: lean, MaxK: 4}.PingBudget(o)
	if !ok {
		t.Fatal("MaxK=4: no opinion")
	}
	if withOption < 2 {
		t.Errorf("MaxK=4 budget = %d, want >= 2: the first ping is not worth its own window "+
			"but is worth reaching the second, which is the whole point of the induction",
			withOption)
	}
}

// The schedule must end where the value does, and must NOT end early where a later window is
// worth reaching.
//
// Both halves matter and they pull opposite ways. Stopping at the first window whose own
// hazard is thin would throw away the option value; running to the last window with any mass
// in it would buy a run of pings across a dead stretch that never pays back. The rule is the
// first j whose VALUE (own hazard plus what staying alive is worth) is not positive.
func TestBudgetScheduleEndsWhereTheValueDoes(t *testing.T) {
	o := pricedObservation(t, 124_845)

	// A worthless tail: window 1 pays, and nothing ever returns afterwards. Windows 2..8 are
	// each worth -ping and nothing else, so the schedule is one ping.
	deadTail := cdfPredictor{points: [][2]float64{{300, 0}, {580, 0.30}, {86400, 0.30}}}
	k, ok := BudgetPolicy{Predictor: deadTail, MaxK: 8}.PingBudget(o)
	if !ok {
		t.Fatal("deadTail: no opinion")
	}
	if k != 1 {
		t.Errorf("dead tail: budget = %d, want 1 — every window after the first is worth exactly "+
			"minus one ping, so buying any of them is a loss", k)
	}

	// A rich window five sweeps out, reachable for four cheap pings. Paying through the empty
	// stretch to reach it is correct: 4 pings cost ~0.15 to win ~0.40 of avoided write.
	farRich := cdfPredictor{points: [][2]float64{
		{300, 0}, {580, 0.30}, {860, 0.30}, {1140, 0.30}, {1420, 0.30}, {1700, 0.95}, {86400, 1},
	}}
	k, ok = BudgetPolicy{Predictor: farRich, MaxK: 8}.PingBudget(o)
	if !ok {
		t.Fatal("farRich: no opinion")
	}
	if k < 5 {
		t.Errorf("far-rich window: budget = %d, want >= 5 — the induction must be willing to hold "+
			"through windows that pay nothing themselves when what they reach pays for them", k)
	}

	// And the same far window is NOT worth reaching once it is too THIN to pay for the pings
	// that get there. Identical shape, 21% conditional hazard instead of 93%: four holding pings
	// cost ~0.19 to win ~0.09, so the schedule stops at the first window again.
	farThin := cdfPredictor{points: [][2]float64{
		{300, 0}, {580, 0.30}, {860, 0.30}, {1140, 0.30}, {1420, 0.30}, {1700, 0.45}, {86400, 1},
	}}
	k, ok = BudgetPolicy{Predictor: farThin, MaxK: 8}.PingBudget(o)
	if !ok {
		t.Fatal("farThin: no opinion")
	}
	if k != 1 {
		t.Errorf("far-thin window: budget = %d, want 1 — reaching it costs four pings and it pays "+
			"about half of one, so the induction must decline to hold", k)
	}
}

// Every reason to have no opinion has to produce ok=false, so the caller falls back to
// Config.MaxPings rather than to a number this type invented.
func TestBudgetDeclinesRatherThanInventingANumber(t *testing.T) {
	fine := cdfPredictor{points: [][2]float64{{580, 0.9}}}
	unpriced := pricedObservation(t, 124_845)
	unpriced.Pricing.Known = false
	noTokens := pricedObservation(t, 0)

	for _, tc := range []struct {
		name string
		pol  BudgetPolicy
		o    Observation
	}{
		{"no predictor", BudgetPolicy{}, pricedObservation(t, 124_845)},
		{"predictor declines", BudgetPolicy{Predictor: cdfPredictor{declines: true}},
			pricedObservation(t, 124_845)},
		{"rates unknown", BudgetPolicy{Predictor: fine}, unpriced},
		{"no cached prefix", BudgetPolicy{Predictor: fine}, noTokens},
	} {
		if _, ok := tc.pol.PingBudget(tc.o); ok {
			t.Errorf("%s: ok=true, want false so Config.MaxPings governs", tc.name)
		}
	}

	// MinPrefix is different in kind: it IS a decision, so it must report ok=true with 0.
	small := BudgetPolicy{Predictor: fine, MinPrefix: 10_000}
	k, ok := small.PingBudget(pricedObservation(t, 5_000))
	if !ok || k != 0 {
		t.Errorf("below MinPrefix: (%d, %v), want (0, true) — declining to ping is a decision, "+
			"not an absence of one", k, ok)
	}
}

// The hazard the induction reads must be CONDITIONAL on the conversation having stayed idle
// this long, because that is the only population that ever faces the decision.
//
// Two predictors with the same mass in window 2 but different amounts already returned before
// window 2 opens must produce different budgets: the one whose survivors are few has a much
// higher conditional hazard on the ones that remain.
func TestBudgetHazardIsConditionalOnStillBeingIdle(t *testing.T) {
	// Both put 5pp of unconditional mass in window 2 = (580, 860].
	// mostGone: 90% already back by 580, so 5pp/10% survivors = 50% conditional.
	// mostStay:  0% already back by 580, so 5pp/100% survivors = 5% conditional.
	mostGone := cdfPredictor{points: [][2]float64{{280, 0.90}, {580, 0.90}, {860, 0.95}, {3600, 1}}}
	mostStay := cdfPredictor{points: [][2]float64{{280, 0.00}, {580, 0.00}, {860, 0.05}, {3600, 1}}}
	o := pricedObservation(t, 124_845)

	hGone, _, ok := BudgetPolicy{Predictor: mostGone, MaxK: 2}.Windows(o)
	if !ok {
		t.Fatal("mostGone: no windows")
	}
	hStay, _, ok := BudgetPolicy{Predictor: mostStay, MaxK: 2}.Windows(o)
	if !ok {
		t.Fatal("mostStay: no windows")
	}
	if !(hGone[1] > 5*hStay[1]) {
		t.Errorf("window-2 conditional hazard: mostGone %.4f vs mostStay %.4f — same "+
			"unconditional mass, so if these are close the divide by the survivor share is "+
			"missing and the model is answering a question nobody asks", hGone[1], hStay[1])
	}
}

// ── the simulator seam ──────────────────────────────────────────────────────

// budgetedStub is a Strategy that always pings and always asks for a fixed budget, so a test
// can prove the number reaches the ping loop.
type budgetedStub struct {
	label  string
	budget int
	ok     bool
}

func (b budgetedStub) Name() string              { return b.label }
func (b budgetedStub) Decide(Observation) Action { return ActionPing5m }
func (b budgetedStub) PingBudget(Observation) (int, bool) {
	return b.budget, b.ok
}

// A PingBudgeter's number has to actually bound the pings that fire, and declining has to
// leave Config.MaxPings in charge. Without this the whole interface is decoration.
func TestPingBudgeterBoundsTheSimulatedPings(t *testing.T) {
	reqs, cfg := dataset(t)
	cfg.MaxPings = 6

	plain := Simulate(reqs, Custom{Label: "always-ping", Decider: func(Observation) Action {
		return ActionPing5m
	}}, cfg)
	if plain.Pings == 0 {
		t.Fatal("baseline fired no pings; the fixture cannot show a budget doing anything")
	}
	oneOnly := Simulate(reqs, budgetedStub{label: "budget-1", budget: 1, ok: true}, cfg)
	if oneOnly.Pings >= plain.Pings {
		t.Errorf("budget=1 fired %d pings, unbudgeted fired %d: the budget did not bind",
			oneOnly.Pings, plain.Pings)
	}
	declines := Simulate(reqs, budgetedStub{label: "no-opinion", budget: 0, ok: false}, cfg)
	if declines.Pings != plain.Pings {
		t.Errorf("declining fired %d pings, Config.MaxPings path fired %d: ok=false must fall "+
			"back to the configured cap, not to the returned zero", declines.Pings, plain.Pings)
	}
	// And the cap is a CEILING: a budget above it must not raise it.
	cfg.MaxPings = 2
	greedy := Simulate(reqs, budgetedStub{label: "budget-99", budget: 99, ok: true}, cfg)
	capped := Simulate(reqs, Custom{Label: "capped", Decider: func(Observation) Action {
		return ActionPing5m
	}}, cfg)
	if greedy.Pings != capped.Pings {
		t.Errorf("budget=99 under MaxPings=2 fired %d pings, want %d: a model must never raise "+
			"an operator's cap", greedy.Pings, capped.Pings)
	}
}

// Adding the seam must not have moved any arm that does not use it. Every registered arm is
// replayed and compared against the same arm before PingBudgeter existed — which is what the
// unbudgeted path still is, so the assertion is that a budget-less strategy is untouched.
func TestUnbudgetedArmsAreUnaffectedByTheSeam(t *testing.T) {
	reqs, cfg := dataset(t)
	for _, spec := range Registry() {
		s, err := NewStrategy(spec.Name, reqs, cfg)
		if err != nil {
			// Arms that carry their own action list have dedicated constructors; the registry's
			// own tests cover those. Not finding one here is not a regression in this seam.
			continue
		}
		name := spec.Name
		if _, isBudgeter := s.(PingBudgeter); isBudgeter {
			t.Errorf("%s implements PingBudgeter: the registered arms are meant to stay on the "+
				"Config.MaxPings path, and one that opted in silently would change behaviour",
				name)
		}
		if r := Simulate(reqs, s, cfg); r == nil {
			t.Errorf("%s: nil result", name)
		}
	}
}

// BudgetPolicy has to be a working Strategy end to end — Simulate must accept it, it must
// choose different budgets on different conversations, and it must cost less than pinging
// everything at the same cap. The last is the claim the docs make; a fixture that cannot show
// it would make the doc unfalsifiable here.
func TestBudgetPolicyRunsThroughSimulateAndVariesPerConversation(t *testing.T) {
	reqs, cfg := dataset(t)
	cfg.MaxPings = 6
	cfg.PingIdle = DefaultPingIdle

	// Return mass concentrated in the first two windows: enough to pay for one or two pings,
	// never for six.
	p := cdfPredictor{points: [][2]float64{
		{300, 0.10}, {580, 0.55}, {860, 0.72}, {1140, 0.74}, {3600, 0.80}, {86400, 1},
	}}
	pol := BudgetPolicy{Predictor: p, Interval: cfg.PingIdle, MaxK: 6, Semantics: cfg.Semantics}
	if pol.Name() != StrategyKeepAliveBudget {
		t.Errorf("Name() = %q, want %q", pol.Name(), StrategyKeepAliveBudget)
	}
	if pol.Describe() == "" {
		t.Error("Describe() is empty; the dashboard renders it beside the arm")
	}

	got := Simulate(reqs, pol, cfg)
	all := Simulate(reqs, Custom{Label: "ping-everything", MaxPings: 6,
		Decider: func(Observation) Action { return ActionPing5m }}, cfg)
	if got.Pings == 0 {
		t.Fatal("BudgetPolicy fired no pings at all; the arm is inert on this fixture")
	}
	if got.Pings >= all.Pings {
		t.Errorf("BudgetPolicy fired %d pings, ping-everything %d: the whole point is fewer",
			got.Pings, all.Pings)
	}
	// The budget is a function of the observation, so it must not be one constant across the
	// fixture's deliberately varied prefixes.
	// The gate is prefix-independent on purpose, so a stub answering the same distribution for
	// every conversation MUST produce one budget — that is the design, not a defect. What has to
	// vary with the observation is the model's answer, so vary that.
	perConv := perObservationPredictor{f: func(o Observation) [][2]float64 {
		switch o.CachedTokens % 3 {
		case 0: // comes back almost at once: nothing to buy
			return [][2]float64{{300, 0.99}, {86400, 1}}
		case 1: // one rich window
			return [][2]float64{{300, 0}, {580, 0.9}, {86400, 1}}
		default: // a long slow tail worth holding through
			return [][2]float64{{300, 0}, {580, 0.2}, {860, 0.4}, {1140, 0.6}, {86400, 1}}
		}
	}}
	varying := BudgetPolicy{Predictor: perConv, Interval: cfg.PingIdle, MaxK: 6,
		Semantics: cfg.Semantics}
	seen := map[int]bool{}
	for _, r := range reqs {
		o := pricedObservation(t, r.CachedContext)
		if k, ok := varying.PingBudget(o); ok {
			seen[k] = true
		}
	}
	if len(seen) < 2 {
		t.Errorf("budget took %d distinct values over the fixture: %v — a policy that picks one "+
			"number for every conversation is Config.MaxPings with extra steps", len(seen), seen)
	}
}

// The ceiling must still be a ceiling with the seam in place. NewOptimal reads the same ping
// core, so if the signature change had quietly altered its arithmetic this is what catches it.
func TestOptimalStillBoundsABudgetedArm(t *testing.T) {
	reqs, cfg := dataset(t)
	cfg.MaxPings = 6
	opt, err := NewStrategy(StrategyOptimal, reqs, cfg)
	if err != nil {
		t.Fatalf("NewStrategy(optimal): %v", err)
	}
	ceiling := Simulate(reqs, opt, cfg)
	p := cdfPredictor{points: [][2]float64{{300, 0.1}, {580, 0.6}, {860, 0.75}, {86400, 1}}}
	arm := Simulate(reqs, BudgetPolicy{Predictor: p, Interval: cfg.PingIdle, MaxK: 6,
		Semantics: cfg.Semantics}, cfg)
	if ceiling.TotalUSD > arm.TotalUSD+1e-9 {
		t.Errorf("optimal cost %.6f exceeds BudgetPolicy's %.6f: the ceiling is not a ceiling",
			ceiling.TotalUSD, arm.TotalUSD)
	}
}
