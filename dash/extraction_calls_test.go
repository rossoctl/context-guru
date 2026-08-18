package dash

import "testing"

// The new table must cascade with its parent. Retention, session eviction, archive
// migration and a manager purge all delete from `requests`; an orphaned extraction row
// would keep transcript content alive after the request that produced it was deleted.
func TestExtractionCallsCascadeWithTheRequest(t *testing.T) {
	db := openTestDB(t)
	e := &Event{TS: 1, TenantID: "t1", SessionID: "s1", Model: "m",
		Extractions: []ExtractionRow{
			{Component: "extract_llm", CandidateTokens: 900, SavedTokens: 120,
				CostUSD: 0.002, LatencyMs: 900, Accepted: true, Before: "b", After: "a"},
			{Component: "extract_llm", CandidateTokens: 400, GateReason: "suppressed: x"},
		}}
	if err := db.insertBatch([]*Event{e}); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		var n int
		if err := db.sql.QueryRow(`SELECT COUNT(*) FROM extraction_calls`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := count(); got != 2 {
		t.Fatalf("stored %d extraction rows, want 2", got)
	}
	if _, err := db.sql.Exec(`DELETE FROM requests`); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != 0 {
		t.Fatalf("%d extraction rows survived the request being deleted", got)
	}
}
