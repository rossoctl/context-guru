package offload

import "github.com/rossoctl/context-guru/components"

// SWEEP V3 TIER TARGET (#1329). Some prices bill a whole request at higher rates above a prompt size
// (OpenAI GPT-5.4 and later above 272k, Haiku 5.5 above 100k). Below the sweet spot the sweep does nothing,
// so a request just above such a line pays the high rates for many turns. With tier_target on, a request
// above a line may be cut to just below it, when the cut pays back before the line is crossed again.
//
// The lines and rates come from the price table (components.TokenRates.Tiers, from modelinfo.Price.At).
// No line is a constant here. A flat price has no line, so nothing changes for it.
//
// THE RULE. Let L be the highest line below P, G = L - margin the goal, D the drop the recheck, rules and
// judge give together (billed tokens). The cut is allowed only when:
//
//  1. P - D <= G                          the cut reaches the goal. A smaller cut is refused (tier_line_not_reached).
//  2. saving > rewrite                    the warm gate's test, with tier rates (see tierEcon).
//
// A request in the sweet spot keeps today's gate. The tier target only widens WHEN the sweep may cut. It never
// changes what is stashed or how: the marker and the stash are the same.
//
// THE MARGIN is how far below the line the goal sits, so the next turns do not cross back at once:
//
//	margin = clamp(tierGrowthTurns x growth per turn, 4% of L, 10% of L)
//
// 4% of 272k is 10.9k (about 2 to 5 turns of a typical agent). The growth term asks for 8 turns of room when
// the session's growth is known. The 10% cap (27k at 272k) keeps the cut from being deeper than it must be.
// The saving also counts only the turns before the prompt grows back to L (see tierEcon), so a thin margin
// makes the cut harder to accept and not just a bad one.
const (
	tierMarginFloorFrac = 0.04
	tierMarginCapFrac   = 0.10
	tierGrowthTurns     = 8.0
)

// tierPlan is the goal of a tier cut.
type tierPlan struct {
	line   int64   // the tier line P is above
	goal   int     // the billed prompt the cut must reach
	growth float64 // billed tokens added per turn, 0 = unknown
}

// tierMargin is the room below the line (see the note above).
func tierMargin(line int64, growth float64) int {
	lo, hi := tierMarginFloorFrac*float64(line), tierMarginCapFrac*float64(line)
	m := tierGrowthTurns * growth
	if m < lo {
		m = lo
	}
	if m > hi {
		m = hi
	}
	return int(m)
}

// tierGoal returns the plan for a request of billed prompt P, or nil when the target does not apply: it is
// off, the price is flat, the price comes from a diagnostic model, or P is at or below every line.
func (e *ExtractSweep) tierGoal(c *components.Ctx, P, growthFrom, growthTurns int) *tierPlan {
	if !e.tierTarget || c == nil || e.warmGatePriceModel != "" {
		return nil
	}
	line, ok := c.SelfRates.LineBelow(int64(P))
	if !ok {
		return nil
	}
	growth := 0.0
	if growthTurns > 0 && P > growthFrom {
		growth = float64(P-growthFrom) / float64(growthTurns)
	}
	goal := int(line) - tierMargin(line, growth)
	if goal <= 0 || goal >= P {
		return nil
	}
	return &tierPlan{line: line, goal: goal, growth: growth}
}

// tierEcon is the gate's arithmetic for a tier cut. The saving is the drop in the per-turn read cost of the
// whole prefix: before at the rates of P, after at the rates of P - D. It counts the turns until the prompt
// grows back to the line, at most. The rewrite is the cache write of the rewritten tail at the rates after
// the cut, net of the read it replaces. Output is not counted: it only makes the saving larger.
// It returns the turns it used.
func (e *ExtractSweep) tierEcon(c *components.Ctx, t *tierPlan, in warmEconIn, P int) (saving, rewrite, turns float64) {
	after := P - int(float64(in.Removed)*in.RatioContent)
	_, readA, _ := agentRates(withRates(c, c.SelfRates.At(int64(P))))
	_, readB, writeB := agentRates(withRates(c, c.SelfRates.At(int64(after))))
	turns = in.Turns
	if t.growth > 0 {
		if hold := float64(int(t.line)-after) / t.growth; hold < turns {
			turns = hold
		}
	}
	if turns < 0 {
		turns = 0
	}
	saving = (float64(P)*readA - float64(after)*readB) * turns
	rewrite = float64(in.Rewritten) * in.RatioFull * (writeB - readB)
	return saving, rewrite, turns
}

func withRates(c *components.Ctx, r components.TokenRates) *components.Ctx {
	cc := *c
	cc.SelfRates = r
	return &cc
}
