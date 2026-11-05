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

// SetAccountantUnsupportedWire marks the request as one on a wire the accountant library does not read yet:
// the OpenAI Responses API keeps its conversation in `input`, and the proxy hashes `messages` and reports no
// operations there. PriceMode then books no saving and says why (fallback unsupported_wire), where it would
// have said no_result, which reads as a request that was merely skipped (context-guru#768 B7).
func (e *Event) SetAccountantUnsupportedWire() { e.accUnsupportedWire = true }

// SetAccountantContext records what qualifies the attached Result: how many of the request's
// changes the trace could not place as operations, and whether the run was observe mode. Both
// gate the accountant override in PriceMode.
func (e *Event) SetAccountantContext(opsDropped int, observe bool) {
	e.accOpsDropped, e.accObserve = opsDropped, observe
}
