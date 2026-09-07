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

// A schedule where a ping is NOT a cheap read has to be declined, not priced as one.
//
// The whole arm rests on one ping costing Pricing.KeepAliveCost. simulatePings only extends
// the entry when Semantics.PingRefreshesTTL, and a ping landing on a lapsed entry is billed
// Pricing.RecreateCost — 12.5x a read. So on semantics or an interval that break the read
// model, quoting an 8.70% break-even is quoting the wrong rate, and the honest answer is
// ok=false: Config.MaxPings is the operator's number and Decide fires no pings at all.
func TestBudgetDeclinesASchedulePricedOnTheWrongRate(t *testing.T) {
	fine := cdfPredictor{points: [][2]float64{{300, 0}, {580, 0.9}, {86400, 1}}}
	o := pricedObservation(t, 124_845)

	for _, tc := range []struct {
		name string
		pol  BudgetPolicy
	}{
		{"a ping does not refresh the entry",
			BudgetPolicy{Predictor: fine, Semantics: Semantics{HitRefreshesTTL: true}}},
		{"a hit does not refresh the entry, so this span's deadline is unknowable",
			BudgetPolicy{Predictor: fine, Semantics: Semantics{PingRefreshesTTL: true}}},
		{"the interval lands exactly on the lifetime",
			BudgetPolicy{Predictor: fine, Interval: TTL5m.Lifetime()}},
		{"the interval is past the lifetime it protects",
			BudgetPolicy{Predictor: fine, Interval: 400 * time.Second}},
	} {
		if k, ok := tc.pol.PingBudget(o); ok {
			t.Errorf("%s: (%d, true), want ok=false — every ping on this schedule is a 1.25x "+
				"write, so a break-even taken from the read rate is the wrong number", tc.name, k)
		}
		if a := tc.pol.Decide(o); a != ActionWrite5m {
			t.Errorf("%s: Decide = %v, want %v — an arm that cannot price the schedule must not "+
				"buy pings on it", tc.name, a, ActionWrite5m)
		}
	}

	// And the documented semantics, however they are spelled, must still be priced. The zero
	// value means DefaultSemantics() here for the same reason it does on Config.
	for _, sem := range []Semantics{{}, DefaultSemantics()} {
		pol := BudgetPolicy{Predictor: fine, Semantics: sem}
		if k, ok := pol.PingBudget(o); !ok || k < 1 {
			t.Errorf("Semantics %+v: (%d, %v), want a budget — this is the shipped provider "+
				"behaviour and the zero value has to mean it", sem, k, ok)
		}
	}
}

// The gate is prefix-independent in its PER-TOKEN part only. A ping's ping_input/ping_output
// overhead does not scale with the prefix, so the true bar rises as the prefix shrinks, and
// the doc comment now says so by how much. This pins those numbers, because "8.70% for every
// prefix" was stated four times and is out by a full point at 500 tokens — the same order as
// the margins the arm is chosen by.
func TestBudgetGateRisesOnASmallPrefixByTheFixedOverhead(t *testing.T) {
	const asymptote = DefaultCacheReadMultiple /
		(DefaultWrite5mMultiple - DefaultCacheReadMultiple)
	for _, tc := range []struct {
		prefix int64
		want   float64
	}{
		{500, 0.097391}, {2_000, 0.089565}, {20_000, 0.087217}, {124_845, 0.086998},
	} {
		o := pricedObservation(t, tc.prefix)
		// The smallest single-window hazard that buys one ping, by bisection on the real gate.
		lo, hi := 0.0, 1.0
		for i := 0; i < 60; i++ {
			mid := (lo + hi) / 2
			pol := BudgetPolicy{MaxK: 1, Predictor: cdfPredictor{
				points: [][2]float64{{300, 0}, {580, mid}, {3600, 1}}}}
			if k, _ := pol.PingBudget(o); k >= 1 {
				hi = mid
			} else {
				lo = mid
			}
		}
		if math.Abs(hi-tc.want) > 1e-5 {
			t.Errorf("prefix %d: measured gate %.6f, want %.6f", tc.prefix, hi, tc.want)
		}
		if hi < asymptote {
			t.Errorf("prefix %d: gate %.6f is BELOW the per-token break-even %.6f, which the "+
				"fixed overhead makes impossible", tc.prefix, hi, asymptote)
		}
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

// Three paths the nine tests above never reach, each of which a mutation would survive.
//
// They are cheap and they are not decorative: the first is the arm's own label, the second is
// what stops a schedule being priced past the point the conversation is certainly back, and
// the third is a predictor that answers one horizon and declines another — which a real fitted
// model does at the edge of its support.
func TestBudgetCoversItsRemainingBranches(t *testing.T) {
	fine := cdfPredictor{points: [][2]float64{{300, 0}, {580, 0.9}, {86400, 1}}}
	o := pricedObservation(t, 124_845)

	if got := (BudgetPolicy{Label: "budget-v2"}).Name(); got != "budget-v2" {
		t.Errorf("Name() = %q, want the Label — the dashboard groups by it", got)
	}

	// CERTAINLY back before the second sweep. Windows must stop, leaving the rest at zero
	// rather than dividing by a survivor share of nothing.
	certain := cdfPredictor{points: [][2]float64{{300, 0}, {580, 0.5}, {560, 1.0}}}
	h, s, ok := BudgetPolicy{Predictor: certain, MaxK: 4}.Windows(o)
	if !ok {
		t.Fatal("certain return: no windows")
	}
	for j := 1; j < len(h); j++ {
		if h[j] != 0 || s[j] != 0 {
			t.Errorf("window %d after a certain return: h=%v s=%v, want zeros — no later ping "+
				"can be needed once the conversation is back", j+1, h[j], s[j])
		}
	}

	// A predictor that answers the survivor question and declines the window question. The
	// whole vector has to be abandoned, not silently completed from the answers it did give.
	partial := horizonPredictor{f: func(horizon time.Duration) (float64, bool) {
		if horizon == DefaultPingIdle {
			return 0.1, true
		}
		return 0, false
	}}
	if _, _, ok := (BudgetPolicy{Predictor: partial, MaxK: 4}).Windows(o); ok {
		t.Error("a predictor that declined a horizon produced windows: a partly-answered vector " +
			"is not a smaller vector, it is an unusable one")
	}
	if _, ok := (BudgetPolicy{Predictor: partial, MaxK: 4}).PingBudget(o); ok {
		t.Error("PingBudget claimed an opinion on a partly-answered predictor")
	}
	// And the same policy with a predictor that answers everything still works, so the test
	// above is about the decline and not about the fixture.
	if _, ok := (BudgetPolicy{Predictor: fine, MaxK: 4}).PingBudget(o); !ok {
		t.Error("the control case declined too; the assertion above proves nothing")
	}
}

// horizonPredictor answers per HORIZON rather than per observation, which is how a model at the
// edge of its support behaves: fitted out to one sweep, nothing to say past it.
type horizonPredictor struct {
	f func(time.Duration) (float64, bool)
}

func (p horizonPredictor) ReuseProbability(_ Observation, h time.Duration) (float64, bool) {
	return p.f(h)
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

// No registered arm may opt into the seam, and a budgeted replay must not be able to reach one
// that has not.
//
// The first half is the invariant: none of the registry's arms implements PingBudgeter, so all
// of them are still budgeted by Config.MaxPings. The second is what the new mutable state on
// convState makes worth asserting — pendingBudget and budgeted are per-conversation fields
// carried across requests, and the failure they invite is one replay's budget surviving into
// another's. So each arm is replayed, a budgeted stub is replayed in between, and the arm is
// replayed again: the two runs of the same arm must be identical to the last cent.
//
// What this deliberately does NOT claim is a comparison against the code before PingBudgeter
// existed. That comparison cannot be written in-package — bypassing Simulate's type assertion
// also bypasses every OTHER optional interface a strategy may implement, so it stops being the
// same replay. The no-op property of a declining budgeter is asserted directly, on a stub, by
// TestPingBudgeterBoundsTheSimulatedPings.
func TestUnbudgetedArmsAreUnaffectedByTheSeam(t *testing.T) {
	reqs, cfg := dataset(t)
	cfg.MaxPings = 6
	covered := 0
	budgeters := map[string]bool{}
	for _, spec := range Registry() {
		s, err := NewStrategy(spec.Name, reqs, cfg)
		if err != nil {
			// Arms that carry their own action list have dedicated constructors; the registry's
			// own tests cover those. Not finding one here is not a regression in this seam.
			continue
		}
		name := spec.Name
		if _, isBudgeter := s.(PingBudgeter); isBudgeter {
			budgeters[name] = true
		}
		before := Simulate(reqs, s, cfg)
		Simulate(reqs, budgetedStub{label: "interloper", budget: 1, ok: true}, cfg)
		after := Simulate(reqs, s, cfg)
		if before == nil || after == nil {
			t.Errorf("%s: nil result", name)
			continue
		}
		covered++
		if math.Abs(before.TotalUSD-after.TotalUSD) > 1e-12 || before.Pings != after.Pings {
			t.Errorf("%s: %.12f over %d pings, then %.12f over %d after a budgeted replay ran "+
				"in between — a budget leaked out of one replay into another", name,
				before.TotalUSD, before.Pings, after.TotalUSD, after.Pings)
		}
	}
	// The set of registered arms on the budget path is an ALLOW-LIST, not "none".
	//
	// It was "none" until keepalive-budget joined the registry, and the message then read "one
	// that opted in silently would change behaviour" — which is still the failure being
	// guarded, because SILENTLY is the load-bearing word. Comparing against a named set keeps
	// that and adds the other direction: an arm dropping off this list is also a behaviour
	// change nobody asked for, and an assertion of "none" could never have caught it.
	want := map[string]bool{StrategyKeepAliveBudget: true}
	for name := range budgeters {
		if !want[name] {
			t.Errorf("%s implements PingBudgeter but is not on the allow-list: the other "+
				"registered arms are meant to stay on the Config.MaxPings path, and one that "+
				"opted in silently would change behaviour", name)
		}
	}
	for name := range want {
		if !budgeters[name] {
			t.Errorf("%s is on the PingBudgeter allow-list but does not implement it; the arm "+
				"still resolves, so it now silently runs on Config.MaxPings instead of on its "+
				"own per-conversation budget", name)
		}
	}
	// A registry that stopped resolving would make the loop above vacuous, so say how much of
	// it was actually replayed. One arm (replay) legitimately has no name-based constructor.
	if covered < len(Registry())-1 {
		t.Errorf("only %d of %d registry arms were replayed; the assertion is nearly vacuous",
			covered, len(Registry()))
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

// And the varying budget has to reach the PING LOOP, not just PingBudget.
//
// The test above calls PingBudget directly, which proves the arithmetic is per-observation and
// nothing about the seam. Simulate could memoize the first Observation it ever saw, or hold one
// budget for the whole replay, and every assertion above would still pass — verified: freezing
// the Observation Simulate hands to PingBudget leaves the whole kvcache package green.
//
// So: a budgeter that says 0 for half the conversations and the cap for the other half must
// land STRICTLY BETWEEN the two constant runs. One number for the whole replay lands on an end.
func TestAPerConversationBudgetReachesThePingLoop(t *testing.T) {
	reqs, cfg := dataset(t)
	cfg.MaxPings = 6

	half := splitBudgeter{cap: cfg.MaxPings}
	none := Simulate(reqs, budgetedStub{label: "all-zero", budget: 0, ok: true}, cfg)
	all := Simulate(reqs, budgetedStub{label: "all-cap", budget: cfg.MaxPings, ok: true}, cfg)
	got := Simulate(reqs, half, cfg)

	if none.Pings >= all.Pings {
		t.Fatalf("the two constant runs fired %d and %d pings; this fixture cannot show a split",
			none.Pings, all.Pings)
	}
	if got.Pings <= none.Pings || got.Pings >= all.Pings {
		t.Errorf("split budget fired %d pings, want strictly between %d (always 0) and %d "+
			"(always %d) — a budget that lands on either end is one number for the whole replay, "+
			"which is what the seam exists to avoid", got.Pings, none.Pings, all.Pings, cfg.MaxPings)
	}
	if half.asked() < 2 {
		t.Errorf("Simulate asked for a budget %d times: it is not asking per conversation",
			half.asked())
	}
}

// splitBudgeter budgets on the CONVERSATION, so a seam that forwards one frozen Observation
// cannot reproduce its ping count.
type splitBudgeter struct {
	cap int
	n   *int
}

func (s splitBudgeter) Name() string              { return "split-budget" }
func (s splitBudgeter) Decide(Observation) Action { return ActionPing5m }
func (s splitBudgeter) PingBudget(o Observation) (int, bool) {
	if s.n != nil {
		*s.n++
	}
	// Deterministic on the conversation id, so the split is a property of the observation and
	// not of the order Simulate happens to walk in.
	sum := 0
	for _, c := range o.Conversation {
		sum += int(c)
	}
	if sum%2 == 0 {
		return 0, true
	}
	return s.cap, true
}
func (s splitBudgeter) asked() int {
	if s.n == nil {
		return 2 // not counting; the ping-count assertion is the one with teeth
	}
	return *s.n
}

// The ceiling must still be a ceiling with the seam in place. NewOptimal reads the same ping
// core, so if the signature change had quietly altered its arithmetic this is what catches it.
// This cannot fail through BudgetPolicy's own arithmetic: Simulate's MaxPings cap bounds BOTH
// sides identically (the PingBudgeter clamp described on Config.MaxPings), so no strategy
// sharing that cap can ever cost less than `optimal` — buggy or correct. What this proves is
// that the SEAM doesn't break `optimal`'s own bookkeeping when the competing strategy happens
// to implement PingBudgeter, not that BudgetPolicy's economics are sound; the mutation-tested
// TestBudget* cases above are what actually pin the arithmetic.
func TestOptimalStillRunsCorrectlyAlongsideAPingBudgeter(t *testing.T) {
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
		t.Errorf("optimal cost %.6f exceeds BudgetPolicy's %.6f: Simulate's own ceiling "+
			"bookkeeping broke", ceiling.TotalUSD, arm.TotalUSD)
	}
}

// panickingPredictor always panics, standing in for a third-party model implementation that
// blows up — a nil pointer in a client library, a malformed response it didn't guard against.
type panickingPredictor struct{}

func (panickingPredictor) ReuseProbability(Observation, time.Duration) (float64, bool) {
	panic("predictor exploded")
}

// Predictor is injectable. CLAUDE.md's fail-open rule says a component error reverts that
// component only, so a panicking Predictor must degrade PingBudget to ok=false — the path the
// arm already handles correctly — rather than propagate through Decide into Simulate (or the
// live ping decision), which would take down more than this one component.
// BudgetPolicy.Interval is documented to have to match Config.PingIdle, but until now nothing
// checked it — a caller that let the two drift got economics silently priced for a schedule
// Simulate was not actually running.
// Simulate must not crash on a BudgetPolicy.Interval that disagrees with Config.PingIdle: it
// is reachable from an HTTP handler (dash/kvcacheapi.go -> dash/kvcachesim.go) with no
// recover() under package dash, and this package already treats a panicking Predictor as a
// bug to guard against (see TestPingBudgetDegradesRatherThanPropagatingAPredictorPanic) — a
// second panic seam right next to that one would contradict it. So Simulate overrides
// Interval to Config.PingIdle instead: the two are made incapable of disagreeing, rather than
// merely checked. A wrong Interval on the input must produce the SAME Result as a correct one.
func TestSimulateAlignsABudgetPolicysIntervalWithConfigRatherThanTrustingIt(t *testing.T) {
	reqs, cfg := dataset(t)
	cfg.PingIdle = 280 * time.Second
	cfg.MaxPings = 6
	p := cdfPredictor{points: [][2]float64{{300, 0.1}, {580, 0.6}, {860, 0.75}, {86400, 1}}}

	correct := Simulate(reqs, BudgetPolicy{Predictor: p, Interval: cfg.PingIdle, MaxK: 4}, cfg)
	wrong := Simulate(reqs, BudgetPolicy{Predictor: p, Interval: 300 * time.Second, MaxK: 4}, cfg)
	if wrong.Pings != correct.Pings || wrong.TotalUSD != correct.TotalUSD {
		t.Errorf("a BudgetPolicy built with the wrong Interval fired %d pings costing %.6f; "+
			"a correctly-configured one fired %d costing %.6f — Simulate let the wrong "+
			"Interval through instead of overriding it to Config.PingIdle",
			wrong.Pings, wrong.TotalUSD, correct.Pings, correct.TotalUSD)
	}
}

func TestPingBudgetDegradesRatherThanPropagatingAPredictorPanic(t *testing.T) {
	o := pricedObservation(t, 124_845)
	pol := BudgetPolicy{Predictor: panickingPredictor{}, MaxK: 4}
	if k, ok := pol.PingBudget(o); ok {
		t.Fatalf("PingBudget = (%d, true) from a panicking Predictor, want ok=false", k)
	}
	if a := pol.Decide(o); a != ActionWrite5m {
		t.Errorf("Decide = %v with a panicking Predictor, want %v — ok=false falls back to "+
			"Config.MaxPings rather than propagating the panic", a, ActionWrite5m)
	}
}
