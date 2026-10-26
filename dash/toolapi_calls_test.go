package dash

import "testing"

// A tool's call count and its last-used time must agree. lastUsedOf reads every session in scope;
// Calls used to count a call only in a session that also declared the tool. So a call in a session
// with no declaration row for the tool (it was deferred, or the session began before the proxy)
// set last_used but left calls at 0, and calls 0 is what files a tool as never used. The report
// then advised removing a tool the user had called.
func TestCallsCountEveryCallInScope(t *testing.T) {
	const name = "mcp__context7__resolve-library-id"
	sessions := map[string]*sessionCost{"s1": {fresh: 1, model: "m"}, "s2": {fresh: 1, model: "m"}}
	// Declared only in s2, which never called it; called only in s1, which never declared it.
	decls := []declRow{{session: "s2", kind: KindMCPTool, name: name, server: "context7", tokens: 9, ts: 10}}
	uses := []useRow{{session: "s1", name: name, calls: 2, lastTS: 20}}

	rep := buildToolReport(sessions, decls, uses, false)

	var tl ToolStat
	for _, st := range rep.Tools {
		if st.Name == name {
			tl = st
		}
	}
	if tl.LastUsedTS != 20 || tl.Calls != 2 {
		t.Errorf("tool calls=%d last_used=%d, want 2 and 20", tl.Calls, tl.LastUsedTS)
	}
	if len(rep.Servers) != 1 || rep.Servers[0].Calls != 2 {
		t.Errorf("servers = %+v, want context7 with 2 calls", rep.Servers)
	}
	// s2 carried it and did not call it: that session's carry is still unused, and it is still not
	// a session that used the tool. Only the call count moves.
	if tl.SessionsUsed != 0 || tl.UnusedReads == 0 {
		t.Errorf("sessions_used=%d unused_reads=%d, want 0 and the s2 carry", tl.SessionsUsed, tl.UnusedReads)
	}
}

// Calls in a session that declared the tool are counted once, not twice.
func TestCallsInADeclaringSessionAreNotDoubleCounted(t *testing.T) {
	const name = "mcp__pw__click"
	sessions := map[string]*sessionCost{"s1": {fresh: 1, model: "m"}}
	decls := []declRow{{session: "s1", kind: KindMCPTool, name: name, server: "pw", tokens: 9, ts: 10}}
	uses := []useRow{{session: "s1", name: name, calls: 3, lastTS: 20}}

	rep := buildToolReport(sessions, decls, uses, false)

	for _, st := range rep.Tools {
		if st.Name == name && (st.Calls != 3 || st.SessionsUsed != 1) {
			t.Errorf("calls=%d sessions_used=%d, want 3 and 1", st.Calls, st.SessionsUsed)
		}
	}
}

// The skills summary agrees with its rows: a skill called only where it had no declaration row
// counts as invoked, and its calls reach the total.
func TestSkillsSummaryCountsACallWithoutADeclaringSession(t *testing.T) {
	sessions := map[string]*sessionCost{"s1": {fresh: 1, model: "m"}, "s2": {fresh: 1, model: "m"}}
	decls := []declRow{
		{session: "s2", kind: KindSkillListing, server: SkillsOK, tokens: 100, ts: 10},
		{session: "s2", kind: KindSkill, name: "dataviz", tokens: 30, ts: 10},
	}
	uses := []useRow{{session: "s1", name: "Skill", skill: "dataviz", calls: 1, lastTS: 20}}

	rep := buildToolReport(sessions, decls, uses, false)

	if rep.Skills.Invoked != 1 || rep.Skills.Calls != 1 {
		t.Errorf("skills invoked=%d calls=%d, want 1 and 1", rep.Skills.Invoked, rep.Skills.Calls)
	}
}

// A server whose tool was called counts that tool as used, even when the call was in a session
// that had no declaration row for it.
func TestServerRollupCountsACalledTool(t *testing.T) {
	const name = "mcp__context7__resolve-library-id"
	sessions := map[string]*sessionCost{"s1": {fresh: 1, model: "m"}, "s2": {fresh: 1, model: "m"}}
	decls := []declRow{{session: "s2", kind: KindMCPTool, name: name, server: "context7", tokens: 9, ts: 10}}
	uses := []useRow{{session: "s1", name: name, calls: 1, lastTS: 20}}

	rep := buildToolReport(sessions, decls, uses, false)

	if len(rep.Servers) != 1 || rep.Servers[0].ToolsUsed != 1 {
		t.Errorf("servers = %+v, want context7 with 1 tool used", rep.Servers)
	}
}

// A called tool is never suggested for removal, whatever its per-session counts say.
func TestSuggestNeverOffersACalledTool(t *testing.T) {
	w := declWindow{sessions: suggestMinSessions, first: 0, last: suggestMinSpan.Milliseconds()}
	if _, ok := suggest(ToolStat{Kind: KindMCPTool, Name: "mcp__a__b", Server: "a", Tokens: 500, Calls: 1}, w); ok {
		t.Error("suggested removing a tool that was called")
	}
}
