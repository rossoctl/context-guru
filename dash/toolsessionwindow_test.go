// Tests for the two things an unused-capability report needs that the aggregate inventory cannot
// answer, both of which exist because a long-lived project's history outlives its configuration.
//
//  1. LAST-N-SESSIONS WINDOWING. A folder worked in for three months, with a plugin installed three
//     weeks ago, has an inventory dominated by the ninety days before that plugin existed. The
//     aggregate says the new skills are barely declared and barely cost anything, because it is
//     averaging them over sessions that predate them. Windowing to the recent sessions is what
//     makes a newly installed, barely used plugin visible at all.
//
//  2. PER-SESSION COST ATTRIBUTION. "This skill cost you $X" is a number nobody can check. "In
//     these four sessions it cost $a, $b, $c, $d, and you never called it" is a number a user can
//     audit against their own memory of what they were doing, which is the difference between a
//     figure that persuades and a figure that gets ignored.
package dash

import (
	"context"
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// seedSessionWithDecl inserts one session of `requests` requests at `ts`, declaring `name` at
// `tokens` tokens and never invoking it. Everything here is deliberately uniform except the
// session id and the timestamp, so a windowing test's result can only come from the window.
func seedSessionWithDecl(t *testing.T, db *DB, session string, ts int64, requests int,
	name string, tokens int) {
	t.Helper()
	for i := 0; i < requests; i++ {
		insertReq(t, db, &Event{
			TS: ts + int64(i), SessionID: session, Model: "m1", TenantID: "t1",
			TokensBefore: 100, TokensAfter: 100, Meta: Meta{Tools: 1},
			CacheRead: 100, TokenAccounting: AccountingComplete,
		})
	}
	if _, err := db.sql.Exec(`INSERT INTO tool_declarations(
		tenant_id, session_id, digest, kind, name, server, tokens, ts)
		VALUES('t1',?,'d1',?,?,'',?,?)`, session, KindTool, name, tokens, ts); err != nil {
		t.Fatal(err)
	}
}

func handPriceFn() func(string) (modelinfo.Price, bool) {
	return func(m string) (modelinfo.Price, bool) { return handPrice.Price(context.Background(), m) }
}

// TestRecentSessionsSelectsTheLastNByRecency is the query the window rests on. "The last ten
// sessions" has to mean the ten most recent by their LAST activity, not by their first: a session
// opened on Monday and still running on Friday is a current session, and ordering by start would
// rank it behind every session opened since.
func TestRecentSessionsSelectsTheLastNByRecency(t *testing.T) {
	db := openTestDB(t)
	// The fixture has to DISCRIMINATE between ordering by a session's first request and by its
	// last, so `straddler` is the OLDEST session by start and the newest by end: opened before
	// everything else, still active after everything else. Under MIN(ts) ordering it ranks last
	// and a "last 2" window drops it; under MAX(ts) it ranks first.
	//
	// An earlier version of this fixture had straddler starting between the other two, where both
	// orderings happen to return the same pair — so the test passed against a MIN(ts)
	// implementation and pinned nothing. Verified by mutation: MIN(ts) now fails it.
	seedSessionWithDecl(t, db, "straddler", 1_000, 2, "A", 100)
	seedSessionWithDecl(t, db, "old", 2_000, 2, "A", 100)
	seedSessionWithDecl(t, db, "recent", 3_000, 2, "A", 100)
	// straddler's latest request lands after everything else's.
	insertReq(t, db, &Event{TS: 9_000, SessionID: "straddler", Model: "m1", TenantID: "t1",
		TokensBefore: 100, TokensAfter: 100, Meta: Meta{Tools: 1},
		CacheRead: 100, TokenAccounting: AccountingComplete})

	ids, err := db.RecentSessions(Filter{TenantAll: true}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("got %d session(s), want 2: %v", len(ids), ids)
	}
	got := map[string]bool{ids[0]: true, ids[1]: true}
	if !got["straddler"] || !got["recent"] {
		t.Errorf("last 2 sessions = %v, want straddler and recent. Ordering by a session's FIRST "+
			"request would have dropped straddler, which is still the most recently active", ids)
	}
	if got["old"] {
		t.Error("`old` is in the last 2 and should not be")
	}

	// Asking for more than exist returns what exists rather than erroring: a fresh install has
	// one session and a selector offering "last 10" must still work there.
	all, err := db.RecentSessions(Filter{TenantAll: true}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("got %d of 3 sessions when asking for 50; a limit above the population must not "+
			"truncate or fail", len(all))
	}
}

// TestWindowingToRecentSessionsChangesWhatTheInventorySays is the whole point of feature 4, written
// as the scenario it exists for: ninety sessions that never declared the new plugin, then four that
// did. Over all history the plugin's skill is a rounding error. Over the last four sessions it is
// the entire inventory.
func TestWindowingToRecentSessionsChangesWhatTheInventorySays(t *testing.T) {
	db := openTestDB(t)
	for i := 0; i < 90; i++ {
		seedSessionWithDecl(t, db, "old"+string(rune('a'+i%26))+string(rune('a'+i/26)),
			int64(1_000+i*10), 1, "OldTool", 100)
	}
	for i := 0; i < 4; i++ {
		seedSessionWithDecl(t, db, "new"+string(rune('a'+i)), int64(500_000+int64(i)*10), 1,
			"NewPluginSkill", 900)
	}

	all, err := db.ToolReportFor(Filter{TenantAll: true}, handPriceFn())
	if err != nil {
		t.Fatal(err)
	}
	recent, err := db.RecentSessions(Filter{TenantAll: true}, 4)
	if err != nil {
		t.Fatal(err)
	}
	windowed, err := db.ToolReportFor(Filter{TenantAll: true, SessionIn: recent}, handPriceFn())
	if err != nil {
		t.Fatal(err)
	}

	find := func(rep *ToolReport, name string) *ToolStat {
		for i := range rep.Tools {
			if rep.Tools[i].Name == name {
				return &rep.Tools[i]
			}
		}
		for i := range rep.Aside {
			if rep.Aside[i].Name == name {
				return &rep.Aside[i]
			}
		}
		return nil
	}
	if s := find(all, "OldTool"); s == nil {
		t.Error("OldTool is absent from the all-history report, which should be dominated by it")
	}
	if s := find(windowed, "OldTool"); s != nil {
		t.Errorf("OldTool is still in the 4-session window (declared in %d sessions); windowing "+
			"did not restrict the declaration set", s.SessionsDeclared)
	}
	ns := find(windowed, "NewPluginSkill")
	if ns == nil {
		t.Fatal("NewPluginSkill is absent from the 4-session window, which is exactly the " +
			"question the window was asked")
	}
	if ns.SessionsDeclared != 4 {
		t.Errorf("NewPluginSkill declared in %d session(s) in the window, want 4", ns.SessionsDeclared)
	}
	// And the window's own coverage reflects the window, not all history: a report that windowed
	// the rows but kept the global session count would divide by the wrong denominator and
	// understate every per-request weight.
	if windowed.Coverage.Sessions != 4 {
		t.Errorf("windowed coverage reports %d sessions, want 4; the denominator must be the "+
			"window or every share and per-request figure in it is wrong",
			windowed.Coverage.Sessions)
	}
}

// TestPerSessionCostIsAttributedAndSumsToTheTotal is feature 5. The two assertions matter equally:
// the per-session rows have to exist, AND they have to add up to the aggregate the report already
// publishes. A breakdown that does not reconcile with its own total is worse than no breakdown,
// because both numbers then appear in the same report and the reader cannot tell which is wrong.
func TestPerSessionCostIsAttributedAndSumsToTheTotal(t *testing.T) {
	db := openTestDB(t)
	// Three sessions of different lengths declaring the same never-invoked tool. Different
	// lengths are the point: the cost of carrying a declaration is its tokens times the number
	// of times the session re-read them, so equal sessions would hide an attribution bug.
	seedSessionWithDecl(t, db, "s1", 1_000, 2, "Ghost", 500)
	seedSessionWithDecl(t, db, "s2", 2_000, 7, "Ghost", 500)
	seedSessionWithDecl(t, db, "s3", 3_000, 11, "Ghost", 500)

	rep, err := db.ToolReportFor(Filter{TenantAll: true, PerSession: true}, handPriceFn())
	if err != nil {
		t.Fatal(err)
	}
	var ghost *ToolStat
	for i := range rep.Tools {
		if rep.Tools[i].Name == "Ghost" {
			ghost = &rep.Tools[i]
		}
	}
	if ghost == nil {
		for i := range rep.Aside {
			if rep.Aside[i].Name == "Ghost" {
				ghost = &rep.Aside[i]
			}
		}
	}
	if ghost == nil {
		t.Fatal("Ghost is absent from the report")
	}
	if len(ghost.Sessions) != 3 {
		t.Fatalf("Ghost carries %d per-session row(s), want 3: without them a user cannot audit "+
			"the figure against what they remember doing", len(ghost.Sessions))
	}

	// Each row names its session, its re-read count and its own cost.
	byID := map[string]SessionCostRow{}
	var sum float64
	var reads int
	for _, r := range ghost.Sessions {
		byID[r.Session] = r
		sum += r.UnusedUSD
		reads += r.UnusedReads
	}
	for id, wantReq := range map[string]int{"s1": 2, "s2": 7, "s3": 11} {
		r, ok := byID[id]
		if !ok {
			t.Errorf("no row for session %s", id)
			continue
		}
		if r.Requests != wantReq {
			t.Errorf("session %s: requests=%d, want %d", id, r.Requests, wantReq)
		}
		// 500 tokens re-read on every request of the session.
		if want := 500 * wantReq; r.UnusedReads != want {
			t.Errorf("session %s: unused_reads=%d, want %d (500 tokens x %d requests)",
				id, r.UnusedReads, want, wantReq)
		}
		if r.UnusedUSD <= 0 {
			t.Errorf("session %s: unused_usd=%v, want a positive cost — these sessions are priced",
				id, r.UnusedUSD)
		}
	}

	// THE RECONCILIATION. The per-session rows must add up to the item's own total.
	if int64(reads) != ghost.UnusedReads {
		t.Errorf("per-session reads sum to %d but the item reports %d; a breakdown that does not "+
			"reconcile with its own total puts two contradictory numbers in one report",
			reads, ghost.UnusedReads)
	}
	nearEq(t, "per-session usd sums to the item total", sum, ghost.UnusedUSD)
}

// TestPerSessionBreakdownIsOffByDefault keeps the new field from changing an existing response.
// /api/tools is read by the dashboard UI, the status line and the plugin, and a per-session array
// on every row of a large inventory is a materially bigger payload for readers that do not use it.
func TestPerSessionBreakdownIsOffByDefault(t *testing.T) {
	db := openTestDB(t)
	seedSessionWithDecl(t, db, "s1", 1_000, 3, "Ghost", 500)

	rep, err := db.ToolReportFor(Filter{TenantAll: true}, handPriceFn())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range append(append([]ToolStat{}, rep.Tools...), rep.Aside...) {
		if len(s.Sessions) != 0 {
			t.Errorf("%s carries %d per-session row(s) without PerSession set; the breakdown must "+
				"be opt-in so existing readers' payloads do not grow", s.Name, len(s.Sessions))
		}
	}
}

// TestAnUnpricedSessionContributesTokensAndNoDollars pins the honesty rule this package already
// applies to its aggregates, at the new per-session level. A session whose model has no known rate
// has a real token cost and an unknown dollar one. Reporting 0.0 there would read as free.
func TestAnUnpricedSessionContributesTokensAndNoDollars(t *testing.T) {
	db := openTestDB(t)
	seedSessionWithDecl(t, db, "priced", 1_000, 3, "Ghost", 500)
	// A model the hand-written price list does not know.
	for i := 0; i < 3; i++ {
		insertReq(t, db, &Event{
			TS: int64(2_000 + i), SessionID: "unpriced", Model: "no-such-model-xyz",
			TenantID: "t1", TokensBefore: 100, TokensAfter: 100, Meta: Meta{Tools: 1},
			CacheRead: 100, TokenAccounting: AccountingComplete,
		})
	}
	if _, err := db.sql.Exec(`INSERT INTO tool_declarations(
		tenant_id, session_id, digest, kind, name, server, tokens, ts)
		VALUES('t1','unpriced','d1',?,'Ghost','',500,2000)`, KindTool); err != nil {
		t.Fatal(err)
	}

	rep, err := db.ToolReportFor(Filter{TenantAll: true, PerSession: true}, handPriceFn())
	if err != nil {
		t.Fatal(err)
	}
	var ghost *ToolStat
	for i := range rep.Tools {
		if rep.Tools[i].Name == "Ghost" {
			ghost = &rep.Tools[i]
		}
	}
	if ghost == nil {
		for i := range rep.Aside {
			if rep.Aside[i].Name == "Ghost" {
				ghost = &rep.Aside[i]
			}
		}
	}
	if ghost == nil {
		t.Fatal("Ghost is absent")
	}
	var seenUnpriced bool
	for _, r := range ghost.Sessions {
		if r.Session != "unpriced" {
			continue
		}
		seenUnpriced = true
		if r.Priced {
			t.Error("the unpriced session is marked priced")
		}
		if r.UnusedUSD != 0 {
			t.Errorf("unpriced session carries usd=%v; it must carry none rather than a figure "+
				"derived from a rate nobody has", r.UnusedUSD)
		}
		if r.UnusedReads != 1500 {
			t.Errorf("unpriced session reads=%d, want 1500: the TOKEN cost is known even where "+
				"the dollar cost is not, and dropping it would understate the weight", r.UnusedReads)
		}
	}
	if !seenUnpriced {
		t.Error("the unpriced session has no row at all; omitting it makes the breakdown look " +
			"complete while hiding real carried weight")
	}
}

// TestToolsAPIHonoursSessionsAndBreakdownParams pins the two new query parameters at the HTTP
// layer, because the DB functions being right is not the same as the endpoint reaching them — and
// the failure mode of a windowing parameter that is silently ignored is the worst kind: the caller
// gets a correct-looking report of the wrong window and nothing says so.
func TestToolsAPIHonoursSessionsAndBreakdownParams(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	db := rec.DB()
	seedSessionWithDecl(t, db, "oldest", 1_000, 2, "OldTool", 100)
	seedSessionWithDecl(t, db, "newest", 500_000, 3, "NewTool", 900)

	names := func(body map[string]any) map[string]bool {
		out := map[string]bool{}
		for _, key := range []string{"tools", "aside"} {
			list, _ := body[key].([]any)
			for _, row := range list {
				if m, ok := row.(map[string]any); ok {
					out[m["name"].(string)] = true
				}
			}
		}
		return out
	}

	// Unwindowed: both sessions' declarations are present.
	_, all := get(t, a, "/api/tools", "")
	if n := names(all); !n["OldTool"] || !n["NewTool"] {
		t.Fatalf("unwindowed report is missing a declaration: %v", n)
	}

	// sessions=1 keeps only the most recently active session.
	_, one := get(t, a, "/api/tools?sessions=1", "")
	n := names(one)
	if !n["NewTool"] {
		t.Error("sessions=1 dropped the newest session's declaration")
	}
	if n["OldTool"] {
		t.Error("sessions=1 still carries the older session's declaration, so the parameter was " +
			"ignored — a caller asking for the last session got all history under that label")
	}

	// breakdown=session attaches the per-session rows; its absence leaves them off.
	_, plain := get(t, a, "/api/tools?sessions=1", "")
	for _, row := range plain["tools"].([]any) {
		if m, ok := row.(map[string]any); ok {
			if _, present := m["sessions"]; present {
				t.Errorf("%v carries a sessions array without breakdown=session", m["name"])
			}
		}
	}
	_, brk := get(t, a, "/api/tools?sessions=1&breakdown=session", "")
	var sawRows bool
	for _, row := range brk["tools"].([]any) {
		m, ok := row.(map[string]any)
		if !ok {
			continue
		}
		rows, present := m["sessions"].([]any)
		if !present || len(rows) == 0 {
			continue
		}
		sawRows = true
		first, _ := rows[0].(map[string]any)
		for _, field := range []string{"session", "requests", "unused_reads"} {
			if _, ok := first[field]; !ok {
				t.Errorf("per-session row for %v has no %q", m["name"], field)
			}
		}
	}
	if !sawRows {
		t.Error("breakdown=session returned no per-session rows at all")
	}
}
