package offload

import (
	"strings"
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
	return components.RatesOf(p)
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
