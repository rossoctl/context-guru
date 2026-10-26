// Tests for the shaping layer: the step between the raw tool inventory and a page that tells
// somebody what to remove.
//
// WHY THIS IS TESTED SEPARATELY FROM /api/tools. The inventory answers "what is declared and what
// was used". Shaping answers "what should you do about it", which is a different kind of claim and
// a far easier one to get subtly wrong: a verdict stronger than its evidence, a removal scope that
// names a setting the mechanism ignores, a subtotal that double-counts, or this feature quietly
// recommending its own removal. Each of those produces a report that still looks right.
//
// The same rules exist in context-guru-plugin/scripts/insights.py, because the plugin ships as
// scripts with no Go in it and a browser cannot read SKILL.md off disk. That duplication is the
// known hazard here, and TestShapingAgreesWithThePluginsOwnRules is what stops it drifting.
package dash

import (
	"strings"
	"testing"
)

// shapeFixture is an inventory with one of each shape that matters: a client tool removable on its
// own, a plugin skill that is not, an MCP tool whose server is the unit, a standalone skill, a
// built-in that must never be offered, and one item that was actually invoked.
func shapeFixture() *ToolReport {
	return &ToolReport{
		Coverage: ToolCoverage{Sessions: 9, Captured: 9},
		Tools: []ToolStat{
			{Kind: KindTool, Name: "ArtifactData", Tokens: 3043, SessionsDeclared: 9,
				UnusedReads: 188666, UnusedUSD: 0.1430, Priced: true,
				Removal: Removal{Kind: "client_tool", Command: `claude --disallowedTools "ArtifactData"`}},
			{Kind: KindMCPTool, Name: "mcp__atlas__geocode", Server: "atlas", Tokens: 1200,
				SessionsDeclared: 9, UnusedReads: 74400, UnusedUSD: 0.0563, Priced: true,
				Removal: Removal{Kind: "mcp_tool", Command: "claude mcp remove atlas"}},
			{Kind: KindTool, Name: "Bash", Tokens: 900, SessionsDeclared: 9, SessionsUsed: 9,
				Calls: 79, Priced: true,
				Removal: Removal{Kind: "builtin", Danger: true}},
		},
		Aside: []ToolStat{
			{Kind: KindTool, Name: "Artifact", Tokens: 5203, SessionsDeclared: 9,
				UnusedReads: 322586, UnusedUSD: 0.2445, Priced: true, Builtin: true,
				Removal: Removal{Kind: "builtin", Danger: true}},
			// A built-in whose removal kind is NOT "builtin". Without this row the Builtin and
			// Danger guards are untested: every other built-in here also maps to ScopeNone
			// through removalScope, so deleting the guards entirely still passed.
			{Kind: KindTool, Name: "Monitor", Tokens: 1893, SessionsDeclared: 9,
				UnusedReads: 117366, UnusedUSD: 0.0890, Priced: true, Builtin: true,
				Removal: Removal{Kind: "client_tool", Danger: true,
					Command: `claude --disallowedTools "Monitor"`}},
		},
		Skills: SkillStat{
			State: "ok", Declared: 2, ListingTokens: 480,
			Skills: []ToolStat{
				{Kind: KindSkill, Name: "superpowers:brainstorming", Tokens: 332,
					SessionsDeclared: 9, UnusedReads: 20584, UnusedUSD: 0.0156, Priced: true,
					Removal: Removal{Kind: "plugin_skill",
						Command: "claude plugin disable superpowers"}},
				{Kind: KindSkill, Name: "dataviz", Tokens: 148, SessionsDeclared: 9,
					UnusedReads: 9176, UnusedUSD: 0.0070, Priced: true,
					Removal: Removal{Kind: "skill",
						Settings: `{"skillOverrides":{"dataviz":"off"}}`}},
			},
		},
	}
}

func findShaped(t *testing.T, doc *InsightsDoc, name string) *InsightsItem {
	t.Helper()
	for i := range doc.Items {
		if doc.Items[i].Name == name {
			return &doc.Items[i]
		}
	}
	return nil
}

// TestRemovalScopeComesFromTheMechanismNotTheKind is the single most consequential rule here.
//
// `skillOverrides` has NO effect on a skill contributed by a plugin — verified on real request
// traffic, where the entry was still sent with the override set, while the same override removed a
// standalone skill entirely. So a report that offers a per-skill setting for a plugin's skill hands
// the user a line they paste and that silently does nothing. The scope must come from the row's own
// removal mechanism, never from its kind: two rows both of KindSkill need different instructions.
func TestRemovalScopeComesFromTheMechanismNotTheKind(t *testing.T) {
	doc := ShapeInsights(shapeFixture(), InsightsOptions{})

	for _, tc := range []struct{ name, want string }{
		{"dataviz", ScopeItem},                     // standalone skill: its own setting works
		{"superpowers:brainstorming", ScopePlugin}, // plugin skill: the plugin is the unit
		{"mcp__atlas__geocode", ScopeServer},       // MCP tool: the server is the unit
		{"ArtifactData", ScopeItem},                // client tool: denied by name
		{"Artifact", ScopeNone},                    // built-in: never offered
	} {
		it := findShaped(t, doc, tc.name)
		if it == nil {
			t.Errorf("%s is absent from the shaped document", tc.name)
			continue
		}
		if it.Scope != tc.want {
			t.Errorf("%s scope = %q, want %q", tc.name, it.Scope, tc.want)
		}
	}

	// Both skills are KindSkill and must still get different scopes, which is the whole point.
	a, b := findShaped(t, doc, "dataviz"), findShaped(t, doc, "superpowers:brainstorming")
	if a != nil && b != nil && a.Scope == b.Scope {
		t.Error("a standalone skill and a plugin's skill were given the same removal scope; " +
			"they need different instructions and the kind cannot tell them apart")
	}
}

// TestAVerdictNeverOutrunsItsEvidence pins the floor. A removal is recommended only for an item
// observed unused across enough sessions AND days: five sessions in one afternoon are five sessions
// on one task, and the tool nobody needed today is the one tomorrow needs. Below that the item is
// still LISTED — the floor gates advice, not disclosure — and marked `consider`.
func TestAVerdictNeverOutrunsItsEvidence(t *testing.T) {
	rep := shapeFixture()

	// Qualified: the server says these items are past its own floor.
	withFloor := ShapeInsights(rep, InsightsOptions{
		Qualified: map[string]bool{"dataviz": true},
	})
	if it := findShaped(t, withFloor, "dataviz"); it == nil {
		t.Fatal("dataviz absent")
	} else if it.Verdict != VerdictRemove {
		t.Errorf("a qualified, never-invoked, individually removable item got verdict %q, want %q",
			it.Verdict, VerdictRemove)
	}

	// Not qualified: listed, but not recommended.
	none := ShapeInsights(rep, InsightsOptions{})
	if it := findShaped(t, none, "dataviz"); it == nil {
		t.Fatal("dataviz absent when nothing qualified; the floor must not hide the inventory")
	} else if it.Verdict != VerdictConsider {
		t.Errorf("an unqualified item got verdict %q, want %q", it.Verdict, VerdictConsider)
	}

	// A built-in is never `remove`, however long it has been observed.
	if it := findShaped(t, ShapeInsights(rep, InsightsOptions{
		Qualified: map[string]bool{"Artifact": true},
	}), "Artifact"); it == nil {
		t.Fatal("Artifact absent")
	} else if it.Verdict != VerdictKeep {
		t.Errorf("a built-in got verdict %q even though it qualified; removing one breaks the "+
			"agent and no amount of evidence changes that", it.Verdict)
	}

	// An invoked item is never offered either, and its reason must differ from a built-in's:
	// "you use this" and "this would break your agent" are different facts.
	bash := findShaped(t, none, "Bash")
	art := findShaped(t, none, "Artifact")
	if bash == nil || art == nil {
		t.Fatal("Bash or Artifact absent")
	}
	if bash.Verdict != VerdictKeep {
		t.Errorf("an invoked tool got verdict %q, want %q", bash.Verdict, VerdictKeep)
	}
	// The two reasons must be structurally different, not merely different because one happens
	// to interpolate a different number. An earlier version compared Bash (9 sessions used) with
	// Artifact (0), so the strings differed by accident and giving both the SAME wording still
	// passed — the mutant survived. Asserted on the distinguishing CLAIM instead.
	if !strings.Contains(bash.Why, "invoked") || !strings.Contains(bash.Why, "earns") {
		t.Errorf("an invoked item's reason (%q) does not say it was invoked and earns its "+
			"weight, which is why it is kept", bash.Why)
	}
	if !strings.Contains(art.Why, "Claude Code's own") {
		t.Errorf("a built-in's reason (%q) does not say it is a built-in; 'you use this' and "+
			"'this would break your agent' are different facts and the reader needs to know "+
			"which applies", art.Why)
	}
	if bash.Why == art.Why {
		t.Error("an invoked item and a built-in carry the same explanation")
	}
	if bash.Why == "" || art.Why == "" {
		t.Error("a verdict with no stated evidence is an assertion")
	}
}

// TestOwnersRollUpAndAreNotAnAddend pins the arithmetic a rollup makes newly tempting. An MCP
// tool is counted once as an item and again inside its server's subtotal: the same tokens at two
// levels. Summing the owners double-counts, and summing them into the removable total
// double-counts differently.
func TestOwnersRollUpAndAreNotAnAddend(t *testing.T) {
	doc := ShapeInsights(shapeFixture(), InsightsOptions{})

	var plug, srv *InsightsOwner
	for i := range doc.Owners {
		switch {
		case doc.Owners[i].Kind == "plugin" && doc.Owners[i].Name == "superpowers":
			plug = &doc.Owners[i]
		case doc.Owners[i].Kind == "server" && doc.Owners[i].Name == "atlas":
			srv = &doc.Owners[i]
		}
	}
	if plug == nil || srv == nil {
		t.Fatalf("expected a plugin owner and a server owner, got %d owner(s)", len(doc.Owners))
	}
	if plug.Items != 1 || srv.Items != 1 {
		t.Errorf("owner item counts = plugin %d, server %d, want 1 and 1", plug.Items, srv.Items)
	}
	if plug.Command == "" || srv.Command == "" {
		t.Error("an owner subtotal with no command is not actionable; the figure is only useful " +
			"beside the thing that acts on it")
	}

	// The removable total counts items once and excludes what cannot be removed.
	// 0.1430 (ArtifactData) + 0.0563 (atlas tool) + 0.0156 (plugin skill) + 0.0070 (dataviz)
	want := 0.1430 + 0.0563 + 0.0156 + 0.0070
	nearEq(t, "removable total over items", doc.RemovableUSD, want)

	// The built-in's weight is NOT in it, though the item is still listed.
	if doc.RemovableUSD >= want+0.2 {
		t.Error("the removable total includes the built-in; its weight is real but it is not a " +
			"saving available to anybody")
	}
	// And owner subtotals summed must not equal it, or items with no owner have been dropped.
	var ownerSum float64
	for _, o := range doc.Owners {
		ownerSum += o.USD
	}
	if nearly(ownerSum, doc.RemovableUSD) {
		t.Error("the sum of owner subtotals equals the removable total, which means every item " +
			"has an owner — the standalone skill and the client tool have none, so one of the " +
			"two figures is wrong")
	}
}

// TestThisFeatureIsNotBilledToTheReader keeps the reporter out of its own report. A cost report
// that counts its own weight as a saving it recommends is arguing for its own removal in the
// middle of its advice, and every total it prints is inflated by the cost of printing it.
func TestThisFeatureIsNotBilledToTheReader(t *testing.T) {
	rep := shapeFixture()
	rep.Skills.Skills = append(rep.Skills.Skills, ToolStat{
		Kind: KindSkill, Name: "context-guru:insights", Tokens: 143, SessionsDeclared: 9,
		UnusedReads: 8580, UnusedUSD: 0.0143, Priced: true,
		Removal: Removal{Kind: "plugin_skill", Command: "claude plugin disable context-guru"},
	})
	doc := ShapeInsights(rep, InsightsOptions{})

	if it := findShaped(t, doc, "context-guru:insights"); it != nil {
		t.Error("this plugin's own skill appears in the inventory it produces")
	}
	for _, o := range doc.Owners {
		if o.Name == "context-guru" {
			t.Error("this plugin appears as an owner in its own report")
		}
	}
	// Disclosed rather than hidden: a reader who counts 3 skills in /plugin and 2 here must be
	// able to find out why. "The tool hid its own cost" is a worse look than the cost.
	if doc.SelfExcluded.Items != 1 {
		t.Errorf("self_excluded items = %d, want 1", doc.SelfExcluded.Items)
	}
	nearEq(t, "self-excluded usd", doc.SelfExcluded.USD, 0.0143)
	if doc.SelfExcluded.Command == "" {
		t.Error("the exclusion is a reporting choice, not a claim that this plugin cannot be " +
			"removed; the command belongs with the figure")
	}
}

// nearly is the tolerance these dollar comparisons need. Exact float equality on a sum of rates is
// the bug TestCalculatorPricesFromTheRowsModelNotABlend demonstrates.
func nearly(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}
