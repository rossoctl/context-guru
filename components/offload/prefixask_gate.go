package offload

import "github.com/rossoctl/context-guru/components"

// The write cap for a prefix-ask answer. Used only when prefixAskWriteCapOn is set.
const (
	// prefixAskWriteCapFrac is the largest write, as a share of the prefix the question read and
	// wrote, that still counts as the turn's own unreadable tail.
	prefixAskWriteCapFrac = 0.02
	// prefixAskWriteCapMax is the hard ceiling on that write, in tokens.
	prefixAskWriteCapMax = 2000
)

// prefixAskWriteCapOn switches the optional write cap. Off until the owner decides the rule (#721).
var prefixAskWriteCapOn = false

// The reasons prefixAskCacheOK gives. Each maps to one gate in the sweep.
const (
	prefixAskReadZero      = "read_zero"
	prefixAskReadBelowTurn = "read_below_last_turn"
	prefixAskWriteOverCap  = "write_over_cap"
)

// prefixAskCacheOK is the ONE decision on whether a prefix-ask answer used the cache. Every gate
// that judges such an answer calls it, so the rule can change in one place (#721).
//
// The rule: the provider never reads a cache entry that ends at a text turn, so a correct question
// always rewrites some tail, and the write alone cannot tell a good question from a bad one. The
// READ can. A question that read less than the last real turn on the thread lost a prefix that turn
// could still read (a model switch, a changed prefix).
//
//   - A read of 0 is always refused.
//   - lastTurnRead > 0: refuse a read below it.
//   - lastTurnRead == 0 means the caller does not know it; only the read-of-0 rule applies.
//   - With prefixAskWriteCapOn: also refuse a write above min(frac of the prefix, max).
func prefixAskCacheOK(u components.PrefixUsage, lastTurnRead int) (ok bool, reason string) {
	if u.CacheRead == 0 {
		return false, prefixAskReadZero
	}
	if lastTurnRead > 0 && u.CacheRead < lastTurnRead {
		return false, prefixAskReadBelowTurn
	}
	if prefixAskWriteCapOn {
		limit := int(float64(u.CacheRead+u.CacheWrite) * prefixAskWriteCapFrac)
		if limit > prefixAskWriteCapMax {
			limit = prefixAskWriteCapMax
		}
		if u.CacheWrite > limit {
			return false, prefixAskWriteOverCap
		}
	}
	return true, ""
}
