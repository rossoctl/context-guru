package dash

import cf "github.com/rossoctl/context-guru/counterfactual"

// The counterfactual Result of the request, set by the proxy when it runs with --savings=shadow
// or counterfactual. It is unexported, so neither JSON nor the SQLite writer sees it.

// SetCounterfactual attaches the library's Result to the event.
func (e *Event) SetCounterfactual(r cf.Result) { e.cfRes = &r }

// HasCounterfactual reports whether a Result is attached.
func (e *Event) HasCounterfactual() bool { return e.cfRes != nil }
