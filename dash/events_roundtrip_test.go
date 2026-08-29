package dash

import (
	"testing"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
)

// FromTrace MUST CARRY EVENTS ACROSS, which is the half of #124 the store round-trip below cannot
// see.
//
// Written after the round-trip test passed with `row.Events = r.Events` removed — it builds CompRow
// values directly, so it never exercises the Report-to-row mapping where the drop actually happened.
// That is the sixth vacuous check this component family has produced, so the lesson is worth the
// comment: a test that starts downstream of the defect verifies the plumbing it kept, not the seam
// that broke.
func TestFromTraceCarriesComponentEvents(t *testing.T) {
	var rep components.Report
	rep.Component, rep.Kind = "extract_llm_sweep", "offload"
	rep.TokensBefore, rep.TokensAfter = 34317, 977
	rep.GateN("below_output_floor", 11)
	rep.EventN("sweep_offered", 12)
	rep.Event("sweep_prefix_cache_read_ok")

	var e Event
	e.FromTrace(apply.Trace{Run: &components.RunReport{
		Components: []components.Report{rep},
	}}, nil)

	if len(e.Components) != 1 {
		t.Fatalf("expected one component row, got %d — the assertion below would be vacuous",
			len(e.Components))
	}
	c := e.Components[0]
	if c.Events["sweep_offered"] != 12 || c.Events["sweep_prefix_cache_read_ok"] != 1 {
		t.Errorf("FromTrace dropped the component's events, so a component that records only "+
			"successes reaches the dashboard with nothing: %v", c.Events)
	}
	// And the two must not be merged on the way across.
	if c.Gates["below_output_floor"] != 11 {
		t.Errorf("FromTrace dropped the gates: %v", c.Gates)
	}
	if _, leaked := c.Gates["sweep_offered"]; leaked {
		t.Errorf("an event leaked into gates: %v", c.Gates)
	}
}

// EVENTS MUST SURVIVE THE ROUND TRIP, per request and aggregated.
//
// Splitting Report.Gates into Gates and Events reached /stats and Prometheus but not this store: the
// component row had a `gates` column and nothing else, so every name that moved — `sweep_dropped`,
// `sweep_offered`, `sweep_candidate_at_depth`, `reapplied_same_session` and the rest — vanished from
// the surface docs/hosted.md points operators to.
//
// The names that moved are the ones a component raises when it SUCCEEDS, so this row was blind in
// proportion to how well a component was doing: observed live, a turn that adjudicated twelve
// outputs and removed twelve showed an empty cell, while a turn that refused everything showed a
// full one. See #124.
//
// Both directions are asserted, because the aggregate is a SECOND query over a second column and
// would fail independently of the per-request read.
func TestEventsReachTheDashboard(t *testing.T) {
	db := openTestDB(t)
	e := &Event{TS: 2000, SessionID: "s", Model: "m", TokensBefore: 34317, TokensAfter: 977,
		Components: []CompRow{
			// The live shape that exposed this: everything it recorded is an event, so under the
			// old schema this row carried no counter information at all.
			{Component: "extract_llm_sweep", Kind: "offload", Acted: true,
				Events: map[string]int{"sweep_offered": 12, "sweep_dropped": 12,
					"sweep_candidate_at_depth": 12, "sweep_prefix_cache_read_ok": 1}},
			// And one carrying both, so the two must not be merged on the way through.
			{Component: "extract_llm", Kind: "offload", Skipped: true,
				Gates:  map[string]int{"below_output_floor": 11},
				Events: map[string]int{"reapplied_same_session": 3}},
		}}
	e2 := &Event{TS: 2001, SessionID: "s", Model: "m", TokensBefore: 100, TokensAfter: 100,
		Components: []CompRow{{Component: "extract_llm_sweep", Kind: "offload", Skipped: true,
			Gates:  map[string]int{"sweep_inventory_below_min": 4},
			Events: map[string]int{"sweep_offered": 4}}}}
	if err := db.insertBatch([]*Event{e, e2}); err != nil {
		t.Fatal(err)
	}

	// PER REQUEST.
	page, err := db.Requests(Filter{}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if page == nil || len(page.Requests) == 0 {
		t.Fatal("no requests came back, so the assertions below are vacuous")
	}
	var found bool
	for _, r := range page.Requests {
		d, err := db.Request(r.ID, true)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range d.Components {
			if c.Component != "extract_llm_sweep" || c.Events["sweep_dropped"] == 0 {
				continue
			}
			found = true
			if c.Events["sweep_offered"] != 12 || c.Events["sweep_candidate_at_depth"] != 12 {
				t.Errorf("per-request events did not round-trip: %v", c.Events)
			}
			if len(c.Gates) != 0 {
				t.Errorf("a component that recorded only events must not gain gates: %v", c.Gates)
			}
		}
	}
	if !found {
		t.Fatal("the events-only component row never came back with its events — that is #124: " +
			"a component that only records successes stores nothing readable")
	}

	// AGGREGATED over the window. A separate query over a separate column, so it fails apart from
	// the per-request read.
	comps, err := db.Components(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*ComponentRow{}
	for i := range comps {
		byName[comps[i].Component] = comps[i]
	}
	sw := byName["extract_llm_sweep"]
	if sw == nil {
		t.Fatal("extract_llm_sweep missing from the aggregate")
	}
	if sw.Events["sweep_offered"] != 16 { // 12 + 4, summed across both requests
		t.Errorf("aggregated events did not sum across requests: %v", sw.Events)
	}
	if sw.Gates["sweep_inventory_below_min"] != 4 {
		t.Errorf("aggregating events must not disturb gates: %v", sw.Gates)
	}
	// The two maps must stay separate in the aggregate as well: merging them here would undo at the
	// API what the column split did at the storage layer.
	if _, leaked := sw.Gates["sweep_offered"]; leaked {
		t.Errorf("an event leaked into the gate totals: %v", sw.Gates)
	}
}
