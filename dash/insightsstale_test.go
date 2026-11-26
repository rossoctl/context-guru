// Tests for staleness: a capability that WAS used, is not used any more, and is still being paid
// for on every request.
//
// WHY THIS EXISTS AS ITS OWN STATE. The report had two: never invoked (offer a removal) and
// invoked (keep). A user working three months in one project — planning, then development, now
// research — has planning skills used heavily in month one and dead since month two. Over all
// history they look used, so they land in `keep` and are never mentioned. Over the last month they
// are pure waste. The verdict that fits them did not exist.
//
// WHY NOT A WINDOW. The obvious fix is "analyse only the last N sessions", and it does not work:
// measured on a real store, session spans here are 0.0h, 1.1h and 99.1h, with one session covering
// four days. N sessions is anywhere from minutes to weeks, and N days is anywhere from two sessions
// to sixty. Any fixed window is a judgement the report cannot justify.
//
// The store already answers the question without one. tool_uses carries last_ts per capability and
// tool_declarations carries ts per declaration, so "used in month one, carried ever since" is a
// fact about the ITEM rather than an artefact of a window somebody picked.
package dash

import (
	"strings"
	"testing"
	"time"
)

// seedUse records an invocation of `name` at `ts`.
func seedUse(t *testing.T, db *DB, session, name string, calls int, ts int64) {
	t.Helper()
	if _, err := db.sql.Exec(`INSERT INTO tool_uses(
		tenant_id, session_id, name, server, skill, calls, first_ts, last_ts)
		VALUES('t1',?,?,'','',?,?,?)`, session, name, calls, ts, ts); err != nil {
		t.Fatal(err)
	}
}

// staleFixture is the user's own scenario, compressed: one capability used long ago and still
// carried, one used just now, one never used at all.
func staleFixture(t *testing.T, db *DB) (now int64) {
	t.Helper()
	now = time.Now().UnixMilli()
	long := now - 90*24*3600*1000 // three months ago
	// Two sessions: an old one that used the planning skill, and a current one that does not.
	seedSessionWithDecl(t, db, "old", long, 4, "PlanningTool", 500)
	seedUse(t, db, "old", "PlanningTool", 12, long)
	// The same tool is STILL DECLARED today, in the current session, and never invoked there.
	seedSessionWithDecl(t, db, "now", now-3600_000, 6, "PlanningTool", 500)
	// A capability used in the current session.
	seedSessionWithDecl(t, db, "now2", now-1800_000, 3, "ActiveTool", 400)
	seedUse(t, db, "now2", "ActiveTool", 7, now-1800_000)
	// And one never invoked anywhere.
	seedSessionWithDecl(t, db, "now3", now-900_000, 3, "GhostTool", 300)
	return now
}

// TestTheInventoryRecordsWhenEachCapabilityWasLastUsed pins the data the verdict rests on. Without
// it the only answerable question is "was this ever used", which is what forces a window.
func TestTheInventoryRecordsWhenEachCapabilityWasLastUsed(t *testing.T) {
	db := openTestDB(t)
	staleFixture(t, db)

	rep, err := db.ToolReportFor(Filter{TenantAll: true}, handPriceFn())
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]ToolStat{}
	for _, s := range append(append([]ToolStat{}, rep.Tools...), rep.Aside...) {
		byName[s.Name] = s
	}

	plan, ok := byName["PlanningTool"]
	if !ok {
		t.Fatalf("PlanningTool is absent: %v", byName)
	}
	if plan.LastUsedTS == 0 {
		t.Error("PlanningTool has no last_used timestamp, though tool_uses records one. Without " +
			"it the report cannot tell a capability that is still in use from one abandoned " +
			"three months ago, and has to ask the reader to pick a window instead")
	}
	if plan.LastCarriedTS == 0 {
		t.Error("PlanningTool has no last_carried timestamp")
	}
	if plan.LastCarriedTS <= plan.LastUsedTS {
		t.Errorf("last_carried (%d) is not after last_used (%d); the fixture declares it today "+
			"and used it three months ago, which is the whole scenario",
			plan.LastCarriedTS, plan.LastUsedTS)
	}
	// Roughly three months of being carried for nothing.
	if days := float64(plan.LastCarriedTS-plan.LastUsedTS) / 86_400_000; days < 80 {
		t.Errorf("dead for %.0f days, want about 90", days)
	}

	if active := byName["ActiveTool"]; active.LastUsedTS == 0 {
		t.Error("ActiveTool was invoked in the current session and carries no last_used")
	}
	// Never invoked means no timestamp at all, not a zero that reads as 1970.
	if ghost := byName["GhostTool"]; ghost.LastUsedTS != 0 {
		t.Errorf("GhostTool was never invoked yet carries last_used=%d", ghost.LastUsedTS)
	}
}

// TestAStaleCapabilityIsItsOwnVerdict is the user-visible half. A capability used once and
// abandoned must not be filed under `keep`: that is the state for something earning its weight,
// and this is the opposite — it is being paid for on every request and has not been touched for
// months.
func TestAStaleCapabilityIsItsOwnVerdict(t *testing.T) {
	db := openTestDB(t)
	staleFixture(t, db)

	rep, err := db.ToolReportFor(Filter{TenantAll: true}, handPriceFn())
	if err != nil {
		t.Fatal(err)
	}
	doc := ShapeInsights(rep, InsightsOptions{})
	byName := map[string]InsightsItem{}
	for _, it := range doc.Items {
		byName[it.Name] = it
	}

	plan := byName["PlanningTool"]
	if plan.Verdict != VerdictStale {
		t.Errorf("PlanningTool verdict = %q, want %q. It was used three months ago and has been "+
			"carried on every request since; filing that under `keep` is why the report could "+
			"not answer the question at all", plan.Verdict, VerdictStale)
	}
	if plan.Why == "" {
		t.Error("a stale verdict with no evidence is an assertion")
	}
	// The evidence has to name WHEN, because that is the fact a reader checks against their own
	// memory — "unused in the last 10 sessions" is not something anybody can verify.
	if !containsDate(plan.Why) {
		t.Errorf("the stale reason %q does not name a date; the whole advantage of this over a "+
			"session window is that the evidence is checkable", plan.Why)
	}

	// Still in use, and never used, keep their existing verdicts.
	if got := byName["ActiveTool"].Verdict; got != VerdictKeep {
		t.Errorf("ActiveTool verdict = %q, want %q", got, VerdictKeep)
	}
	if got := byName["GhostTool"].Verdict; got != VerdictConsider && got != VerdictRemove {
		t.Errorf("GhostTool was never invoked; verdict = %q, want consider or remove", got)
	}

	// A stale item IS removable, which is the point — so it carries a scope and a cost, unlike an
	// item still in use.
	if plan.Scope == "" || plan.Scope == ScopeNone {
		t.Errorf("PlanningTool scope = %q; a stale capability is removable and must say how", plan.Scope)
	}
	if plan.Tokens == 0 {
		t.Error("a stale item carries no weight figure, so the reader cannot see what it costs " +
			"to keep paying for something abandoned")
	}
}

// TestStalenessNeedsRealEvidenceNotJustAnOldTimestamp guards the obvious false positive. A
// capability used in the CURRENT session is not stale however long ago it was first seen, and a
// capability whose last use is recent is not stale either. Without this the verdict would fire on
// anything with a long history.
func TestStalenessNeedsRealEvidenceNotJustAnOldTimestamp(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UnixMilli()
	long := now - 120*24*3600*1000

	// Used continuously: first seen four months ago, last used an hour ago.
	seedSessionWithDecl(t, db, "a", long, 3, "Workhorse", 400)
	seedUse(t, db, "a", "Workhorse", 40, long)
	seedSessionWithDecl(t, db, "b", now-3600_000, 5, "Workhorse", 400)
	seedUse(t, db, "b", "Workhorse", 9, now-3600_000)

	rep, err := db.ToolReportFor(Filter{TenantAll: true}, handPriceFn())
	if err != nil {
		t.Fatal(err)
	}
	doc := ShapeInsights(rep, InsightsOptions{})
	for _, it := range doc.Items {
		if it.Name != "Workhorse" {
			continue
		}
		if it.Verdict == VerdictStale {
			t.Errorf("Workhorse was used an hour ago and is marked stale. Age of FIRST use is "+
				"not evidence of abandonment; only the gap since the LAST use is. why=%q", it.Why)
		}
		if it.Verdict != VerdictKeep {
			t.Errorf("Workhorse verdict = %q, want %q", it.Verdict, VerdictKeep)
		}
	}
}

// containsDate is a crude check that a sentence names a calendar date or a day count, rather than
// a session count. The distinction is the point of this feature: a date is something the reader can
// check against their own memory, and "unused in the last 10 sessions" is not.
func containsDate(s string) bool {
	for _, month := range []string{"Jan", "Feb", "Mar", "Apr", "May", "Jun",
		"Jul", "Aug", "Sep", "Oct", "Nov", "Dec"} {
		if strings.Contains(s, month) {
			return true
		}
	}
	return strings.Contains(s, " day")
}
