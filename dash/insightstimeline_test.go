// Tests for the per-item timeline: when each capability was CARRIED and when it was USED, bucketed
// so a page can draw the activity chart.
//
// WHY A PICTURE AND NOT A VERDICT. The `stale` verdict answers "is this abandoned" with a 14-day
// threshold I had to invent, and a threshold is a judgement the report cannot defend. A timeline
// needs none: a block of invocations at the left and a flat line since IS the answer, and the
// reader decides what counts as abandoned for their own work. It also surfaces the finding the
// table buries — on real data the one stale item sorted 26th of 26 inside a collapsed fold,
// because it was the cheapest, so the most certain finding was the least visible.
//
// THE RESOLUTION IS WHAT THE STORE HAS, not what would be nicest. tool_uses is one row per
// (session, name) with first_ts/last_ts and an accumulated call count — 961 Bash calls collapse
// into a single row — so there is no per-invocation timestamp and never was. A session's span is
// the finest grain available, and a day bucket is the coarsest that still shows a phase change.
package dash

import (
	"testing"
	"time"
)

// TestTheTimelineBucketsUseAndCarriageByDay pins the shape the chart needs: two series over the
// same buckets, so the gap between them is drawable without the page doing arithmetic.
func TestTheTimelineBucketsUseAndCarriageByDay(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UnixMilli()
	day := int64(24 * 3600 * 1000)

	// Used on day -9 and day -8, carried every day since: the user's planning-phase scenario in
	// miniature.
	seedSessionWithDecl(t, db, "s1", now-9*day, 3, "PlanTool", 500)
	seedUse(t, db, "s1", "PlanTool", 11, now-9*day)
	seedSessionWithDecl(t, db, "s2", now-8*day, 2, "PlanTool", 500)
	seedUse(t, db, "s2", "PlanTool", 4, now-8*day)
	for d := int64(5); d >= 1; d-- {
		seedSessionWithDecl(t, db, "carry"+string(rune('a'+d)), now-d*day, 2, "PlanTool", 500)
	}

	tl, err := db.ToolTimeline(Filter{TenantAll: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	item, ok := tl.Items["PlanTool"]
	if !ok {
		t.Fatalf("PlanTool absent from the timeline: %v", tl.Items)
	}

	// Both series cover the SAME buckets, in the same order, so index i means the same day in
	// each. A page that had to align two differently-bucketed series would be doing the one job
	// this endpoint exists to do for it.
	if len(item.Calls) != len(tl.Buckets) || len(item.Carried) != len(tl.Buckets) {
		t.Fatalf("series lengths %d calls / %d carried against %d buckets; they must be aligned "+
			"or the page cannot draw them on one axis",
			len(item.Calls), len(item.Carried), len(tl.Buckets))
	}
	if len(tl.Buckets) < 9 {
		t.Errorf("only %d bucket(s) for 9 days of history", len(tl.Buckets))
	}

	// Calls land in the two early buckets and nowhere else.
	var callDays, carryDays int
	var lastCallIdx, lastCarryIdx = -1, -1
	for i := range tl.Buckets {
		if item.Calls[i] > 0 {
			callDays++
			lastCallIdx = i
		}
		if item.Carried[i] > 0 {
			carryDays++
			lastCarryIdx = i
		}
	}
	if callDays != 2 {
		t.Errorf("calls appear in %d bucket(s), want 2 — it was invoked on exactly two days", callDays)
	}
	if carryDays < 7 {
		t.Errorf("carried in %d bucket(s), want at least 7 — it was declared on two early days "+
			"and five recent ones", carryDays)
	}
	// THE WHOLE POINT: carriage continues after use stops, and the gap is visible in the data
	// rather than needing a threshold to name it.
	if lastCarryIdx <= lastCallIdx {
		t.Errorf("last carried bucket (%d) is not after the last call bucket (%d); the shape this "+
			"draws — used early, carried since — is the finding", lastCarryIdx, lastCallIdx)
	}
}

// TestTheTimelineNeverInventsResolutionItDoesNotHave guards the temptation to fake precision.
// tool_uses holds one accumulated row per session, so a session's calls cannot be spread across
// the days it spanned: doing so would draw a shape the store never recorded.
func TestTheTimelineNeverInventsResolutionItDoesNotHave(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UnixMilli()
	day := int64(24 * 3600 * 1000)

	// One session spanning four days with 100 calls, recorded as a single accumulated row. On a
	// real store this is the normal case: 961 Bash calls in one 101-hour session.
	//
	// Its requests are spread across those four days, because a real session's are: an earlier
	// version of this fixture put every request at one timestamp, which made the axis one day wide
	// while the use landed three days later — outside it. The implementation was right and the
	// fixture described a session that cannot occur.
	for d := int64(4); d >= 1; d-- {
		insertReq(t, db, &Event{
			TS: now - d*day, SessionID: "long", Model: "m1", TenantID: "t1",
			TokensBefore: 100, TokensAfter: 100, Meta: Meta{Tools: 1},
			CacheRead: 100, TokenAccounting: AccountingComplete,
		})
	}
	if _, err := db.sql.Exec(`INSERT INTO tool_declarations(
		tenant_id, session_id, digest, kind, name, server, tokens, ts)
		VALUES('t1','long','d1',?,'Workhorse','',400,?)`, KindTool, now-4*day); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`INSERT INTO tool_uses(
		tenant_id, session_id, name, server, skill, calls, first_ts, last_ts)
		VALUES('t1','long','Workhorse','','',100,?,?)`, now-4*day, now-1*day); err != nil {
		t.Fatal(err)
	}

	tl, err := db.ToolTimeline(Filter{TenantAll: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	item := tl.Items["Workhorse"]
	var total, days int
	for _, c := range item.Calls {
		total += c
		if c > 0 {
			days++
		}
	}
	if total != 100 {
		t.Errorf("calls sum to %d, want 100: a bucketing that loses or duplicates invocations "+
			"makes every bar on the page wrong", total)
	}
	if days != 1 {
		t.Errorf("100 calls were spread over %d buckets. The store records ONE accumulated row "+
			"for this session, so it does not know which day each call fell on — spreading them "+
			"draws a shape nobody measured", days)
	}
}

// TestTheTimelineWindowsToRecentDaysWhenAsked keeps a three-month history from forcing 90 bars
// into the chart, while leaving the default unbounded: the whole value of this view is seeing a
// phase change, and a default window would hide the phase before it.
func TestTheTimelineWindowsToRecentDaysWhenAsked(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UnixMilli()
	day := int64(24 * 3600 * 1000)
	for d := int64(40); d >= 1; d -= 2 {
		seedSessionWithDecl(t, db, "s"+string(rune('a'+d%26))+string(rune('a'+d/26)),
			now-d*day, 2, "Tool", 300)
	}

	all, err := db.ToolTimeline(Filter{TenantAll: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	recent, err := db.ToolTimeline(Filter{TenantAll: true}, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Buckets) <= len(recent.Buckets) {
		t.Errorf("unwindowed gave %d bucket(s) and days=7 gave %d; the window must narrow it",
			len(all.Buckets), len(recent.Buckets))
	}
	if len(recent.Buckets) > 8 {
		t.Errorf("days=7 produced %d buckets", len(recent.Buckets))
	}
}
