package dash

import "github.com/rossoctl/accountant"

// The accountant Result of the request, set by the proxy when a dashboard is on. It is unexported,
// so neither JSON nor the SQLite writer sees it.

// SetAccountant attaches the library's Result to the event.
func (e *Event) SetAccountant(r accountant.Result) { e.cfRes = &r }

// HasAccountant reports whether a Result is attached.
func (e *Event) HasAccountant() bool { return e.cfRes != nil }

// AccountantResult returns a copy of the attached Result and whether one is attached. It is how a
// test or a host reads what the store does not keep.
func (e *Event) AccountantResult() (accountant.Result, bool) {
	if e.cfRes == nil {
		return accountant.Result{}, false
	}
	return *e.cfRes, true
}

// SetAccountantContext records what qualifies the attached Result: how many of the request's
// changes the trace could not place as operations, and whether the run was observe mode. Both
// gate the accountant override in PriceMode.
func (e *Event) SetAccountantContext(opsDropped int, observe bool) {
	e.cfOpsDropped, e.cfObserve = opsDropped, observe
}
