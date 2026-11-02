package dash

import "testing"

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
