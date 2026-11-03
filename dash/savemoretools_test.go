package dash

import "testing"

func smtRows(doc *SaveMoreToolsDoc) map[string]SaveMoreToolsRow {
	out := map[string]SaveMoreToolsRow{}
	for _, r := range doc.Rows {
		out[r.Name] = r
	}
	return out
}

// One row per tool, MCP server and skill, each with its state and reason. A tool Claude Code defers
// reads deferred_client, a tool the history rule defers reads deferred_cg, a listed one removed;
// Context Guru's own are not rows; a skill has no deferral action.
func TestSaveMoreToolsRowsAndStates(t *testing.T) {
	rep := &ToolReport{
		Aside: []ToolStat{
			{Kind: KindTool, Name: "Bash", Builtin: true, Tokens: 900, CarryUSD: 4},
			{Kind: KindTool, Name: "Monitor", Builtin: true, Deferred: true, CarryUSD: 0},
			{Kind: KindTool, Name: "WebFetch", Builtin: true, Tokens: 500, CarryUSD: 2},
			{Kind: KindTool, Name: "context_guru_expand", Tokens: 100, CarryUSD: 1},
			{Kind: KindServerTool, Name: "web_search", CarryUSD: 1},
		},
		Tools: []ToolStat{
			{Kind: KindMCPTool, Name: "mcp__pw__click", Server: "pw", Tokens: 300, LastUsedTS: 7},
			{Kind: KindMCPTool, Name: "mcp__gh__pr", Server: "gh", Deferred: true},
		},
		Servers: []ServerStat{{Server: "pw", Tokens: 300, CarryUSD: 3, CarryPriced: true}, {Server: "gh", CarryUSD: 0.5}},
		Skills:  SkillStat{Skills: []ToolStat{{Kind: KindSkill, Name: "dataviz", CarryUSD: 0.2}}},
	}
	host := SaveMoreToolsHost{SearchOn: true, Keep: []string{"Bash"}, Removed: []string{"mcp__gh"}, Writable: true,
		Explain: func(name string) (bool, string) {
			if name == "WebFetch" || name == "mcp__pw" {
				return true, "declared and not used in your last 3 sessions"
			}
			return false, "used or loaded in your last 3 sessions"
		}}
	doc := buildSaveMoreTools(rep, host, Filter{Since: 1, Until: 2})
	rows := smtRows(doc)

	want := map[string]string{"Bash": StateLoaded, "Monitor": StateDeferredCC, "WebFetch": StateDeferredCG,
		"mcp__pw": StateDeferredCG, "mcp__gh": StateRemoved, "dataviz": StateLoaded}
	if len(rows) != len(want) {
		t.Fatalf("rows = %v, want %d rows", rows, len(want))
	}
	for n, st := range want {
		if rows[n].State != st || rows[n].Reason == "" {
			t.Errorf("%s: state %q reason %q, want %q", n, rows[n].State, rows[n].Reason, st)
		}
	}
	if !rows["Bash"].Kept || len(rows["Bash"].Actions) != 1 || rows["Bash"].Actions[0] != "unkeep:Bash" {
		t.Errorf("Bash = %+v, want kept with an unkeep action", rows["Bash"])
	}
	if a := rows["mcp__pw"].Actions; len(a) != 1 || a[0] != "keep:mcp__pw" {
		t.Errorf("mcp__pw actions = %v", a)
	}
	if rows["mcp__pw"].LastUsedTS != 7 || rows["mcp__pw"].Kind != KindMCPServer {
		t.Errorf("mcp__pw = %+v", rows["mcp__pw"])
	}
	if len(rows["dataviz"].Actions) != 0 {
		t.Errorf("a skill offers %v; deferral does not touch skills", rows["dataviz"].Actions)
	}
	if doc.Rows[0].Name != "Bash" {
		t.Errorf("rows are not sorted by carrying cost: first is %s", doc.Rows[0].Name)
	}
	ids := map[string]bool{}
	for _, o := range doc.Options {
		ids[o.ID] = true
	}
	if !ids["search_off"] || ids["search_on"] || !ids["keep:WebFetch"] || !ids["unkeep:Bash"] {
		t.Errorf("options = %v", doc.Options)
	}
	if offers := doc.SaveMoreToolsOffers(); offers["keep:WebFetch"].(SaveMoreToolsOption).Name != "WebFetch" {
		t.Errorf("offers = %v", offers)
	}
}

// With search off nothing reads as deferred by Context Guru, and the account switch offers search_on.
func TestSaveMoreToolsSearchOff(t *testing.T) {
	rep := &ToolReport{Aside: []ToolStat{{Kind: KindTool, Name: "WebFetch", Builtin: true}}}
	doc := buildSaveMoreTools(rep, SaveMoreToolsHost{Explain: func(string) (bool, string) { return true, "x" }}, Filter{})
	if r := smtRows(doc)["WebFetch"]; r.State != StateLoaded {
		t.Errorf("WebFetch = %+v, want loaded with search off", r)
	}
	if last := doc.Options[len(doc.Options)-1]; last.ID != "search_on" {
		t.Errorf("last option = %+v, want search_on", last)
	}
}
