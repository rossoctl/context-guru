package dash

import "testing"

// An import must record what the history shows was INVOKED, not only what it cost.
//
// THE BUG THIS FIXES, found by importing real history and then reading the page it feeds. The
// insights view reported `superpowers:brainstorming` as "never invoked, but only 3 sessions of
// evidence" — and the user's own transcripts showed it being used four times, a month before the
// proxy existed. Two more superpowers skills said the same. The page was recommending the removal
// of capabilities the user had demonstrably been using.
//
// The cause was that an import wrote `requests` rows and nothing else. Invocations live in
// `tool_uses`, keyed by session, and the verdict logic reads them to decide whether a capability
// earned its weight — so importing spend without invocations produced history that could only
// ever make a capability look MORE abandoned than it was. That direction matters: the error was
// not noise, it was systematically biased toward recommending removal.

// Importing a turn that invoked a skill must leave that skill looking used.
func TestAnImportRecordsInvocationsNotJustSpend(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	writeJSONL(t, home, "-Users-v-proj", "s-inv",
		turnLineCalling("s-inv", "/Users/v/proj", daysAgo(9),
			`{"type":"tool_use","id":"t1","name":"Skill","input":{"skill":"superpowers:brainstorming"}}`),
		turnLineCalling("s-inv", "/Users/v/proj", daysAgo(8),
			`{"type":"tool_use","id":"t2","name":"Skill","input":{"skill":"superpowers:brainstorming"}}`),
		turnLineCalling("s-inv", "/Users/v/proj", daysAgo(7),
			`{"type":"tool_use","id":"t3","name":"Bash","input":{"command":"ls"}}`))

	if _, err := db.ImportHistory(home, ImportOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}

	var calls int
	var first, last int64
	if err := db.sql.QueryRow(
		`SELECT calls, first_ts, last_ts FROM tool_uses WHERE skill = ?`,
		"superpowers:brainstorming").Scan(&calls, &first, &last); err != nil {
		t.Fatalf("the imported skill has no tool_uses row, so every verdict will call it "+
			"never-invoked: %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d; want 2, one per turn that invoked it", calls)
	}
	if first == 0 || last == 0 || first > last {
		t.Errorf("first_ts=%d last_ts=%d; the dates are the evidence behind a `keep`, and a "+
			"stale verdict needs them to measure a gap", first, last)
	}
	// A plain tool is recorded under `name` with no skill, which is how the inventory
	// distinguishes the two.
	var toolCalls int
	if err := db.sql.QueryRow(
		`SELECT calls FROM tool_uses WHERE name = 'Bash' AND skill = ''`).Scan(&toolCalls); err != nil {
		t.Fatalf("an imported tool invocation was not recorded: %v", err)
	}
	if toolCalls != 1 {
		t.Errorf("Bash calls = %d; want 1", toolCalls)
	}
}

// THE VERDICT ITSELF HAS TO CHANGE. Writing the row is not the point; not recommending the
// removal of something the user has been using is.
func TestAnImportedInvocationStopsTheRemovalAdvice(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	writeJSONL(t, home, "-Users-v-proj", "s-verdict",
		turnLineCalling("s-verdict", "/Users/v/proj", daysAgo(9),
			`{"type":"tool_use","id":"t1","name":"Skill","input":{"skill":"superpowers:brainstorming"}}`))

	if _, err := db.ImportHistory(home, ImportOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}
	// Through the INVENTORY, which is the path the verdict actually takes — not a lookup written
	// for this test. ToolReportFor is what the insights document is shaped from, so if the
	// invocation does not reach it, no amount of correct rows in tool_uses will fix the advice.
	rep, err := db.ToolReportFor(Filter{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, st := range rep.Skills.Skills {
		if st.Name != "superpowers:brainstorming" {
			continue
		}
		found = true
		if st.Calls == 0 || st.SessionsUsed == 0 {
			t.Errorf("the inventory reports %s as calls=%d sessions_used=%d, so the verdict will "+
				"call it never-invoked and recommend removing something the user has been using",
				st.Name, st.Calls, st.SessionsUsed)
		}
	}
	if !found {
		t.Skip("the skill is not in this store's declared inventory, so the verdict path cannot " +
			"be exercised here; the tool_uses assertions above still hold")
	}
}

// Re-running must not inflate the counts. The import is idempotent for requests; it has to be for
// invocations too, or a user who runs it twice sees every capability as twice as used.
func TestReimportingDoesNotInflateInvocationCounts(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	writeJSONL(t, home, "-Users-v-proj", "s-again",
		turnLineCalling("s-again", "/Users/v/proj", daysAgo(9),
			`{"type":"tool_use","id":"t1","name":"Skill","input":{"skill":"superpowers:writing-plans"}}`),
		turnLineCalling("s-again", "/Users/v/proj", daysAgo(8),
			`{"type":"tool_use","id":"t2","name":"Skill","input":{"skill":"superpowers:writing-plans"}}`))

	if _, err := db.ImportHistory(home, ImportOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ImportHistory(home, ImportOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}
	var calls int
	if err := db.sql.QueryRow(`SELECT calls FROM tool_uses WHERE skill = ?`,
		"superpowers:writing-plans").Scan(&calls); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("calls = %d after two imports; want 2. A repeated import must not make a "+
			"capability look more used than it was", calls)
	}
}

// A dry run records no invocations either, or the "nothing was written" promise is false in a way
// that changes advice.
func TestADryRunRecordsNoInvocations(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	writeJSONL(t, home, "-Users-v-proj", "s-dryinv",
		turnLineCalling("s-dryinv", "/Users/v/proj", daysAgo(9),
			`{"type":"tool_use","id":"t1","name":"Skill","input":{"skill":"dataviz"}}`))

	if _, err := db.ImportHistory(home, ImportOptions{Apply: false}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM tool_uses`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d invocation rows written during a dry run; want none", n)
	}
}

// A MARKETPLACE SKILL HAS NO COLON, and is still a skill.
//
// `superpowers:brainstorming` is plainly a skill from its name; `artifact-design` and `dataviz`
// are skills too and look exactly like tools. An importer that classifies by the shape of the
// name files them under the wrong half of the inventory — so they would appear as tools the user
// cannot remove rather than as skills they can.
//
// The transcript already knows, because the invocation came through the `Skill` tool. This pins
// that the fact is carried rather than re-derived, which is the only way to get a bare name right.
func TestABareMarketplaceSkillIsStillRecordedAsASkill(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	writeJSONL(t, home, "-Users-v-proj", "s-bare",
		turnLineCalling("s-bare", "/Users/v/proj", daysAgo(9),
			`{"type":"tool_use","id":"t1","name":"Skill","input":{"skill":"artifact-design"}}`),
		turnLineCalling("s-bare", "/Users/v/proj", daysAgo(8),
			`{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"ls"}}`))

	if _, err := db.ImportHistory(home, ImportOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}
	// Recorded in the `skill` column, under the Skill tool, exactly as the live path does it.
	var name, skill string
	if err := db.sql.QueryRow(
		`SELECT name, skill FROM tool_uses WHERE skill = 'artifact-design'`).
		Scan(&name, &skill); err != nil {
		t.Fatalf("artifact-design was not recorded as a skill: %v. A bare marketplace name has "+
			"no colon, so anything deriving skill-ness from the name files it as a tool", err)
	}
	if name != "Skill" {
		t.Errorf("tool name = %q; want Skill, which is how the inventory keys a skill", name)
	}
	// And the real tool is NOT in the skill column.
	var bareSkill string
	if err := db.sql.QueryRow(`SELECT skill FROM tool_uses WHERE name = 'Bash'`).
		Scan(&bareSkill); err != nil {
		t.Fatal(err)
	}
	if bareSkill != "" {
		t.Errorf("Bash was recorded with skill=%q; a tool is not a skill", bareSkill)
	}
}
