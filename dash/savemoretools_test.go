package dash

import (
	"strconv"
	"strings"
	"testing"
)

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
			{Kind: KindMCPTool, Name: "mcp__gh__pr", Server: "gh"},
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
	if a := rows["mcp__pw"].Actions; len(a) != 2 || a[0] != "keep:mcp__pw" || a[1] != "defer:mcp__pw" {
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
	// #878: always defer is offered now, except for a never-defer tool (Bash), one Claude Code defers
	// itself (Monitor) and one the proxy does not send (mcp__gh).
	if len(doc.NotYet) != 0 || !ids["defer:WebFetch"] || ids["defer:Bash"] || ids["defer:Monitor"] || ids["defer:mcp__gh"] {
		t.Errorf("not_yet = %+v, options = %v", doc.NotYet, doc.Options)
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

// Reviewer notes on #877 round 1. A server reads deferred by Claude Code only when all its tools are,
// a client-deferred tool on the removal list is still client-deferred (the filter cannot remove it),
// and more than the limit of rows is cut to the costliest.
func TestSaveMoreToolsServerAndRemovedEdges(t *testing.T) {
	rep := &ToolReport{
		Aside: []ToolStat{{Kind: KindTool, Name: "Monitor", Builtin: true, Deferred: true}},
		Tools: []ToolStat{
			{Kind: KindMCPTool, Name: "mcp__half__a", Server: "half", Deferred: true},
			{Kind: KindMCPTool, Name: "mcp__half__b", Server: "half"},
			{Kind: KindMCPTool, Name: "mcp__all__a", Server: "all", Deferred: true},
		},
		Servers: []ServerStat{{Server: "half"}, {Server: "all"}},
	}
	rows := smtRows(buildSaveMoreTools(rep, SaveMoreToolsHost{Removed: []string{"Monitor"}}, Filter{}))
	if rows["mcp__half"].State != StateLoaded || rows["mcp__all"].State != StateDeferredCC {
		t.Errorf("half = %s, all = %s; want loaded and deferred_client", rows["mcp__half"].State, rows["mcp__all"].State)
	}
	if rows["Monitor"].State != StateDeferredCC {
		t.Errorf("Monitor = %+v, want deferred_client although it is on the removal list", rows["Monitor"])
	}

	many := &ToolReport{}
	for i := 0; i < saveMoreToolsLimit+3; i++ {
		many.Aside = append(many.Aside, ToolStat{Kind: KindTool, Name: "T" + string(rune('A'+i%26)) + strconv.Itoa(i), CarryUSD: float64(i)})
	}
	doc := buildSaveMoreTools(many, SaveMoreToolsHost{}, Filter{})
	if len(doc.Rows) != saveMoreToolsLimit || doc.Truncated != 3 || doc.Rows[len(doc.Rows)-1].CarryUSD != 3 {
		t.Errorf("rows %d, truncated %d, cheapest kept %v; want %d, 3, 3", len(doc.Rows), doc.Truncated,
			doc.Rows[len(doc.Rows)-1].CarryUSD, saveMoreToolsLimit)
	}
}

// The always-defer list (#878): a listed row reads deferred by Context Guru without history, offers
// undefer, and the keep list wins over it.
func TestSaveMoreToolsAlwaysDefer(t *testing.T) {
	rep := &ToolReport{
		Aside: []ToolStat{
			{Kind: KindTool, Name: "WebFetch", Builtin: true, Tokens: 500},
			{Kind: KindTool, Name: "Monitor", Builtin: true, Tokens: 500},
			{Kind: KindTool, Name: "Bash", Builtin: true, Tokens: 900},
		},
		Tools:   []ToolStat{{Kind: KindMCPTool, Name: "mcp__pw__click", Server: "pw", Tokens: 300}},
		Servers: []ServerStat{{Server: "pw", Tokens: 300}},
	}
	host := SaveMoreToolsHost{SearchOn: true, Writable: true, Keep: []string{"Monitor"},
		AlwaysDefer: []string{"mcp__pw", "WebFetch", "Monitor", "Bash"}}
	doc := buildSaveMoreTools(rep, host, Filter{})
	rows := smtRows(doc)
	for _, n := range []string{"WebFetch", "mcp__pw"} {
		r := rows[n]
		if r.State != StateDeferredCG || !r.AlwaysDeferred || r.Actions[len(r.Actions)-1] != "undefer:"+n ||
			!strings.Contains(r.Reason, "always-defer") {
			t.Errorf("%s = %+v, want deferred by CG from the always-defer list, with undefer", n, r)
		}
	}
	if r := rows["Monitor"]; r.State != StateLoaded {
		t.Errorf("Monitor = %+v, want loaded: the keep list wins", r)
	}
	if r := rows["Bash"]; r.State != StateLoaded {
		t.Errorf("Bash = %+v, want loaded: a never-defer tool", r)
	}
	// A never-defer tool that is not kept and not listed offers no defer.
	plain := smtRows(buildSaveMoreTools(rep, SaveMoreToolsHost{SearchOn: true, Writable: true}, Filter{}))
	for _, a := range plain["Bash"].Actions {
		if a == "defer:Bash" {
			t.Errorf("Bash offers %v; a never-defer tool cannot be always deferred", plain["Bash"].Actions)
		}
	}
	if strings.Join(doc.AlwaysDefer, ",") != "Bash,Monitor,WebFetch,mcp__pw" {
		t.Errorf("always_defer = %v", doc.AlwaysDefer)
	}
	labels := map[string]string{}
	for _, o := range doc.Options {
		labels[o.ID] = o.Action + "|" + o.Name
	}
	if labels["undefer:mcp__pw"] != "undefer|mcp__pw" || labels["undefer:WebFetch"] != "undefer|WebFetch" {
		t.Errorf("options = %v", doc.Options)
	}
	// With search off, nothing is deferred, list or not.
	host.SearchOn = false
	if r := smtRows(buildSaveMoreTools(rep, host, Filter{}))["WebFetch"]; r.State != StateLoaded {
		t.Errorf("search off: WebFetch = %+v, want loaded", r)
	}
}
