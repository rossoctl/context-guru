package offload

import (
	"strings"
	"sync/atomic"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// Tests for the sweep's price-tier target (#1329).

const tierOn = "tier_target: true\n"

func tierRates(model string, p modelinfo.Price, flat bool) components.TokenRates {
	if !flat {
		p = p.WithTiers(model)
	}
	// The same conversion as lifecycle.RatesOf (offload cannot import lifecycle): each tier through Price.At.
	out := components.TokenRates{Input: p.Input, Output: p.Output, CacheRead: p.CacheRead, CacheWrite: p.CacheWrite}
	n := 0
	for _, tr := range p.Tiers {
		if tr.AbovePromptTokens > 0 {
			q := p.At(tr.AbovePromptTokens + 1)
			out.Tiers[n] = components.RateTier{AbovePromptTokens: tr.AbovePromptTokens, Input: q.Input, Output: q.Output, CacheRead: q.CacheRead, CacheWrite: q.CacheWrite}
			n++
		}
	}
	return out
}

func gptRates(flat bool) components.TokenRates {
	return tierRates("gpt-5.6", modelinfo.Price{Input: 5e-6, Output: 30e-6, CacheRead: 0.5e-6, CacheWrite: 6.25e-6}, flat)
}

func haikuRates() components.TokenRates {
	return tierRates("claude-haiku-5-5", modelinfo.Price{Input: 1e-6, Output: 5e-6, CacheRead: 0.1e-6, CacheWrite: 1.25e-6}, false)
}

// gptRig is a gpt-5.6 request on a 1M window: the sweet spot is 600k, so 300k is below it and above 272k.
// The outputs are twice the size of the default ones, so the two rule drops are a large share of the prompt.
func gptRig(t *testing.T, session, cfg string, flat bool) *v3Rig {
	r := newV3Rig(t, session, cfg)
	r.c, r.window = 1_000_000, 1_000_000
	r.rates, r.model = gptRates(flat), "gpt-5.6"
	for i := range r.calls {
		r.calls[i].out = strings.Repeat("candidate "+r.calls[i].id+" distinct line of output\n", 5400)
	}
	return r
}

// tierRun sends the warm requests of a thread at billed prompt p. It returns the request that was cut, if any,
// and the number of cut events.
func tierRun(r *v3Rig, p int) (*bschemas.BifrostChatRequest, int) {
	r.request(0)
	var last *bschemas.BifrostChatRequest
	for i := 0; i < 3; i++ {
		req, rep := r.request(p)
		last = req
		if n := rep.Events[eventAppliedWarm]; n > 0 {
			return req, n
		}
	}
	return last, 0
}

func TestTierRatesCarryTheLines(t *testing.T) {
	g := gptRates(false)
	if line, ok := g.LineBelow(300_000); !ok || line != 272_000 {
		t.Fatalf("line = %d %v", line, ok)
	}
	if _, ok := g.LineBelow(272_000); ok {
		t.Fatal("at the line is not above it")
	}
	if g.At(300_000).CacheRead != 2*g.CacheRead || g.At(250_000).CacheRead != g.CacheRead {
		t.Fatalf("At: %+v", g.At(300_000))
	}
	if _, ok := gptRates(true).LineBelow(900_000); ok {
		t.Fatal("a flat price has no line")
	}
}

func TestTierMarginFollowsGrowthAndIsClamped(t *testing.T) {
	if m := tierMargin(272_000, 0); m != 10_880 {
		t.Fatalf("floor = %d", m)
	}
	if m := tierMargin(272_000, 2_000); m != 16_000 {
		t.Fatalf("growth = %d", m)
	}
	if m := tierMargin(272_000, 50_000); m != 27_200 {
		t.Fatalf("cap = %d", m)
	}
}

// 300k on gpt-5.6 with enough expected turns: the cut goes below 272k.
func TestTierCutsBelowTheLineWhenItPays(t *testing.T) {
	r := gptRig(t, "tier-gpt", v3Rule+tierOn, false)
	req, n := tierRun(r, 300_000)
	if n == 0 {
		d, _ := lastWarmDecision()
		t.Fatalf("no cut; refused %v, last %+v", v3Counters().Refused, d)
	}
	if len(dropped(req)) == 0 {
		t.Fatal("nothing dropped")
	}
	v := v3Counters()
	if len(v.Recent) != 1 || 300_000-v.Recent[0].D > 272_000-tierMargin(272_000, 0) {
		t.Fatalf("the cut does not reach below the line: %+v", v.Recent)
	}
}

// Too few expected turns: today's target stands (no cut).
func TestTierKeepsTodaysTargetWhenTheCutDoesNotPay(t *testing.T) {
	r := gptRig(t, "tier-few", strings.Replace(v3Rule, "warm_expected_turns: 200", "warm_expected_turns: 5", 1)+tierOn, false)
	req, n := tierRun(r, 300_000)
	if n != 0 || len(dropped(req)) != 0 || r.refused("does_not_pay") == 0 {
		t.Fatalf("events %d refused %v", n, v3Counters().Refused)
	}
}

// A cut that does not reach the goal is refused, even when it pays by the old gate.
func TestTierRefusesACutThatStaysAboveTheLine(t *testing.T) {
	r := gptRig(t, "tier-short", v3Rule+tierOn, false)
	req, n := tierRun(r, 450_000) // the rules remove about 95k: 450k stays above the line
	if n != 0 || len(dropped(req)) != 0 || r.refused("tier_line_not_reached") == 0 {
		t.Fatalf("events %d refused %v", n, v3Counters().Refused)
	}
}

// Haiku 5.5 at 110k: the cut goes below 100k.
func TestTierHaikuCutsBelowItsLine(t *testing.T) {
	r := newV3Rig(t, "tier-haiku", v3Rule+tierOn)
	r.c, r.window = 200_000, 200_000
	r.rates, r.model = haikuRates(), "claude-haiku-5-5"
	for i := range r.calls {
		r.calls[i].out = strings.Repeat("candidate "+r.calls[i].id+" distinct line of output\n", 1200)
	}
	req, n := tierRun(r, 110_000)
	if n == 0 {
		t.Fatalf("no cut; refused %v", v3Counters().Refused)
	}
	v := v3Counters()
	if len(v.Recent) != 1 || 110_000-v.Recent[0].D > 100_000-tierMargin(100_000, 0) || len(dropped(req)) == 0 {
		t.Fatalf("cut %+v", v.Recent)
	}
}

// No tiers (flat) or the switch off: unchanged, a request below the sweet spot is not cut.
func TestTierFlatPriceAndSwitchOffChangeNothing(t *testing.T) {
	for name, mk := range map[string]func(*testing.T) *v3Rig{
		"flat":       func(t *testing.T) *v3Rig { return gptRig(t, "tier-flat", v3Rule+tierOn, true) },
		"switch-off": func(t *testing.T) *v3Rig { return gptRig(t, "tier-off", v3Rule, false) },
		"explicit":   func(t *testing.T) *v3Rig { return gptRig(t, "tier-default", v3Rule+"tier_target: false\n", false) },
	} {
		t.Run(name, func(t *testing.T) {
			r := mk(t)
			req, n := tierRun(r, 300_000)
			if n != 0 || len(dropped(req)) != 0 || r.refused("not_in_sweet_spot") == 0 {
				t.Fatalf("events %d refused %v", n, v3Counters().Refused)
			}
		})
	}
}

// A cut that ends below the line but inside the margin is refused: the next turns would cross back at once.
// D is 97,154 here, so 360k ends at 262,846: below 272k, inside the 10,880 margin.
func TestTierRefusesACutThatEndsInsideTheMargin(t *testing.T) {
	r := gptRig(t, "tier-margin", v3Rule+tierOn, false)
	req, n := tierRun(r, 360000)
	if n != 0 || len(dropped(req)) != 0 || r.refused("tier_line_not_reached") == 0 {
		t.Fatalf("events %d refused %v", n, v3Counters().Refused)
	}
}

const tierJudge = v3Base + "selector: judge\nprotect_last_n: 2\n" + tierOn

// The judge selector: above the line, below the sweet spot. The question is asked, the answer is applied, and
// the cut reaches the goal.
func TestTierJudgeSelectorAsksThenCutsBelowTheGoal(t *testing.T) {
	r := gptRig(t, "tier-judge", tierJudge, false)
	asker := pingAsker()
	r.asker = asker
	r.request(0)
	for i := 0; i < 2; i++ {
		r.request(300_000)
	}
	r.forward()
	if atomic.LoadInt64(&asker.calls) == 0 {
		t.Fatalf("the judge was not asked; refused %v", v3Counters().Refused)
	}
	req, rep := r.request(300_000)
	if rep.Events[eventAppliedWarm] == 0 || len(dropped(req)) == 0 {
		d, _ := lastWarmDecision()
		t.Fatalf("the answer was not applied: %v refused %v last %+v", rep.Events, v3Counters().Refused, d)
	}
	v := v3Counters()
	if len(v.Recent) != 1 || 300_000-v.Recent[0].D > 272_000-tierMargin(272_000, 0) || v.JudgeDrops == 0 {
		t.Fatalf("cut %+v", v)
	}
}

// A held answer is not applied when, at apply time, the prompt has fallen below every line.
func TestTierHeldAnswerIsNotAppliedBelowEveryLine(t *testing.T) {
	r := gptRig(t, "tier-held", tierJudge, false)
	r.asker = pingAsker()
	r.request(0)
	for i := 0; i < 2; i++ {
		r.request(300_000)
	}
	r.forward()
	req, rep := r.request(250_000)
	if rep.Events[eventAppliedWarm] != 0 || len(dropped(req)) != 0 {
		t.Fatalf("events %v refused %v", rep.Events, v3Counters().Refused)
	}
}

// With tier_target on and v3 off (warm only), nothing changes: tier_target does not switch v3 on.
func TestTierTargetWithoutV3DoesNothing(t *testing.T) {
	e := shippedSweep(t, "warm: true\n"+tierOn)
	if e.v3 {
		t.Fatal("tier_target switched v3 on")
	}
	r := gptRig(t, "tier-nov3", "warm: true\nwarm_every_n: 3\nwarm_min_age_turns: 5\nwarm_max_turns: 200\nwarm_expected_turns: 200\n"+tierOn, false)
	req, n := tierRun(r, 300_000)
	if n != 0 || len(dropped(req)) != 0 {
		t.Fatalf("events %d", n)
	}
}

// The diagnostic warm_gate_price_model prices the gate with another card: the tier target stays off.
func TestTierTargetIsOffWithAGatePriceModel(t *testing.T) {
	r := gptRig(t, "tier-gpm", v3Rule+tierOn+"warm_gate_price_model: gpt-5.6\n", false)
	req, n := tierRun(r, 300_000)
	if n != 0 || len(dropped(req)) != 0 || r.refused("not_in_sweet_spot") == 0 {
		t.Fatalf("events %d refused %v", n, v3Counters().Refused)
	}
}

// After a tier cut the growth wait (min_drop_frac x C) does not hold the thread: the tier gate decides.
func TestTierCutIsNotHeldByTheGrowthWait(t *testing.T) {
	r := gptRig(t, "tier-wait", v3Rule+tierOn, false)
	if _, n := tierRun(r, 300_000); n == 0 {
		t.Fatalf("fixture: no first cut; refused %v", v3Counters().Refused)
	}
	for i := 0; i < 3; i++ {
		r.request(300_000)
	}
	if r.refused("waiting_for_growth") != 0 {
		t.Fatalf("the growth wait held a tier cut: %v", v3Counters().Refused)
	}
}

// The question's own cost is priced at the tier rates of its prompt size (gpt-5.6, 300k prompt: 2x).
func TestTierCardPricesACallAtItsPromptSize(t *testing.T) {
	g := gptRates(false)
	above := g.Cost(0, 1000, 0, 300_000)
	want := 300_000*1e-6 + 1000*45e-6 // read 2x of 0.5e-6, output 1.5x of 30e-6
	if d := above - want; d > 1e-9 || d < -1e-9 {
		t.Fatalf("cost = %v, want %v", above, want)
	}
	if below, w := g.Cost(0, 1000, 0, 250_000), 250_000*0.5e-6+1000*30e-6; below-w > 1e-9 || w-below > 1e-9 {
		t.Fatalf("below the line: %v, want %v", below, w)
	}
	if got, want := ratesPricing(g.At(300_000)).Cost(0, 1000, 0, 300_000), above; got-want > 1e-9 || want-got > 1e-9 {
		t.Fatalf("sweep leg pricing %v differs from the card %v", got, want)
	}
}
