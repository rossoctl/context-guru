package keepalive

import "math"

// The ping value rule (#1420).
//
// A flat dollar cap per ping is blind to value. A GPT-5.5 ping at 600K tokens costs about $0.60 and
// saves about $5, but a $0.50 cap refuses it. So the guard compares two figures of the same entry:
//
//   - the cost of the whole planned chain: K pings, each at its projected cost;
//   - the miss cost: what the next request pays extra when the cache is gone. That is the prefix
//     at the write rate of the entry (5 minutes, or 1 hour on an entry billed at the 1-hour tier)
//     minus the read it would have had. On OpenAI the write rate is the input rate.
//
// A ping is allowed when the chain costs less than one miss. It is per SESSION and not per ping:
// the K pings of one idle span protect ONE return, so they are weighed against ONE miss. K is the
// planned count at record time (Policy.MaxPings after the wait plan), and 1 when none is planned.
//
// Both figures are priced at the tier of the ping's prompt size (Price.At), so a long prompt that
// bills at 2x raises both sides together. An unpriced model gives 0 for both and is allowed: an
// incomplete price list must not switch off a protection.
//
// The operator's flat cap (Policy.MaxUSDPerPing > 0) is OPT-IN and applies on top of this rule.
// Replay (campaign) calls the same function, so the live keeper and the replay decide alike.

// Verdict is the answer of PingVerdict.
type Verdict int

const (
	// VerdictOK means the ping is allowed.
	VerdictOK Verdict = iota
	// VerdictOverCap means the operator's own flat cap refuses it.
	VerdictOverCap
	// VerdictNotWorth means the planned chain costs at least the miss it protects.
	VerdictNotWorth
)

// Reason is the PingState reason for a refusal. Empty for VerdictOK.
func (v Verdict) Reason() string {
	switch v {
	case VerdictOverCap:
		return ReasonOverBudget
	case VerdictNotWorth:
		return ReasonNotWorth
	}
	return ""
}

// PingCap is the operator's flat cap, or +Inf when the operator set none. Unlike Ceiling it does not
// map zero to a default: zero means "not set", and a negative value means "no cap" as before.
func (p Policy) PingCap() float64 {
	if p.MaxUSDPerPing > 0 {
		return p.MaxUSDPerPing
	}
	return math.Inf(1)
}

// MissUSD is the extra cost of one cache miss on a prefix: the write rate minus the read rate, on
// every prefix token. oneHour picks the 1-hour write rate. Zero when the price is unknown.
func MissUSD(price Price, model string, prefix int64, oneHour bool) float64 {
	if prefix <= 0 || price.Zero() {
		return 0
	}
	write := price.CacheWrite5mFor(model)
	if oneHour {
		write = price.CacheWrite1hFor(model)
	}
	return max(0, float64(prefix)*(write-price.CacheReadFor(model)))
}

// PingVerdict decides one ping chain. pingUSD is the projected cost of one ping, missUSD the miss
// cost (MissUSD), pings the planned count K (below 1 counts as 1).
//
// A zero pingUSD (unpriced) is always allowed. A zero missUSD is also allowed: it means the miss
// cost is unknown (no price) or degenerate (the write rate is not above the read rate, so a miss
// costs nothing extra to the price list), and the guard fails open for both, as it does for an
// unpriced model. The operator's flat cap still applies.
func (p Policy) PingVerdict(pingUSD, missUSD float64, pings int) Verdict {
	if pingUSD <= 0 {
		return VerdictOK
	}
	if pingUSD > p.PingCap() {
		return VerdictOverCap
	}
	if missUSD <= 0 {
		return VerdictOK
	}
	if float64(max(pings, 1))*pingUSD >= missUSD {
		return VerdictNotWorth
	}
	return VerdictOK
}
