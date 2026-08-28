package components

import "testing"

// A NAME MUST NOT BE BOTH A DECLINE AND AN EVENT for one component.
//
// The two maps answer opposite questions — did this component turn a candidate away, or did it do
// something — and they are exported as two Prometheus series. A name in both means the component
// cannot say which happened, and a consumer summing either series double-counts it.
//
// This is the invariant that replaces the old arrangement, where everything landed in Gates and was
// exported under `cg_component_gate_declines_total`. A cache hit (`reapplied_same_session`) and a
// removal that worked (`sweep_dropped`) were counted as declines there, so the series ROSE as a
// component worked better and anyone reading it to judge pipeline effectiveness got the wrong sign.
func TestGatesAndEventsAreDisjoint(t *testing.T) {
	var r Report
	r.Gate("below_output_floor")
	r.GateN("cached_prefix", 3)
	r.Event("sweep_dropped")
	r.EventN("sweep_offered", 12)

	if len(r.Gates) != 2 || len(r.Events) != 2 {
		t.Fatalf("gates=%v events=%v: both maps must fill independently", r.Gates, r.Events)
	}
	for name := range r.Gates {
		if _, both := r.Events[name]; both {
			t.Errorf("%q is recorded as BOTH a decline and an event; a consumer summing either "+
				"series counts it twice, and the component cannot say which happened", name)
		}
	}
	if r.Gates["cached_prefix"] != 3 || r.Events["sweep_offered"] != 12 {
		t.Errorf("the N variants must add rather than overwrite: gates=%v events=%v", r.Gates, r.Events)
	}
}

// Event must be nil-safe and must not count a non-positive N, matching Gate/GateN exactly. A
// component holding a nil Report is the ordinary case for an emitter that was not wired.
func TestEventMatchesGateOnEdgeCases(t *testing.T) {
	var nilRep *Report
	nilRep.Event("x")     // must not panic
	nilRep.EventN("y", 1) // must not panic

	var r Report
	r.EventN("zero", 0)
	r.EventN("negative", -5)
	if len(r.Events) != 0 {
		t.Errorf("a non-positive N must record nothing, got %v", r.Events)
	}
}
