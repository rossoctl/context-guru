package accrec

import "strconv"

// The reasons a request books no accountant figure. The log line acc_fallback names one of them.
const (
	// DeferralUnplaced: a deferral whose stub cannot be placed (a stub that would not be smaller than its
	// definition). Computing on the bodies as they are would fail the sum check, so no figure is computed.
	DeferralUnplaced = "deferral_unplaced"
	// RejectedStatus: the provider answered with a status that is not 2xx.
	RejectedStatus = "rejected_status"
	// ComputePanicked: the library panicked while it priced the request.
	ComputePanicked = "compute_panicked"
)

// Rejected is true when the provider answered with a status that is not 2xx. Status 0 means no upstream
// answered (such as /compact), and is not a rejection.
func Rejected(status int) bool { return status != 0 && (status < 200 || status > 299) }

// ShouldCompute decides whether the accountant prices a request that has a record and a usage. A rejected request
// (a status that is not 0 and not 2xx) books no saving, in tokens or in dollars: the body of a 4xx or 5xx can still
// carry a usage object, but the provider billed none of the request. A deferral that could not be placed books no
// figure either. Neither does a request that took several upstream rounds: its usage is not one request's. The
// thread state is left as the previous request left it; the next request then sees a longer gap, which the library
// handles (a phase it cannot prove is unknown and falls back too).
//
// noResult is the reason an earlier step already set. When compute is false, reason is the noResult to record: the
// given one, or RejectedStatus (which replaces it).
func ShouldCompute(status, rounds int, noResult string) (compute bool, reason string) {
	switch {
	case Rejected(status):
		return false, RejectedStatus
	case noResult == DeferralUnplaced:
		return false, noResult
	case rounds <= 1:
		return true, noResult
	}
	return false, noResult
}

// NoResultWhy names the branch that left an event with no accountant Result, for the acc_fallback line. The reason
// no_result covers four unrelated things, and the log line used to say only that. hasRecord is false when the
// pipeline did not run: no session, or no record was opened.
func NoResultWhy(hasRecord, usageOK bool, noResult string, rounds int) string {
	switch {
	case !hasRecord:
		return "no_record"
	case !usageOK:
		return "no_usage"
	case noResult != "":
		return noResult
	case rounds > 1:
		return "upstream_rounds=" + strconv.Itoa(rounds)
	}
	return ""
}
