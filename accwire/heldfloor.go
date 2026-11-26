package accwire

import "github.com/rossoctl/accountant"

// HeldFloorUSD is the lower bound of a held figure (policy of context-guru, not of the accountant).
//
// The held ceiling prices each removed token as the write the provider would have made on a cold
// cache. If the cache was warm, the client would have paid the read rate for the tokens in the warm
// prefix, so the saving is lower. The read rate is the least a client token costs. The floor prices
// the same removed tokens at the read rate: it takes the ceiling and subtracts, for each operation,
// the Saved dollars minus the same tokens priced as reads (ByComponentSaved, ByComponentSavedAsRead).
// Caused tokens and the cache effects stay as they are.
//
// ceiling is the figure that the floor lowers. skip is a list of keys of fig (the operator as the
// host gave it) whose tokens are not in that ceiling. The floor is never above the ceiling: with a
// price table whose read is dearer than its write, it equals the ceiling.
func HeldFloorUSD(fig accountant.USDFigure, ceiling float64, skip ...string) float64 {
	low := ceiling
	for op, saved := range fig.ByComponentSaved {
		if contains(skip, op) {
			continue
		}
		low -= saved - fig.ByComponentSavedAsRead[op]
	}
	if low > ceiling {
		return ceiling
	}
	return low
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
