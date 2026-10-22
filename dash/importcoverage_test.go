package dash

import (
	"strings"
	"testing"
)

// What the report says when its window is partly imported.
//
// THE DANGER THIS GUARDS. An imported row knows what a request cost and not what it declared, so
// a window holding imported rows has days where carriage was never observed at all. The waste
// figures are computed only from what WAS observed — which is correct, and silently misleading:
// the reader sees a request count that tripled and a waste figure that did not move, and the
// obvious inference is that the extra history was cheap. It was not; it was unmeasured.
//
// So the document states its own coverage, and the one thing it may never do is report a plain
// "measured over the window" for a window that is partly imported.

// mixedStore is a store with both kinds of row: measured recent days, imported older ones.
func mixedStore(t *testing.T) *DB {
	t.Helper()
	db := openTestDB(t)
	if err := db.insertBatch([]*Event{
		// Imported: older, and carrying no declaration data by construction.
		{TS: daysAgo(9), SessionID: "i1", Model: "m", Source: SourceImported, CacheWrite: 1000},
		{TS: daysAgo(8), SessionID: "i2", Model: "m", Source: SourceImported, CacheWrite: 1000},
		// Measured: the proxy saw these, so they have a declared inventory.
		{TS: daysAgo(2), SessionID: "m1", Model: "m", TokensBefore: 10},
	}); err != nil {
		t.Fatal(err)
	}
	return db
}

// The headline rule: a partly-imported window must not present itself as measured.
func TestAPartlyImportedWindowSaysSo(t *testing.T) {
	db := mixedStore(t)
	mux := projectAPI(t, db)

	doc := insightsFor(t, mux, "?timeline=1")
	if doc.Basis == "measured over the window" {
		t.Fatal("a window containing imported rows reports the plain measured basis. The waste " +
			"figures cover only the measured part, so this invites the reader to treat " +
			"unmeasured history as cheap history")
	}
	if !strings.Contains(strings.ToLower(doc.Basis), "import") {
		t.Errorf("Basis = %q; it has to name the imported part, since that is the part whose "+
			"carriage was never observed", doc.Basis)
	}
	// And the figures behind it, so a reader can judge how much of the window is unexplained
	// rather than being told only that some of it is.
	if doc.Coverage == nil {
		t.Fatal("the document carries no Coverage block; 'partly imported' is not actionable " +
			"without knowing how partly")
	}
	if doc.Coverage.ImportedRequests != 2 || doc.Coverage.MeasuredRequests != 1 {
		t.Errorf("Coverage = %d imported / %d measured; want 2/1",
			doc.Coverage.ImportedRequests, doc.Coverage.MeasuredRequests)
	}
}

// A WINDOW WITH NO IMPORTED ROWS IS UNCHANGED. The coverage statement is a warning, and a warning
// that appears when there is nothing to warn about is one the reader learns to skip — which would
// cost exactly the case it exists for.
func TestAFullyMeasuredWindowKeepsItsPlainBasis(t *testing.T) {
	db := openTestDB(t)
	if err := db.insertBatch([]*Event{
		{TS: daysAgo(2), SessionID: "m1", Model: "m", TokensBefore: 10},
	}); err != nil {
		t.Fatal(err)
	}
	mux := projectAPI(t, db)
	doc := insightsFor(t, mux, "?timeline=1")
	if doc.Basis != "measured over the window" {
		t.Errorf("Basis = %q on a fully measured window; want the plain statement", doc.Basis)
	}
	if doc.Coverage != nil && doc.Coverage.ImportedRequests != 0 {
		t.Errorf("Coverage reports %d imported rows in a store with none",
			doc.Coverage.ImportedRequests)
	}
}

// THE WASTE FIGURES THEMSELVES MUST NOT MOVE. An imported row has no declared inventory, so it
// cannot add to what was carried — and if it did, the page would price an inventory nobody saw.
//
// This is the test that would catch an importer "improving" its output by deriving a declaration
// weight from usage: the figures would rise and look better, and be fiction.
func TestImportedRowsAddNoWaste(t *testing.T) {
	db := openTestDB(t)
	if err := db.insertBatch([]*Event{
		{TS: daysAgo(2), SessionID: "m1", Model: "m", TokensBefore: 10},
	}); err != nil {
		t.Fatal(err)
	}
	mux := projectAPI(t, db)
	before := insightsFor(t, mux, "?timeline=1")

	// Add a year of imported history, far more than the measured part.
	var imported []*Event
	for i := 10; i < 40; i++ {
		imported = append(imported, &Event{
			TS: daysAgo(i), SessionID: "i" + strings.Repeat("x", i%3), Model: "m",
			Source: SourceImported, CacheWrite: 50000, FreshInput: 900,
		})
	}
	if err := db.insertBatch(imported); err != nil {
		t.Fatal(err)
	}
	after := insightsFor(t, mux, "?timeline=1")

	if after.RemovableUSD != before.RemovableUSD {
		t.Errorf("removable waste moved from %v to %v after importing history. An imported row "+
			"records no declared inventory, so anything it adds here is invented",
			before.RemovableUSD, after.RemovableUSD)
	}
	if after.RemovableTokens != before.RemovableTokens {
		t.Errorf("removable tokens moved from %d to %d; same problem",
			before.RemovableTokens, after.RemovableTokens)
	}
}
