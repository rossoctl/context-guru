package dash

import "testing"

// The `source` column: whether a row was MEASURED by the proxy or IMPORTED from a transcript.
//
// NOT BOOKKEEPING. The two kinds of row can answer different questions, and the difference is not
// recoverable from anything else on the row. A measured row saw the request go past, so it knows
// what was declared on it and can therefore say "you carried this and never called it" — the
// waste finding this whole dashboard is built around. An imported row knows only what the
// provider billed, because a transcript does not record the declaration list.
//
// So a query that cannot tell them apart will mix an inventory that exists with one that was
// never observed, and report a smaller problem than the user has — quietly, and in the one figure
// they would act on. The column is what makes that mixing impossible to do by accident.
func TestAnImportedRowIsDistinguishableFromAMeasuredOne(t *testing.T) {
	db := openTestDB(t)
	if err := db.insertBatch([]*Event{
		{TS: 1000, SessionID: "m1", Model: "m", TokensBefore: 10},
		{TS: 2000, SessionID: "m2", Model: "m", TokensBefore: 10},
		{TS: 3000, SessionID: "i1", Model: "m", TokensBefore: 10, Source: SourceImported},
	}); err != nil {
		t.Fatal(err)
	}

	count := func(f Filter) int {
		page, err := db.Requests(f, 100, 0)
		if err != nil {
			t.Fatal(err)
		}
		return len(page.Requests)
	}
	if n := count(Filter{Source: SourceImported}); n != 1 {
		t.Errorf("imported rows = %d; want 1", n)
	}
	// A MEASURED row stores "" rather than the word, because every row written before this column
	// existed is measured and cannot be back-filled. So the filter needs its own flag, exactly as
	// ProjectNone does and for the same reason: "" is a real value here, not an absence.
	if n := count(Filter{SourceMeasured: true}); n != 2 {
		t.Errorf("measured rows = %d; want 2", n)
	}
	// And an unset filter is EVERY row. Absence is not a value; reading it as one would hide
	// imported history behind a default nobody chose.
	if n := count(Filter{}); n != 3 {
		t.Errorf("an unset source filter returned %d rows; want all 3", n)
	}

	// THE VALUE COMES BACK ON THE ROW, not just in the count.
	//
	// Counting rows through a filter proved only that the WHERE clause works; a read path that
	// dropped the column entirely still passed, because the filter did the selecting. Anything
	// deciding what a row may claim has to see the marker on the row itself.
	page, err := db.Requests(Filter{}, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range page.Requests {
		got[e.SessionID] = e.Source
	}
	for sess, want := range map[string]string{"m1": "", "m2": "", "i1": SourceImported} {
		if got[sess] != want {
			t.Errorf("session %s reads Source=%q; want %q — a report cannot honour a rule it "+
				"cannot see", sess, got[sess], want)
		}
	}
}

// AN UNRECOGNISED SOURCE RECORDS AS MEASURED, not as itself.
//
// Every honesty rule downstream keys off this field: a report decides whether it may show a waste
// verdict by asking whether the window holds imported rows. A value nobody allow-listed would
// become a third kind of row that no rule covers — and the rule would then pass it, because it is
// not the imported marker. Collapsing to measured is the safe direction: it claims LESS about
// what the row supports, so a mistake under-reports rather than inventing a finding.
func TestAnUnknownSourceMarkerIsNotStored(t *testing.T) {
	db := openTestDB(t)
	if err := db.insertBatch([]*Event{
		{TS: 1000, SessionID: "junk", Model: "m", Source: "guessed-from-vibes"},
		{TS: 2000, SessionID: "good", Model: "m", Source: SourceImported},
	}); err != nil {
		t.Fatal(err)
	}
	var junk, good string
	if err := db.sql.QueryRow(`SELECT source FROM requests WHERE session_id='junk'`).
		Scan(&junk); err != nil {
		t.Fatal(err)
	}
	if err := db.sql.QueryRow(`SELECT source FROM requests WHERE session_id='good'`).
		Scan(&good); err != nil {
		t.Fatal(err)
	}
	if junk != "" {
		t.Errorf("an unknown source was stored as %q; want \"\" so no rule has to guess at it",
			junk)
	}
	if good != SourceImported {
		t.Errorf("the known marker was mangled to %q", good)
	}
}

// The column survives a migration rather than costing the history.
//
// A schemaVersion bump renames the database aside and discards every request row — thousands of
// them on a live install — so this goes in through additiveColumns like `project` before it. A
// row written before the column existed reads as "" and so reads as measured, which is correct:
// it was.
func TestTheSourceColumnArrivesWithoutDiscardingHistory(t *testing.T) {
	db := openTestDB(t)
	if err := db.insertBatch([]*Event{{TS: 1, SessionID: "old", Model: "m"}}); err != nil {
		t.Fatal(err)
	}
	var src string
	if err := db.sql.QueryRow(`SELECT source FROM requests WHERE session_id = 'old'`).
		Scan(&src); err != nil {
		t.Fatalf("the source column is not readable: %v", err)
	}
	if src != "" {
		t.Errorf("a row written with no source reads %q; want \"\", which means measured", src)
	}
}
