package dash

import (
	"os"
	"testing"
	"time"
)

// Importing pre-install history from Claude Code's transcripts into the requests table.
//
// The reader (transcript.go) turns a session file into billed turns; this turns those turns into
// rows. Two rules decide whether the result can be trusted, and both are tested here rather than
// asserted in a comment: an imported row must never overlap a measured one, and it must never
// carry a figure the transcript could not support.

// daysAgo is a timestamp n days before now, as epoch ms — transcripts are dated in real time, so
// the fixtures have to be too. Named around the existing `day` helper in this package.
func daysAgo(n int) int64 {
	return time.Now().AddDate(0, 0, -n).UnixMilli()
}

// THE CUTOFF. An imported row must never land on a day the proxy already measured.
//
// The two sources overlap by construction: a transcript covers the whole session, including the
// part that ran after the proxy was installed. Importing that part would count the same request
// twice — once measured, once imported — and double every figure on the overlapping days. The
// measured row is always the better one, because it knows what the request declared.
func TestAnImportStopsWhereMeasuredDataBegins(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()

	// The proxy measured day 5 onward.
	if err := db.insertBatch([]*Event{
		{TS: daysAgo(5), SessionID: "s-span", Model: "m", TokensBefore: 10},
	}); err != nil {
		t.Fatal(err)
	}
	// A transcript spanning days 10 to 2 — four days before the proxy, two after.
	var lines []string
	for _, d := range []int{10, 9, 8, 6, 4, 2} {
		lines = append(lines, turnLine("s-span", "/Users/v/proj", daysAgo(d), 100, 10))
	}
	writeJSONL(t, home, "-Users-v-proj", "s-span", lines...)

	got, err := db.ImportHistory(home, ImportOptions{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Imported != 4 {
		t.Errorf("imported %d turns; want the 4 that predate the measured window. Importing the "+
			"rest would count those requests twice, once from each source", got.Imported)
	}
	if got.SkippedOverlap != 2 {
		t.Errorf("SkippedOverlap = %d; want 2, and it has to be REPORTED: a silent skip looks "+
			"like a transcript that was short", got.SkippedOverlap)
	}
	var n int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM requests WHERE source = ?`,
		SourceImported).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("%d imported rows on disk; want 4", n)
	}
}

// THE BOUNDARY ITSELF belongs to the measured side.
//
// A turn at exactly the earliest measured timestamp IS the request the proxy recorded — the
// transcript and the proxy are describing the same moment from two sides. `>` instead of `>=`
// imports it as well, so that one request exists twice and every figure on that day is overstated
// by it. One row is a small error; the same off-by-one on a busy boundary second is not, and
// neither is visible once written.
func TestTheMeasuredBoundaryItselfIsNotImported(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	at := daysAgo(5)

	if err := db.insertBatch([]*Event{
		{TS: at, SessionID: "s-edge", Model: "m", TokensBefore: 10},
	}); err != nil {
		t.Fatal(err)
	}
	// The same moment in the transcript, plus one turn safely before it.
	writeJSONL(t, home, "-Users-v-proj", "s-edge",
		turnLine("s-edge", "/Users/v/proj", at, 100, 10),
		turnLine("s-edge", "/Users/v/proj", daysAgo(6), 100, 10))

	got, err := db.ImportHistory(home, ImportOptions{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Imported != 1 {
		t.Errorf("imported %d turns; want only the 1 strictly before the measured boundary — "+
			"the turn AT the boundary is the request the proxy already has", got.Imported)
	}
	var atBoundary int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM requests WHERE ts = ?`, at).
		Scan(&atBoundary); err != nil {
		t.Fatal(err)
	}
	if atBoundary != 1 {
		t.Errorf("%d rows at the boundary timestamp; want 1. The same request now exists twice, "+
			"once from each source", atBoundary)
	}
}

// CARRIAGE IS NEVER INVENTED. This is the strongest rule in the feature.
//
// Every cost figure on the insights page means "spent carrying this for nothing", and that needs
// the list of tools each request DECLARED. A transcript does not record it. So an imported row
// must carry no declaration count, no unused reads and no waste dollars — if it did, the page
// would show a real-looking price for an inventory nobody observed, and the one thing this
// dashboard claims is that every number traces to a measurement.
func TestAnImportedRowCarriesNoFigureTheTranscriptCannotSupport(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	writeJSONL(t, home, "-Users-v-proj", "s-care",
		turnLine("s-care", "/Users/v/proj", daysAgo(9), 5000, 400))

	if _, err := db.ImportHistory(home, ImportOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}
	var tools, declTok int
	var baseline float64
	if err := db.sql.QueryRow(
		`SELECT tools, filtered_decl_tokens, baseline_cost_usd FROM requests WHERE source = ?`,
		SourceImported).Scan(&tools, &declTok, &baseline); err != nil {
		t.Fatal(err)
	}
	if tools != 0 {
		t.Errorf("an imported row claims %d declared tools; a transcript does not record them, "+
			"so any number here is invented", tools)
	}
	if declTok != 0 {
		t.Errorf("an imported row claims %d declaration tokens; same problem", declTok)
	}
	if baseline != 0 {
		t.Errorf("an imported row carries a baseline cost of %v, which is a counterfactual it "+
			"has no basis for", baseline)
	}
}

// The usage it CAN support arrives intact, priced the same way a live row is.
func TestAnImportedRowCarriesTheUsageTheProviderBilled(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	writeJSONL(t, home, "-Users-v-proj", "s-usage",
		turnLine("s-usage", "/Users/v/proj", daysAgo(9), 5000, 400))

	if _, err := db.ImportHistory(home, ImportOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}
	page, err := db.Requests(Filter{Source: SourceImported}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Requests) != 1 {
		t.Fatalf("got %d imported rows; want 1", len(page.Requests))
	}
	e := page.Requests[0]
	if e.CacheWrite != 5000 || e.OutputTokens != 400 {
		t.Errorf("usage lost in the import: write=%d out=%d; want 5000/400",
			e.CacheWrite, e.OutputTokens)
	}
	if e.Project != "/Users/v/proj" {
		t.Errorf("Project = %q; the cwd is what makes an imported row attributable", e.Project)
	}
	if e.Source != SourceImported {
		t.Errorf("Source = %q; want %q", e.Source, SourceImported)
	}
}

// A dry run reports what it would do and writes nothing. It is the mode a user should run first,
// since this reads their filesystem and writes thousands of rows.
func TestADryRunImportWritesNothing(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	writeJSONL(t, home, "-Users-v-proj", "s-dry",
		turnLine("s-dry", "/Users/v/proj", daysAgo(9), 100, 10),
		turnLine("s-dry", "/Users/v/proj", daysAgo(8), 100, 10))

	got, err := db.ImportHistory(home, ImportOptions{Apply: false})
	if err != nil {
		t.Fatal(err)
	}
	if got.Importable != 2 {
		t.Errorf("Importable = %d; want 2 — a dry run has to say how big the job is", got.Importable)
	}
	if got.Imported != 0 {
		t.Errorf("a dry run wrote %d rows; it must write none", got.Imported)
	}
	var n int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d rows reached the database during a dry run", n)
	}
}

// RE-RUNNABLE. An import that duplicates its own rows on a second run is one nobody can repeat
// safely — and repeating it is the normal case, since a user accumulates more history every day.
func TestRunningTheImportTwiceDoesNotDuplicate(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	writeJSONL(t, home, "-Users-v-proj", "s-twice",
		turnLine("s-twice", "/Users/v/proj", daysAgo(9), 100, 10),
		turnLine("s-twice", "/Users/v/proj", daysAgo(8), 100, 10))

	first, err := db.ImportHistory(home, ImportOptions{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.ImportHistory(home, ImportOptions{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if first.Imported != 2 {
		t.Fatalf("first run imported %d; want 2", first.Imported)
	}
	if second.Imported != 0 {
		t.Errorf("the second run imported %d rows again; want 0", second.Imported)
	}
	if second.SkippedAlready != 2 {
		t.Errorf("SkippedAlready = %d; want 2, so a re-run says why it did nothing rather than "+
			"looking like it failed", second.SkippedAlready)
	}
	var n int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("%d rows after two runs; want 2", n)
	}
}

// What was CALLED is recorded, because "which of my skills have I ever used" is the question the
// whole import exists to answer — and it is answerable where the cost of carrying them is not.
func TestAnImportRecordsWhatWasCalled(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	writeJSONL(t, home, "-Users-v-proj", "s-called",
		turnLineCalling("s-called", "/Users/v/proj", daysAgo(9),
			`{"type":"tool_use","id":"t1","name":"Skill","input":{"skill":"superpowers:brainstorming"}}`),
		turnLineCalling("s-called", "/Users/v/proj", daysAgo(8),
			`{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"ls"}}`))

	got, err := db.ImportHistory(home, ImportOptions{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Called["superpowers:brainstorming"] != 1 {
		t.Errorf("Called = %v; want the skill under its own name, which is the one finding this "+
			"import can make about capabilities", got.Called)
	}
	if got.Called["Bash"] != 1 {
		t.Errorf("Called = %v; want Bash recorded too", got.Called)
	}
	if _, bad := got.Called["Skill"]; bad {
		t.Error("a skill was recorded against the `Skill` tool, which collapses every skill in " +
			"the user's history into one name")
	}
}

// S4.1: one measured request in one session must not hide another session's older or newer
// history. The cutoff is per session.
func TestAMeasuredSessionDoesNotHideOtherSessionsHistory(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	if err := db.insertBatch([]*Event{
		{TS: daysAgo(20), SessionID: "measured", Model: "m", TokensBefore: 10},
	}); err != nil {
		t.Fatal(err)
	}
	writeJSONL(t, home, "-p", "other",
		turnLine("other", "/Users/v/other", daysAgo(5), 100, 10),
		turnLine("other", "/Users/v/other", daysAgo(3), 100, 10))
	got, err := db.ImportHistory(home, ImportOptions{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Imported != 2 || got.SkippedOverlap != 0 {
		t.Errorf("imported %d, overlap %d; want 2 and 0", got.Imported, got.SkippedOverlap)
	}
}

// S4.2/S4.4: an interrupted import (requests written, uses lost) heals on the next run, and a
// re-run never lowers or doubles a count.
func TestAReRunHealsMissingUsesAndNeverDoublesThem(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	writeJSONL(t, home, "-p", "s1",
		turnLineCalling("s1", "/Users/v/p", daysAgo(9), `{"type":"tool_use","name":"Bash","input":{}}`),
		turnLineCalling("s1", "/Users/v/p", daysAgo(8), `{"type":"tool_use","name":"Bash","input":{}}`))
	if _, err := db.ImportHistory(home, ImportOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}
	calls := func() int {
		var n int
		db.sql.QueryRow(`SELECT COALESCE(SUM(calls),0) FROM tool_uses WHERE name='Bash'`).Scan(&n)
		return n
	}
	if calls() != 2 {
		t.Fatalf("calls = %d; want 2", calls())
	}
	db.sql.Exec(`DELETE FROM tool_uses`) // the lost half of an interrupted run
	if _, err := db.ImportHistory(home, ImportOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}
	if calls() != 2 {
		t.Errorf("after healing calls = %d; want 2", calls())
	}
	if _, err := db.ImportHistory(home, ImportOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}
	if calls() != 2 {
		t.Errorf("after a third run calls = %d; want still 2", calls())
	}
}

// S4: a re-run that sees FEWER turns for a (session, project, name) than are stored must not
// lower the stored count. History can shrink under it (a trimmed transcript, a narrower -since)
// and an absolute write would silently undercount.
func TestAReRunWithLessHistoryNeverLowersACount(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	path := writeJSONL(t, home, "-p", "s1",
		turnLineCalling("s1", "/Users/v/p", daysAgo(9), `{"type":"tool_use","name":"Bash","input":{}}`),
		turnLineCalling("s1", "/Users/v/p", daysAgo(8), `{"type":"tool_use","name":"Bash","input":{}}`),
		turnLineCalling("s1", "/Users/v/p", daysAgo(7), `{"type":"tool_use","name":"Bash","input":{}}`))
	if _, err := db.ImportHistory(home, ImportOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}
	// The transcript later holds only one turn.
	if err := os.WriteFile(path, []byte(turnLineCalling("s1", "/Users/v/p", daysAgo(7),
		`{"type":"tool_use","name":"Bash","input":{}}`)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ImportHistory(home, ImportOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}
	var n int
	db.sql.QueryRow(`SELECT calls FROM tool_uses WHERE name='Bash'`).Scan(&n)
	if n != 3 {
		t.Errorf("calls = %d after a re-run with less history; want 3, never lowered", n)
	}
}
