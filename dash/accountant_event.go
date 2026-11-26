package dash

import "github.com/rossoctl/accountant"

// The accountant Result of the request, set by the proxy when a dashboard is on. It is unexported,
// so neither JSON nor the SQLite writer sees it.

// SetAccountant attaches the library's Result to the event.
func (e *Event) SetAccountant(r accountant.Result) { e.accRes = &r }

// HasAccountant reports whether a Result is attached.
func (e *Event) HasAccountant() bool { return e.accRes != nil }

// AccountantResult returns a copy of the attached Result and whether one is attached. It is how a
// test or a host reads what the store does not keep.
func (e *Event) AccountantResult() (accountant.Result, bool) {
	if e.accRes == nil {
		return accountant.Result{}, false
	}
	return *e.accRes, true
}

// SetAccountantServerHeldHistory marks an OpenAI Responses request with server-held history (previous_response_id, conversation or item_reference): the
// provider holds the earlier turns, so the body is only this turn and the proxy cannot see the prefix the
// provider reads. PriceMode then books no saving and says why (fallback server_held_history), where it would
// have said no_result, which reads as a request that was merely skipped (context-guru#899).
func (e *Event) SetAccountantServerHeldHistory() { e.accServerHeldHistory = true }

// SetAccountantContext records what qualifies the attached Result: how many of the request's
// changes the trace could not place as operations, and whether the run was observe mode. Both
// gate the accountant override in PriceMode.
func (e *Event) SetAccountantContext(opsDropped int, observe bool) {
	e.accOpsDropped, e.accObserve = opsDropped, observe
}
