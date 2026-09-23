package offload

import (
	"math"
	"strconv"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
)

// THE TWO DEFECTS THESE EXIST TO CATCH, both established from iteration 024's recorded decisions:
//
//  1. the break-even prices a removal at the cache reads it saves, and the one run where this component
//     demonstrably helped paid 28:1 against that price ($20.26 spent, $0.72 banked) while task accuracy
//     rose 0.486 -> 0.608 with 8 tasks better and 0 worse. Priced as shipped, the gate authorises 19% of
//     the removal value that produced that result.
//  2. the turn horizon was measured on the request as it ARRIVED, so a request already past the window
//     had no turns remaining and no benefit could ever repay — on 51 of iteration 024's 203 firings
//     (25%) the horizon was exactly zero, which refuses unconditionally at any premium.
//
// Every test below was checked by introducing the defect, not by reading the diff.

// premiumReq is a transcript large enough that a deep drop costs real money to rewrite, with a token
// count taken from schema.TextTokens rather than asserted, as the other econ fixtures do.
func premiumReq(n, each int) *bschemas.BifrostChatRequest {
	req := &bschemas.BifrostChatRequest{}
	for i := 0; i < n; i++ {
		body := strings.Repeat("premium line "+strconv.Itoa(i)+" of the transcript\n", each)
		if i%2 == 1 {
			req.Input = append(req.Input, assistantMsg(body))
			continue
		}
		req.Input = append(req.Input, toolResultMsg(body))
	}
	return req
}

func sweepPricing(premium float64) rewritePricing {
	return rewritePricing{approval: 1, premium: premium, creditRemoval: true}
}

// The premium divides `need`, and it does so on the BENEFIT side, which is where the claim belongs.
// Mutation that must fail this: drop `* premium` from eff in prefixRewritePaysWith.
func TestRewardPremiumDividesNeed(t *testing.T) {
	req := premiumReq(24, 400)
	c := econCtx(200_000)
	base, _, _ := prefixRewritePaysWith(req, 800, 2, sweepPricing(1), c)
	if base < 4 {
		t.Fatalf("fixture is too cheap to be a test of division: need=%d, want >= 4", base)
	}
	for _, m := range []float64{2, 4, 20} {
		got, _, _ := prefixRewritePaysWith(req, 800, 2, sweepPricing(m), c)
		// The arithmetic is ceil(onetime/(eff*m)), which is not in general ceil(base/m) once ceil has
		// been applied twice; assert the bound the claim actually makes.
		if want := int(math.Ceil(float64(base) / m)); got > want || got < 1 {
			t.Fatalf("premium %v: need=%d, want in [1,%d] (unpremiumed need=%d)", m, got, want, base)
		}
	}
}

// A batch that DECLINES at face value and CLEARS under a premium — the behaviour change the config key
// exists to buy, asserted end to end rather than inferred from the need value.
func TestRewardPremiumTurnsADeclineIntoAFiring(t *testing.T) {
	req := premiumReq(24, 400)
	c := econCtx(200_000)
	// A deep drop: the rewritten span reaches back to index 2, so the cache-write dominates.
	if _, _, ok := prefixRewritePaysWith(req, 800, 2, sweepPricing(1), c); ok {
		t.Skip("fixture already repays at face value; nothing to demonstrate")
	}
	if _, _, ok := prefixRewritePaysWith(req, 800, 2, sweepPricing(maxRewardPremium), c); !ok {
		need, have, _ := prefixRewritePaysWith(req, 800, 2, sweepPricing(maxRewardPremium), c)
		t.Fatalf("a premium of %v still declines (need=%d have=%d): the premium is not reaching the "+
			"break-even", maxRewardPremium, need, have)
	}
}

// The premium shortens repayment for the ADJUDICATION as well as the cache write, because `need` is a
// ratio and the premium scales its denominator. Intended, and pinned here so it is not "fixed" later by
// someone who reads the premium as a discount on the rewrite alone.
// Mutation that must fail this: apply the premium to the cache-write term only.
func TestRewardPremiumScalesTheAskTermToo(t *testing.T) {
	req := premiumReq(24, 400)
	c := econCtx(200_000)
	// The LAST message removed entirely: the rewritten span is that message alone, so `rewritten`
	// clamps to zero and the ASK is the only cost left in `onetime`.
	last := len(req.Input) - 1
	saved := schema.TextTokens(schema.MessageText(req.Input[last]))
	p := sweepPricing(1)
	p.askUSD = 5.00
	one, _, _ := prefixRewritePaysWith(req, saved, last, p, c)
	if one < 2 {
		t.Fatalf("fixture is wrong: a $5 ask against %d saved tokens must need more than one turn, got %d",
			saved, one)
	}
	p.premium = 10
	ten, _, _ := prefixRewritePaysWith(req, saved, last, p, c)
	if ten >= one {
		t.Fatalf("the premium did not reach the ask term: need=%d at premium 1, %d at premium 10", one, ten)
	}
}

// NO PREMIUM CAN AUTHORISE A ZERO HORIZON. ceil(anything positive) >= 1 > 0, so a request with no turns
// left refuses however valuable a removed token is believed to be. This is the limit that makes the
// premium honest, and the reason the horizon fix is not optional.
// Mutation that must fail this: `if premium > 1 { return 0, have, true }`.
func TestRewardPremiumCannotAuthoriseAZeroHorizon(t *testing.T) {
	req := premiumReq(24, 400)
	// A window barely above the request leaves no room: turnsRemainingAfter returns 0 even after the
	// credit, because the post-removal size is still within one turn's growth of the window.
	total := schema.MessagesTokens(req)
	c := econCtx(total + 1)
	for _, m := range []float64{1, 20, maxRewardPremium} {
		need, have, ok := prefixRewritePaysWith(req, 800, 2, sweepPricing(m), c)
		if have != 0 {
			t.Fatalf("fixture is wrong: premium %v left have=%d, want 0", m, have)
		}
		if ok {
			t.Fatalf("premium %v authorised a removal with no turns to repay it (need=%d)", m, need)
		}
	}
}

// A premium of 1 is the shipped arithmetic, bit for bit, on the sweep's own path — so a run configured
// without the key measures the same component as before it existed.
// Mutation that must fail this: multiply eff by 1.0001 when premium == 1.
func TestRewardPremiumOneChangesNothingButTheHorizon(t *testing.T) {
	req := premiumReq(24, 400)
	for _, tc := range []struct {
		name              string
		window            int
		saved, shallowest int
	}{
		{"deep drop", 200_000, 800, 2},
		{"late drop", 200_000, 800, 20},
		{"whole span", 200_000, 1 << 20, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := econCtx(tc.window)
			p := sweepPricing(1)
			p.creditRemoval = false // isolate the premium from the horizon change
			need, have, ok := prefixRewritePaysWith(req, tc.saved, tc.shallowest, p, c)
			wNeed, wHave, wOK := originalPrefixRewritePays(req, tc.saved, tc.shallowest, c)
			if need != wNeed || have != wHave || ok != wOK {
				t.Fatalf("premium 1 moved the break-even: got (need=%d have=%d ok=%v), original says "+
					"(need=%d have=%d ok=%v)", need, have, ok, wNeed, wHave, wOK)
			}
		})
	}
}

// ---- the horizon, #232 ----

// THE 25% CASE. A request at or past the window has no turns remaining, so the shipped arithmetic
// refuses every removal there — including the removal that would bring it back under.
// Mutation that must fail this: pass reqNow as reqAfter, i.e. stop crediting.
func TestHorizonCreditsItsOwnRemoval(t *testing.T) {
	req := premiumReq(24, 400)
	total := schema.MessagesTokens(req)
	// A window the request has already overrun: 70% of its own size.
	c := econCtx(total * 7 / 10)
	saved := total / 2 // removing half brings it well under

	uncredited := sweepPricing(1)
	uncredited.creditRemoval = false
	if _, have, _ := prefixRewritePaysWith(req, saved, 2, uncredited, c); have != 0 {
		t.Fatalf("fixture is wrong: the uncredited horizon must be 0 here, got %d", have)
	}
	_, have, _ := prefixRewritePaysWith(req, saved, 2, sweepPricing(1), c)
	if have <= 0 {
		t.Fatalf("crediting the removal left the horizon at %d: an over-window request can still never "+
			"repay anything, which is the defect", have)
	}
}

// The GROWTH RATE comes from the history that happened, the ROOM LEFT from the request as it will be.
// One value used to serve both, and collapsing them again is the natural way to "simplify" this.
// Mutation that must fail this: use reqAfter for perTurn in turnsRemainingAfter.
func TestHorizonRateComesFromHistoryNotTheRemoval(t *testing.T) {
	const window, turns = 100_000, 10
	// Same post-removal size, different histories: the one that grew faster has fewer turns left.
	slow := turnsRemainingAfter(20_000, 10_000, turns, window) // 2,000/turn
	fast := turnsRemainingAfter(60_000, 10_000, turns, window) // 6,000/turn
	if slow <= fast {
		t.Fatalf("the rate is being taken from the post-removal size: slow history gave %d turns, "+
			"fast history gave %d, want slow > fast", slow, fast)
	}
	if want := (window - 10_000) / 2_000; slow != want {
		t.Fatalf("slow history: got %d turns, want %d", slow, want)
	}
}

// Passing the same value for both reproduces the original expression, which is what keeps coref's
// arithmetic untouched. Asserted against a literal recomputation, not against the new code.
func TestTurnsRemainingAfterCollapsesToTheOriginal(t *testing.T) {
	for _, tc := range []struct{ req, turns, window int }{
		{20_000, 10, 100_000}, {99_999, 3, 100_000}, {100_000, 3, 100_000},
		{1, 1, 100_000}, {20_000, 0, 100_000}, {20_000, 10, 0},
	} {
		got := estimateTurnsRemaining(tc.req, tc.turns, tc.window)
		want := 0
		if tc.window > 0 && tc.turns > 0 && tc.req > 0 && tc.req < tc.window {
			if per := tc.req / tc.turns; per > 0 {
				want = (tc.window - tc.req) / per
			}
		}
		if got != want {
			t.Fatalf("estimateTurnsRemaining(%d,%d,%d) = %d, want %d",
				tc.req, tc.turns, tc.window, got, want)
		}
	}
}

// COREF IS UNTOUCHED BY THE HORIZON CHANGE, which is the whole reason creditRemoval is opt-in. If this
// fails, coref's drop selection is optimising a different objective than the measurements that
// calibrated it.
func TestCorefHorizonIsNotCredited(t *testing.T) {
	req := premiumReq(24, 400)
	total := schema.MessagesTokens(req)
	c := econCtx(total * 7 / 10) // over-window: the case where crediting would change the answer
	if _, have, ok := prefixRewritePays(req, total/2, 2, c); have != 0 || ok {
		t.Fatalf("coref's path started crediting the removal: have=%d ok=%v, want 0/false", have, ok)
	}
}

// The premium is a CONFIG-level belief, so the config has to refuse the two ways of getting it wrong.
func TestRewardPremiumConfigRefusesTheTwoMistakes(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		wantErr    bool
		want       float64
	}{
		{"unset means unadjusted", "min_tokens: 100\n", false, 1},
		{"one is explicit and allowed", "reward_premium: 1\n", false, 1},
		{"twenty", "reward_premium: 20\n", false, 20},
		{"below one is refused", "reward_premium: 0.5\n", true, 0},
		{"a typo is refused", "reward_premium: 2000\n", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmp, err := newExtractSweep([]byte(tc.yaml))
			if tc.wantErr {
				if err == nil {
					t.Fatal("accepted a reward_premium it must refuse")
				}
				return
			}
			if err != nil {
				t.Fatalf("refused a valid config: %v", err)
			}
			if got := cmp.(*ExtractSweep).rewardPremium; got != tc.want {
				t.Fatalf("rewardPremium = %v, want %v", got, tc.want)
			}
		})
	}
}

var _ = components.Ctx{}

// ---- the counterfactual behind the two decline labels ----

// THE LABEL HAS TO VARY THE ASK AND NOTHING ELSE. `askDeclined` is documented as "the same batch,
// priced with a free ask, would have been authorised", and the counterfactual used to be
// prefixRewritePays — which also resets the approval discount, the premium and the horizon credit. So
// it answered a broader question, and under a premium above 1 it answers it uselessly: the re-pricing
// happens at premium 1, nothing clears, and every decline lands on the REWRITE label regardless of
// which cost actually refused.
//
// The case below is engineered to separate the two implementations: at premium 20 a free ask clears,
// at premium 1 it does not. The old counterfactual therefore says "the rewrite refused" and the
// correct one says "the ask refused".
// Mutation that must fail this: replace the freeAsk pricing with prefixRewritePays.
func TestAskDeclinedLabelHoldsThePremium(t *testing.T) {
	const premium = 20.0
	req := premiumReq(24, 400)

	// THE FIXTURE IS SEARCHED FOR, NOT GUESSED. The case that separates the two implementations is
	// narrow — the rewrite must repay at premium 20 and NOT at premium 1, while the ask pushes the
	// total back over — and a hand-picked (saved, ask, window) landed outside it and t.Skip'd. A
	// skipped test reads as a pass, which is how this assertion would have gone vacuous; the mutation
	// run caught it. So the preconditions are evaluated with the REAL functions and a failure to find
	// any qualifying combination is a FAILURE, never a skip.
	type fixture struct {
		saved, shallowest, window int
		askUSD                    float64
	}
	var found *fixture
	var tried int
	for _, window := range []int{60_000, 80_000, 120_000, 200_000, 400_000} {
		for _, shallowest := range []int{2, 4, 8, 12} {
			for _, saved := range []int{400, 800, 1_600, 3_200, 6_400, 12_800} {
				for _, askUSD := range []float64{0.05, 0.25, 1.00, 5.00, 25.00} {
					tried++
					c := econCtx(window)
					full := sweepPricing(premium)
					full.askUSD = askUSD
					if _, _, ok := prefixRewritePaysWith(req, saved, shallowest, full, c); ok {
						continue // the ask is not decisive here
					}
					free := full
					free.askUSD = 0
					if _, _, ok := prefixRewritePaysWith(req, saved, shallowest, free, c); !ok {
						continue // the rewrite refuses on its own: not an ask decline
					}
					// The OLD counterfactual must disagree, or the fixture cannot tell them apart.
					if _, _, ok := prefixRewritePays(req, saved, shallowest, c); ok {
						continue
					}
					found = &fixture{saved, shallowest, window, askUSD}
				}
				if found != nil {
					break
				}
			}
			if found != nil {
				break
			}
		}
		if found != nil {
			break
		}
	}
	if found == nil {
		t.Fatalf("no fixture in %d combinations separates the two counterfactuals; the search space "+
			"needs widening rather than the assertion weakening", tried)
	}
	t.Logf("fixture: saved=%d shallowest=%d window=%d askUSD=%v",
		found.saved, found.shallowest, found.window, found.askUSD)

	c := econCtx(found.window)
	e := &ExtractSweep{econTrigger: true, rewardPremium: premium, asks: &askLedger{}}
	d := econDecision{offered: found.saved, approval: 1, askUSD: found.askUSD}
	e.priceBatch(req, found.saved, found.shallowest, &d, c) // the SAME function econPays calls
	if d.ok {
		t.Fatalf("the component cleared where the arithmetic declines: need=%d have=%d", d.need, d.have)
	}
	if !d.askDeclined {
		t.Fatalf("decline blamed on the rewrite (need=%d have=%d premium=%v) when a free ask at the "+
			"SAME premium would have cleared: the counterfactual is resetting terms it must hold",
			d.need, d.have, premium)
	}
}
