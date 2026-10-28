package dash

import cf "github.com/rossoctl/context-guru/counterfactual"

// The counterfactual Result of the request, set by the proxy when it runs with --savings=shadow
// or counterfactual. It is unexported, so neither JSON nor the SQLite writer sees it.

// SetCounterfactual attaches the library's Result to the event.
func (e *Event) SetCounterfactual(r cf.Result) { e.cfRes = &r }

// HasCounterfactual reports whether a Result is attached.
func (e *Event) HasCounterfactual() bool { return e.cfRes != nil }

// Counterfactual returns a copy of the attached Result and whether one is attached. It is how a
// test or a host reads what the store does not keep.
func (e *Event) Counterfactual() (cf.Result, bool) {
	if e.cfRes == nil {
		return cf.Result{}, false
	}
	return *e.cfRes, true
}

// SetCounterfactualContext records what qualifies the attached Result: how many of the request's
// changes the trace could not place as operations, and whether the run was observe mode. Both
// gate the counterfactual override in PriceMode.
func (e *Event) SetCounterfactualContext(opsDropped int, observe bool) {
	e.cfOpsDropped, e.cfObserve = opsDropped, observe
}
