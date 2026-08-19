package dash

import (
	"slices"
	"testing"
	"time"
)

// TestFacetDropdownIsNotScopedByItsOwnSelection pins the fix for the "I have to press
// Clear every time" bug.
//
// Scoping a dropdown by the whole filter INCLUDING its own column collapses that list to
// the single value already chosen, so the dimension becomes a one-way door: the only exit
// is Clear, which discards every other filter too. The useful half — narrowing the OTHER
// dimensions to what is reachable under the current selection — must survive, so this
// asserts both directions rather than just the bug.
func TestFacetDropdownIsNotScopedByItsOwnSelection(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UnixMilli()
	// bob ran only sonnet; codex ran only haiku. So with agent=bob selected:
	//   - the AGENT list must still offer codex (or it cannot be switched to), and
	//   - the MODEL list must narrow to sonnet (the point of scoped facets).
	if err := db.insertBatch([]*Event{
		{TS: now, SessionID: "s1", Agent: "bob", Model: "sonnet", Provider: "anthropic",
			Preset: "codesmart", Mode: ModeActive, Components: []CompRow{{Component: "dedup"}}},
		{TS: now, SessionID: "s2", Agent: "codex", Model: "haiku", Provider: "openai",
			Preset: "codesafe", Mode: ModeActive, Components: []CompRow{{Component: "cmdfilter"}}},
	}); err != nil {
		t.Fatal(err)
	}

	f, err := db.Facets(Filter{Agent: "bob", TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f["agent"], "codex") {
		t.Errorf("agent facet = %v, want it to still offer \"codex\" — a dropdown scoped by "+
			"its own selection is a one-way door out of which the only exit is Clear", f["agent"])
	}
	if !slices.Contains(f["agent"], "bob") {
		t.Errorf("agent facet = %v, want it to include the selected value too", f["agent"])
	}
	// The other dimensions must STILL be scoped, or self-blanking has been applied too
	// broadly and every dropdown lists everything.
	if slices.Contains(f["model"], "haiku") {
		t.Errorf("model facet = %v: haiku belongs to codex, so it must not appear while "+
			"agent=bob is selected", f["model"])
	}
	if !slices.Contains(f["model"], "sonnet") {
		t.Errorf("model facet = %v, want sonnet", f["model"])
	}
	if slices.Contains(f["component"], "cmdfilter") {
		t.Errorf("component facet = %v: cmdfilter ran only on codex's request", f["component"])
	}

	// Same property for the component dimension, which is served by a different query
	// through the join table and so could regress independently.
	f2, err := db.Facets(Filter{Component: "dedup", TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f2["component"], "cmdfilter") {
		t.Errorf("component facet = %v, want it to still offer \"cmdfilter\" so the "+
			"dimension can be re-pointed without Clear", f2["component"])
	}
}

// TestFacetSelfBlankingNeverWidensTheTenantScope guards the dangerous direction. Tenant
// is not a user-facing dimension, it is the authorization scope; blanking it to "open up
// a dropdown" would turn the filter bar into a cross-tenant enumeration.
func TestFacetSelfBlankingNeverWidensTheTenantScope(t *testing.T) {
	for _, dim := range []string{"model", "provider", "agent", "preset", "mode", "reason", "component", "nonsense"} {
		got := selfBlanked(Filter{Tenant: "tenant-a", Agent: "bob"}, dim)
		if got.Tenant != "tenant-a" {
			t.Errorf("selfBlanked(%q) cleared Tenant: a facet query must never widen the "+
				"authorization scope", dim)
		}
		if got.TenantAll {
			t.Errorf("selfBlanked(%q) set TenantAll: that is a manager-only service-wide "+
				"view, not something a dropdown may switch on", dim)
		}
		// The other direction: a manager's already-wide scope must be PRESERVED, or a
		// manager's dropdowns silently narrow to their own account's values.
		if wide := selfBlanked(Filter{TenantAll: true, Agent: "bob"}, dim); !wide.TenantAll {
			t.Errorf("selfBlanked(%q) cleared TenantAll: it must preserve the resolved "+
				"scope in both directions, never decide it", dim)
		}
	}
}
