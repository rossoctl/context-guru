package dash

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestOverviewSessionScopedMatchesTableWide locks in the #492 invariant that makes the fix
// safe: for a single session, Overview's three window-function sub-queries (the replay
// ceiling, its keep-alive inflation correction, and CompactionResets) must return the SAME
// numbers whether or not any OTHER session is present in the database. PARTITION BY
// session_id is what makes this true — each session's window values never depend on another
// session's rows — and it is the whole argument for scoping those CTEs to the caller's own
// session instead of scanning every session (see the "#492" comments in overview.go).
func TestOverviewSessionScopedMatchesTableWide(t *testing.T) {
	target := "target-session"

	// A small fixture for the target session: enough rows, in a mix of orders, to exercise
	// the replay ceiling (saved_unique > 0), its ping-side correction (a keep-alive ping),
	// and CompactionResets (a turn smaller than the one before it).
	targetEvents := []*Event{
		mkEvent(1000, target, "m", 100, 60), // saved_unique=40
		mkEvent(2000, target, "m", 200, 90), // saved_unique=110, bigger than the previous turn
		mkEvent(3000, target, "m", 50, 20),  // saved_unique=30, SMALLER than the previous turn: a reset
	}
	ping := mkEvent(2500, target, "m", 0, 0)
	ping.KeepAlive = true
	ping.SavedUnique = 0
	targetEvents = append(targetEvents, ping)

	db := openTestDB(t)
	if err := db.insertBatch(targetEvents); err != nil {
		t.Fatal(err)
	}

	f := Filter{Session: target}
	alone, err := db.Overview(f)
	if err != nil {
		t.Fatal(err)
	}

	// Now add a few hundred rows across OTHER sessions — including sessions that themselves
	// have saved_unique > 0 and keep-alive pings, which is exactly the shape of the subquery
	// the fix removed from the CTEs' own FROM clause. If a regression reintroduces an
	// unscoped scan, these rows are what would change the numbers (or the plan's cost).
	var others []*Event
	for i := 0; i < 300; i++ {
		s := fmt.Sprintf("other-%d", i%7)
		e := mkEvent(int64(10_000+i*10), s, "m", 100, 40)
		if i%5 == 0 {
			e.KeepAlive = true
			e.SavedUnique = 0
		}
		others = append(others, e)
	}
	if err := db.insertBatch(others); err != nil {
		t.Fatal(err)
	}

	withOthers, err := db.Overview(f)
	if err != nil {
		t.Fatal(err)
	}

	if alone.ReplayProjectedTokens != withOthers.ReplayProjectedTokens {
		t.Errorf("ReplayProjectedTokens changed when other sessions were added: %d -> %d",
			alone.ReplayProjectedTokens, withOthers.ReplayProjectedTokens)
	}
	if alone.CompactionResets != withOthers.CompactionResets {
		t.Errorf("CompactionResets changed when other sessions were added: %d -> %d",
			alone.CompactionResets, withOthers.CompactionResets)
	}
	if alone.Requests != withOthers.Requests || alone.Sessions != withOthers.Sessions {
		t.Errorf("the session-scoped request/session counts themselves changed: "+
			"requests %d -> %d, sessions %d -> %d",
			alone.Requests, withOthers.Requests, alone.Sessions, withOthers.Sessions)
	}
}

// TestOverviewSessionScopedQueriesDoNotScanWholeTable is the #492 regression test: a
// session-scoped lean call must cost what the SESSION costs, not what the TABLE costs. It
// seeds many thousands of rows across other sessions — enough that an accidental return to
// an unscoped `FROM requests` inside the replay ceiling, its correction, or CompactionResets
// would show up as a full table scan in EXPLAIN QUERY PLAN — and asserts none of their CTEs
// scan `requests` without a session_id bound.
//
// This checks the PLAN rather than the wall clock on purpose: #492's own reproduction showed
// that modernc.org/sqlite's absolute cost for the pathological shape depends on planner
// statistics and session size in ways that do not reproduce reliably on a tiny, freshly built
// test database, where even a full scan of a few thousand rows is still fast in absolute
// terms. The plan is what the fix actually changed, and it does not depend on timing.
func TestOverviewSessionScopedQueriesDoNotScanWholeTable(t *testing.T) {
	target := "target-session"
	db := openTestDB(t)
	if err := db.insertBatch([]*Event{
		mkEvent(1000, target, "m", 100, 60),
		mkEvent(2000, target, "m", 50, 20),
	}); err != nil {
		t.Fatal(err)
	}

	// Many other sessions, including some with saved_unique > 0 and keep-alive pings, so the
	// pre-fix queries' own `session_id IN (SELECT ...)` / unfiltered CTEs would have had
	// plenty to scan.
	var others []*Event
	for i := 0; i < 2000; i++ {
		s := fmt.Sprintf("other-%d", i%50)
		e := mkEvent(int64(10_000+i*10), s, "m", 100, 40)
		if i%5 == 0 {
			e.KeepAlive = true
			e.SavedUnique = 0
		}
		others = append(others, e)
	}
	if err := db.insertBatch(others); err != nil {
		t.Fatal(err)
	}

	f := Filter{Session: target}
	cond, args := f.where()

	replaySQL, replayArgs := replayCeilingQuery(f, cond, args)
	compactionSQL, compactionArgs := compactionResetsQuery(f, cond, args)

	queries := map[string]struct {
		sql  string
		args []any
	}{
		// These call the EXACT functions Overview() calls (see replayCeilingQuery and
		// compactionResetsQuery in overview.go), so a regression in the real code shows up
		// here rather than only in a hand-copied duplicate that could drift from it.
		"replay ceiling (later)":  {"EXPLAIN QUERY PLAN " + replaySQL, replayArgs},
		"CompactionResets (prev)": {"EXPLAIN QUERY PLAN " + compactionSQL, compactionArgs},
	}

	for name, q := range queries {
		rows, err := db.sql.QueryContext(db.readCtx(), q.sql, q.args...)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				rows.Close()
				t.Fatalf("%s: scanning plan row: %v", name, err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		full := strings.Join(plan, " | ")
		// A plan step that scans `requests` under any of its aliases here (r, p, and the
		// pre-fix unscoped subqueries' own s/k) WITHOUT a session_id-keyed search is the #492
		// shape: unbounded by the session, bounded only by the table. "SEARCH ... session_id="
		// or "COVERING INDEX idx_requests_session" is the fixed shape; a bare "SCAN " of any of
		// those aliases, with no index clause, is not.
		for _, bad := range []string{"SCAN requests", "SCAN r ", "SCAN p ", "SCAN s", "SCAN k"} {
			if strings.Contains(full, bad) {
				t.Errorf("%s: plan scans the whole table unscoped by session (%q): %s", name, bad, full)
				break
			}
		}
	}
}

// TestOverviewSessionScopedStaysFastWithManyOtherSessions is a loose, non-flaky timing
// backstop: #492's own measurement was on a 262 MB production database, which this test
// cannot reproduce, but a session-scoped Overview() call should still finish in well under a
// second against a modest fixture, which an accidental full-table scan over many sessions
// would not reliably do once the fixture is large enough. Generous on purpose — this is a
// backstop for the plan-based test above, not the primary assertion.
func TestOverviewSessionScopedStaysFastWithManyOtherSessions(t *testing.T) {
	target := "target-session"
	db := openTestDB(t)
	if err := db.insertBatch([]*Event{mkEvent(1000, target, "m", 100, 60)}); err != nil {
		t.Fatal(err)
	}
	var others []*Event
	for i := 0; i < 5000; i++ {
		s := fmt.Sprintf("other-%d", i%100)
		e := mkEvent(int64(10_000+i*10), s, "m", 100, 40)
		if i%5 == 0 {
			e.KeepAlive = true
			e.SavedUnique = 0
		}
		others = append(others, e)
	}
	if err := db.insertBatch(others); err != nil {
		t.Fatal(err)
	}

	t0 := time.Now()
	if _, err := db.Overview(Filter{Session: target}); err != nil {
		t.Fatal(err)
	}
	if dt := time.Since(t0); dt > 2*time.Second {
		t.Errorf("session-scoped Overview() took %v against 5,000 other-session rows; "+
			"want well under a second", dt)
	}
}
