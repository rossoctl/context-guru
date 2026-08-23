package dash

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// The fixtures below are hand-built to the shape MEASURED on real captured Claude Code
// bodies (/tmp/cg-runs/capture-*.jsonl): tools are flat {name, description,
// input_schema} elements, and the skills listing is prose inside a {"role":"system"}
// message's <system-reminder>, immediately below an agent-types listing of the
// IDENTICAL `- name: description` shape. See TestCorpusInventory for the same code run
// against the captures themselves.

// agentTypesReminder is the trap: same line shape, different meaning. Anything that
// counts these as skills has produced a wrong inventory, which is the one failure mode
// that could later authorise stripping something real.
const agentTypesReminder = `<system-reminder>
Available agent types for the Agent tool:
- claude: Catch-all for any task. (Tools: *)
- Explore: Read-only search agent. (Tools: All tools except Agent)
</system-reminder>`

const skillsReminder = `<system-reminder>
The following skills are available for use with the Skill tool:

- dataviz: Use this skill whenever you are about to create ANY chart.
- superpowers:brainstorming: You MUST use this before any creative work.
- apps/web:deploy: Directory-scoped skill.
</system-reminder>`

// tool renders one declaration in the measured Anthropic shape.
func tool(name, desc string) string {
	b, _ := json.Marshal(map[string]any{
		"name": name, "description": desc,
		"input_schema": map[string]any{"type": "object", "properties": map[string]any{}},
	})
	return string(b)
}

// ccBody builds an Anthropic-dialect body: declarations, a system-role message carrying
// the reminders, and one assistant turn holding tool calls.
func ccBody(t *testing.T, tools []string, reminders string, calls []map[string]any) []byte {
	t.Helper()
	var blocks []any
	for _, c := range calls {
		blocks = append(blocks, map[string]any{
			"type": "tool_use", "id": "tu_" + fmt.Sprint(c["name"]),
			"name": c["name"], "input": c["input"],
		})
	}
	msgs := []any{
		map[string]any{"role": "user", "content": "go"},
		map[string]any{"role": "system", "content": []any{
			map[string]any{"type": "text", "text": reminders},
		}},
	}
	if blocks != nil {
		msgs = append(msgs, map[string]any{"role": "assistant", "content": blocks})
		msgs = append(msgs, map[string]any{"role": "user", "content": "thanks"})
	}
	m, err := json.Marshal(map[string]any{"model": "claude", "messages": msgs})
	if err != nil {
		t.Fatal(err)
	}
	// Splice the tools array in raw so the fixture keeps the measured element shape.
	return []byte(`{"tools":[` + strings.Join(tools, ",") + `],` + string(m)[1:])
}

func TestSplitMCPName(t *testing.T) {
	// Real names from an MCP-loaded host: the plugin form, and a tool name containing '-'.
	for _, tc := range []struct{ name, server, tool string }{
		{"mcp__plugin_context7_context7__resolve-library-id", "plugin_context7_context7", "resolve-library-id"},
		{"mcp__playwright__browser_take_screenshot", "playwright", "browser_take_screenshot"},
		// A tool name that itself contains the delimiter: non-greedy on the server half,
		// which is the only split the convention permits.
		{"mcp__srv__deep__tool", "srv", "deep__tool"},
	} {
		server, tool, ok := SplitMCPName(tc.name)
		if !ok || server != tc.server || tool != tc.tool {
			t.Errorf("%s -> %q/%q ok=%v, want %q/%q", tc.name, server, tool, ok, tc.server, tc.tool)
		}
	}
	// Not MCP tool declarations: a bare tool, and the truncated prefix forms that appear
	// in permission patterns rather than in a tools array.
	for _, n := range []string{"Bash", "mcp__", "mcp__playwright__", "mcp____x", "notmcp__a__b"} {
		if _, _, ok := SplitMCPName(n); ok {
			t.Errorf("%q was accepted as an MCP tool name", n)
		}
	}
}

func TestScanInventoryClassifiesAndMeasures(t *testing.T) {
	body := ccBody(t, []string{
		tool("Bash", "run a command"),
		tool("Skill", "invoke a skill"),
		tool("mcp__playwright__browser_click", "click"),
		`{"type":"web_search_20260209","name":"web_search"}`,
		`{"type":"mcp_toolset","mcp_server_name":"connector-side"}`,
		`{"name":"deferred","description":"x","defer_loading":true,"input_schema":{}}`,
	}, agentTypesReminder+"\n\n"+skillsReminder, []map[string]any{
		{"name": "Bash", "input": map[string]any{"command": "ls"}},
		{"name": "Skill", "input": map[string]any{"skill": "dataviz"}},
	})

	inv := ScanInventory("anthropic", body)
	if inv == nil {
		t.Fatal("no inventory for a body that declares tools")
	}
	byName := map[string]Decl{}
	for _, d := range inv.Decls {
		byName[d.Kind+":"+d.Name] = d
	}
	for _, want := range []struct{ key, server string }{
		{KindTool + ":Bash", ""},
		{KindMCPTool + ":mcp__playwright__browser_click", "playwright"},
		{KindServerTool + ":web_search", ""},
		{KindServerTool + ":connector-side", ""},
		{KindSkill + ":dataviz", ""},
		{KindSkill + ":superpowers:brainstorming", ""},
		{KindSkill + ":apps/web:deploy", ""},
		{KindSkillListing + ":", SkillsOK},
	} {
		d, ok := byName[want.key]
		if !ok {
			t.Fatalf("missing declaration %q; got %v", want.key, keysOf(byName))
		}
		if d.Server != want.server {
			t.Errorf("%s server = %q, want %q", want.key, d.Server, want.server)
		}
		if want.key != KindSkillListing+":" && d.Tokens <= 0 {
			t.Errorf("%s has no token weight", want.key)
		}
	}
	// The agent-types listing above the skills header must not appear as a skill.
	for _, bad := range []string{KindSkill + ":claude", KindSkill + ":Explore"} {
		if _, ok := byName[bad]; ok {
			t.Errorf("%s was counted as a skill: the header anchor is not holding", bad)
		}
	}
	// defer_loading declarations are advertised but not sent, so charging their schema
	// would double-count tool search.
	if d := byName[KindTool+":deferred"]; d.Tokens != 0 {
		t.Errorf("deferred tool weighed %d tokens, want 0", d.Tokens)
	}
	// The token weight is the real BPE count of the whole element, not len/4.
	if got := byName[KindTool+":Bash"].Tokens; got < 5 || got > 60 {
		t.Errorf("Bash declaration = %d tokens, implausible for the fixture", got)
	}
	used := map[string]Used{}
	for _, u := range inv.Used {
		used[u.Name+"/"+u.Skill] = u
	}
	if u, ok := used["Bash/"]; !ok || u.Calls != 1 {
		t.Errorf("Bash use = %+v", u)
	}
	if _, ok := used["Skill/dataviz"]; !ok {
		t.Errorf("the skill invocation was not recorded: %v", keysOf(used))
	}
	if inv.UseFingerprint == 0 {
		t.Error("no use fingerprint, so resent turns would be counted twice")
	}
}

func keysOf[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestScanInventoryOpenAIDialect proves the second dialect: the name is one level
// deeper, and a call's arguments arrive as a JSON STRING rather than an object.
func TestScanInventoryOpenAIDialect(t *testing.T) {
	body := []byte(`{"tools":[
	  {"type":"function","function":{"name":"Skill","description":"d","parameters":{}}},
	  {"type":"function","function":{"name":"mcp__ctx__query-docs","description":"d","parameters":{}}}],
	  "messages":[{"role":"assistant","tool_calls":[
	    {"id":"c1","type":"function","function":{"name":"Skill","arguments":"{\"skill\":\"dataviz\"}"}},
	    {"id":"c2","type":"function","function":{"name":"mcp__ctx__query-docs","arguments":"{}"}}]}]}`)
	inv := ScanInventory("openai", body)
	if inv == nil {
		t.Fatal("no inventory")
	}
	// The dialect switch has to reach BOTH halves: a declaration read at tools.#.name
	// would be empty here, and a call read from content blocks would be missed.
	var names []string
	for _, d := range inv.Decls {
		names = append(names, d.Kind+":"+d.Name+"/"+d.Server)
	}
	want := []string{KindServerTool + ":Skill/", KindMCPTool + ":mcp__ctx__query-docs/ctx"}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Errorf("declarations = %v, want %v", names, want)
	}
	var used []string
	for _, u := range inv.Used {
		used = append(used, u.Name+"/"+u.Skill)
	}
	if !contains(used, "Skill/dataviz") || !contains(used, "mcp__ctx__query-docs/") {
		t.Errorf("used = %v", used)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// TestSkillListingFailsSafe is the important one. An unreadable listing must produce
// "unknown", never a confident empty inventory — a later filter reading "0 skills
// declared" from a listing it simply could not parse would strip 1,100 tokens of prose
// describing capabilities the model still has.
func TestSkillListingFailsSafe(t *testing.T) {
	moved := `<system-reminder>
The following skills are available for use with the Skill tool:

  * dataviz — a reformatted listing from a future Claude Code
  * artifact-design — no leading "- name:" anywhere
</system-reminder>`
	inv := ScanInventory("anthropic", ccBody(t, []string{tool("Skill", "x")}, moved, nil))
	if inv == nil {
		t.Fatal("no inventory")
	}
	var marker *Decl
	skills := 0
	for i, d := range inv.Decls {
		switch d.Kind {
		case KindSkillListing:
			marker = &inv.Decls[i]
		case KindSkill:
			skills++
		}
	}
	if marker == nil {
		t.Fatal("a listing was present and no marker row was recorded")
	}
	if marker.Server != SkillsUnknown {
		t.Errorf("parse state = %q, want %q", marker.Server, SkillsUnknown)
	}
	if skills != 0 {
		t.Errorf("%d skills invented from an unparseable listing", skills)
	}
	if marker.Tokens <= 0 {
		t.Error("the listing's size is measurable even when its contents are not")
	}
	// And with no listing at all: no marker, no skills, no invention.
	inv = ScanInventory("anthropic", ccBody(t, []string{tool("Bash", "x")}, "<system-reminder>nothing</system-reminder>", nil))
	for _, d := range inv.Decls {
		if d.Kind == KindSkill || d.Kind == KindSkillListing {
			t.Errorf("skills reported for a body carrying no listing: %+v", d)
		}
	}
}

// TestScanInventoryNoTools: an agent that declares no tools has no inventory. It must
// not crash, and it must not create a row that would later read as "declared nothing,
// used nothing" — which is indistinguishable from a fully-efficient session.
func TestScanInventoryNoTools(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","tools":[],"messages":[]}`,
		`{"tools":"not an array","messages":[{"role":"assistant","content":[{"type":"tool_use","name":"Ghost"}]}]}`,
		`{`,
		``,
	} {
		if inv := ScanInventory("anthropic", []byte(body)); inv != nil {
			t.Errorf("phantom inventory for %q: %+v", body, inv)
		}
	}
}

// TestInventoryWriterDedupes pins the two mechanisms that keep this cheap and correct:
// a declaration set is stored once per session however many requests carry it, and a
// resent turn's tool calls are not counted again.
func TestInventoryWriterDedupes(t *testing.T) {
	db := openTestDB(t)
	w := &invWriter{db: db, seen: map[string]*invSession{}}
	inv := ScanInventory("anthropic", ccBody(t,
		[]string{tool("Bash", "b"), tool("Read", "r")}, skillsReminder,
		[]map[string]any{{"name": "Bash", "input": map[string]any{}}}))

	// Three requests carrying the same declarations and the same last tool-using turn:
	// the shape Claude Code actually produces, since it resends the whole transcript.
	batch := []invMsg{
		{tenant: "t1", session: "s1", ts: 1, inv: inv},
		{tenant: "t1", session: "s1", ts: 2, inv: inv},
		{tenant: "t1", session: "s1", ts: 3, inv: inv},
	}
	if err := w.write(batch); err != nil {
		t.Fatal(err)
	}
	decls, uses, err := db.countInventoryRows()
	if err != nil {
		t.Fatal(err)
	}
	// 2 tools + 3 skills + 1 listing marker = 6, once.
	if decls != 6 {
		t.Errorf("declaration rows = %d, want 6 (one set, stored once)", decls)
	}
	if uses != 1 {
		t.Errorf("usage rows = %d, want 1", uses)
	}
	var calls int
	if err := db.sql.QueryRow(`SELECT calls FROM tool_uses WHERE name='Bash'`).Scan(&calls); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("Bash calls = %d, want 1: the resent turn was counted again", calls)
	}
}

// TestInventoryGCFollowsRequests: the tables are session-keyed, so the trigger in
// additiveDDL is what makes retention, eviction and a tenant purge remove the inventory
// too. Without it a purged tenant's tool names would outlive its request rows.
func TestInventoryGCFollowsRequests(t *testing.T) {
	db := openTestDB(t)
	if err := db.insertBatch([]*Event{
		mkEvent(1000, "s1", "m", 100, 90), mkEvent(2000, "s1", "m", 100, 90),
	}); err != nil {
		t.Fatal(err)
	}
	w := &invWriter{db: db, seen: map[string]*invSession{}}
	if err := w.write([]invMsg{{tenant: "", session: "s1", ts: 1,
		inv: ScanInventory("anthropic", ccBody(t, []string{tool("Bash", "b")}, skillsReminder,
			[]map[string]any{{"name": "Bash", "input": map[string]any{}}}))}}); err != nil {
		t.Fatal(err)
	}
	// One of two request rows gone: the session is still live, the inventory stays.
	if _, err := db.sql.Exec(`DELETE FROM requests WHERE ts = 1000`); err != nil {
		t.Fatal(err)
	}
	if d, _, _ := db.countInventoryRows(); d == 0 {
		t.Fatal("inventory dropped while the session still has requests")
	}
	if _, err := db.sql.Exec(`DELETE FROM requests WHERE ts = 2000`); err != nil {
		t.Fatal(err)
	}
	d, u, err := db.countInventoryRows()
	if err != nil {
		t.Fatal(err)
	}
	if d != 0 || u != 0 {
		t.Errorf("inventory rows survived the session: %d declarations, %d uses", d, u)
	}
}

// TestRecordInventoryEndToEnd runs the real path: recorder queue, writer goroutine,
// shutdown drain.
func TestRecordInventoryEndToEnd(t *testing.T) {
	rec, err := NewRecorder(Options{DBPath: filepath.Join(t.TempDir(), "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	inv := ScanInventory("anthropic", ccBody(t, []string{tool("Bash", "b")}, skillsReminder,
		[]map[string]any{{"name": "Bash", "input": map[string]any{}}}))
	rec.RecordInventory("t1", "s1", 1000, inv, true)
	rec.RecordInventory("t1", "", 1000, inv, true) // no session: nothing to key on, dropped
	rec.RecordInventory("t1", "s2", 1000, nil, true)
	db := rec.DB()
	deadline := time.Now().Add(5 * time.Second)
	for {
		d, _, err := db.countInventoryRows()
		if err != nil {
			t.Fatal(err)
		}
		if d > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("nothing was written")
		}
		time.Sleep(20 * time.Millisecond)
	}
	var sessions int
	if err := db.sql.QueryRow(`SELECT COUNT(DISTINCT session_id) FROM tool_declarations`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 1 {
		t.Errorf("sessions with inventory = %d, want 1", sessions)
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// Read side
// ---------------------------------------------------------------------------

// seedReport writes two sessions: one that declares four tools and uses one, and one
// with request rows but NO inventory (every production row today looks like this).
func seedReport(t *testing.T) *DB {
	t.Helper()
	db := openTestDB(t)
	var evs []*Event
	for i := 0; i < 10; i++ { // 10 requests, all cache hits
		e := mkEvent(int64(1000+i), "used", "claude", 100, 90)
		e.TenantID, e.Tools = "t1", 4
		evs = append(evs, e)
	}
	e := mkEvent(2000, "nocapture", "claude", 100, 90)
	e.TenantID, e.Tools = "t1", 4
	evs = append(evs, e)
	other := mkEvent(3000, "someone-else", "claude", 100, 90)
	other.TenantID, other.Tools = "t2", 4
	evs = append(evs, other)
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}
	w := &invWriter{db: db, seen: map[string]*invSession{}}
	msgs := []invMsg{{tenant: "t1", session: "used", ts: 1000, inv: &Inventory{
		Digest: "d1",
		Decls: []Decl{
			{Kind: KindTool, Name: "Bash", Tokens: 100},
			{Kind: KindTool, Name: "Workflow", Tokens: 5000},
			{Kind: KindMCPTool, Name: "mcp__pw__click", Server: "pw", Tokens: 400},
			{Kind: KindSkillListing, Server: SkillsOK, Tokens: 1000},
			{Kind: KindSkill, Name: "dataviz", Tokens: 300},
		},
		Used:           []Used{{Name: "Bash", Calls: 7}},
		UseFingerprint: 42,
	}}}
	msgs = append(msgs, invMsg{tenant: "t2", session: "someone-else", ts: 3000, inv: &Inventory{
		Digest: "d2",
		Decls:  []Decl{{Kind: KindTool, Name: "SecretTool", Tokens: 900}},
	}})
	if err := w.write(msgs); err != nil {
		t.Fatal(err)
	}
	return db
}

func flatPrice(string) (modelinfo.Price, bool) {
	// $1/MTok input, cache read at 0.1x and creation at 1.25x — the shape prices.yaml uses.
	return modelinfo.Price{Input: 1e-6, CacheRead: 1e-7, CacheWrite: 1.25e-6}, true
}

func TestToolReportDiffsDeclaredAgainstUsed(t *testing.T) {
	db := seedReport(t)
	rep, err := db.ToolReportFor(Filter{Tenant: "t1"}, flatPrice)
	if err != nil {
		t.Fatal(err)
	}
	// Coverage: a session whose requests predate capture is NOT a session with nothing
	// unused. This is the whole of requirement "backfill honestly".
	if rep.Coverage.Sessions != 2 || rep.Coverage.Captured != 1 || rep.Coverage.NotCaptured != 1 {
		t.Errorf("coverage = %+v, want 2 sessions / 1 captured / 1 not captured", rep.Coverage)
	}
	byName := map[string]ToolStat{}
	for _, s := range rep.Tools {
		byName[s.Name] = s
	}
	aside := map[string]ToolStat{}
	for _, s := range rep.Aside {
		aside[s.Name] = s
	}
	// Bash is one of Claude Code's own, so it is reported in Aside and NOWHERE in the main set.
	// Both halves are asserted: the group has to be visible (it is most of the prompt) and it has
	// to be outside the figures the page invites the reader to act on.
	if s := aside["Bash"]; s.SessionsUsed != 1 || s.Calls != 7 || s.UnusedReads != 0 || !s.Builtin {
		t.Errorf("Bash in Aside = %+v, want a used built-in with no waste", s)
	}
	if _, ok := byName["Bash"]; ok {
		t.Error("Bash is in the main tool list; the built-in split is not applied")
	}
	// Workflow is a client tool that is NOT one of Claude Code's, so it stays actionable. This is
	// the boundary the split is drawn on, and getting it wrong the other way takes every
	// SDK application's removable declarations off the page.
	if _, ok := aside["Workflow"]; ok {
		t.Error("Workflow is in Aside; only built-ins and provider tools belong there")
	}
	// Workflow: declared, never called, 10 requests re-read it.
	wf := byName["Workflow"]
	if wf.SessionsUsed != 0 || wf.UnusedReads != 50_000 {
		t.Errorf("Workflow = %+v, want 0 uses and 5000x10 unused reads", wf)
	}
	// Priced at the tier billed: mkEvent rows all have cache_read>0, so 50,000 cache-read
	// tokens at $0.1/MTok = $0.005.
	if got := wf.UnusedUSD; got < 0.0049 || got > 0.0051 {
		t.Errorf("Workflow unused USD = %g, want ~0.005", got)
	}
	// Declared weight per captured session over the CONTROLLABLE set: 5000+400+1000+300 = 6700.
	// Bash's 100 is not in it and is reported separately in AsideTokens, so the two still add up
	// to the 6800 the session actually carried — the split moves weight between figures, it never
	// loses any.
	if rep.Totals.DeclaredTokens != 6700 || rep.Totals.UnusedTokens != 6700 {
		t.Errorf("totals = %+v, want 6700 declared / 6700 unused per session", rep.Totals)
	}
	if rep.Totals.AsideTokens != 100 || rep.Totals.AsideSetTokens != 100 {
		t.Errorf("aside weight = %d/%d, want 100/100 (Bash)",
			rep.Totals.AsideTokens, rep.Totals.AsideSetTokens)
	}
	if got := rep.Totals.DeclaredTokens + rep.Totals.AsideTokens; got != 6800 {
		t.Errorf("controllable + aside = %d, want the 6800 the session declared", got)
	}
	// The share denominator is still the WHOLE set, aside included, so a share is a share of the
	// real prompt: the skills LISTING (1000) + Workflow (5000) + the MCP tool (400) + Bash (100)
	// = 6500. The dataviz row's 300 is deliberately absent — a skill's weight is its ENTRY inside
	// that listing, so adding the rows to the listing counts the same bytes twice. That is why
	// this figure and DeclaredTokens above differ, and it predates the built-in split.
	if rep.Totals.DeclaredSetTokens != 6500 {
		t.Errorf("declared set = %d, want 6500 (listing + every tool row, aside included)",
			rep.Totals.DeclaredSetTokens)
	}
	if rep.Totals.DeclaredSetTokens-rep.Totals.AsideSetTokens != 6400 {
		t.Errorf("controllable share of the set = %d, want 6400",
			rep.Totals.DeclaredSetTokens-rep.Totals.AsideSetTokens)
	}
	if rep.Totals.UnusedReads != 67_000 {
		t.Errorf("unused reads = %d, want 67000", rep.Totals.UnusedReads)
	}
	if rep.Totals.RequestsPerSession != 5.5 {
		t.Errorf("requests per session = %v, want 5.5 (11 requests / 2 sessions)", rep.Totals.RequestsPerSession)
	}
	// Skills: declared, never invoked, so the listing's own weight is waste too.
	if rep.Skills.State != SkillsOK || rep.Skills.Declared != 1 || rep.Skills.Invoked != 0 {
		t.Errorf("skills = %+v", rep.Skills)
	}
	if rep.Skills.UnusedListingReads != 10_000 {
		t.Errorf("unused listing reads = %d, want 10000", rep.Skills.UnusedListingReads)
	}
	// MCP rollup, per server, because a server is the unit a user adds or removes.
	if len(rep.Servers) != 1 || rep.Servers[0].Server != "pw" || rep.Servers[0].UnusedReads != 4000 {
		t.Errorf("servers = %+v", rep.Servers)
	}
	// Tenant scoping: t1 must not see t2's declarations, in any field.
	for _, s := range rep.Tools {
		if s.Name == "SecretTool" {
			t.Fatal("another tenant's tool leaked into this report")
		}
	}
}

// TestToolReportUnpricedIsNotFree: a model with no known rates leaves the tokens
// visible and the dollars absent, never a zero that reads as "this cost nothing".
func TestToolReportUnpricedIsNotFree(t *testing.T) {
	db := seedReport(t)
	rep, err := db.ToolReportFor(Filter{Tenant: "t1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Totals.UnusedReads == 0 {
		t.Fatal("no token figure")
	}
	if rep.Totals.Priced || rep.Totals.UnusedUSD != 0 {
		t.Errorf("totals = %+v, want priced=false with no dollar claim", rep.Totals)
	}
	if rep.Coverage.UnpricedSessions != 1 {
		t.Errorf("unpriced sessions = %d, want 1", rep.Coverage.UnpricedSessions)
	}
}

// TestToolAPIScope: the endpoint is tenant-scoped through the same helper every other
// data route uses, so one account cannot read another's inventory.
func TestToolAPIScope(t *testing.T) {
	rec, err := NewRecorder(Options{DBPath: filepath.Join(t.TempDir(), "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	// Reuse the seeded shape on the recorder's own database.
	db := rec.DB()
	e := mkEvent(1000, "used", "claude", 100, 90)
	e.TenantID, e.Tools = "t1", 1
	if err := db.insertBatch([]*Event{e}); err != nil {
		t.Fatal(err)
	}
	w := &invWriter{db: db, seen: map[string]*invSession{}}
	if err := w.write([]invMsg{{tenant: "t1", session: "used", ts: 1000, inv: &Inventory{
		Digest: "d1", Decls: []Decl{{Kind: KindTool, Name: "Workflow", Tokens: 5000}},
	}}}); err != nil {
		t.Fatal(err)
	}
	api := NewAPI(rec)
	api.SetAuth(func(r *http.Request) (Principal, bool) {
		switch r.Header.Get("X-Who") {
		case "t1":
			return Principal{TenantID: "t1"}, true
		case "t2":
			return Principal{TenantID: "t2"}, true
		}
		return Principal{}, false
	})
	for _, tc := range []struct {
		who  string
		code int
		see  bool
	}{{"t1", 200, true}, {"t2", 200, false}, {"", 401, false}} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/tools", nil)
		if tc.who != "" {
			req.Header.Set("X-Who", tc.who)
		}
		api.tools(rr, req)
		if rr.Code != tc.code {
			t.Fatalf("%q: status %d, want %d", tc.who, rr.Code, tc.code)
		}
		if tc.code != 200 {
			continue
		}
		var rep ToolReport
		if err := json.Unmarshal(rr.Body.Bytes(), &rep); err != nil {
			t.Fatal(err)
		}
		if saw := len(rep.Tools) > 0; saw != tc.see {
			t.Errorf("%q saw tools = %v, want %v (%s)", tc.who, saw, tc.see, rr.Body)
		}
	}
	// Every route this feature mounts declares its scope, the same rule api.go's table
	// enforces for the rest of the surface.
	for _, rt := range api.toolRoutes() {
		if rt.class != scopeTenant {
			t.Errorf("%s is not tenant-scoped", rt.pattern)
		}
	}
}

// A SESSION ID IS CLIENT-SUPPLIED, so two accounts can present the same one — by accident or
// deliberately. Every key in this feature must therefore lead with tenant_id. Without it the
// three failures below are all reachable by an account that guesses or reuses another's
// session id, and none of them surfaces as an error:
//
//   - tool_uses upserts `calls = calls + excluded.calls`, so one account's invocations are
//     ADDED to another account's row: a cross-account write, and the writer's own usage is
//     recorded under the wrong tenant.
//   - tool_declarations inserts ON CONFLICT DO NOTHING, so the second account's declarations
//     are silently DISCARDED and its inventory reads as never captured.
//   - the GC trigger deletes by session, so a purge would take the other account's rows with
//     it — or, because the WHEN clause still sees the other account's requests, would leave
//     the purged account's tool names on disk.
//
// This is the shape of the known archived_sessions.session_id defect, which is why it has a
// test of its own rather than a comment.
func TestInventoryKeysAreTenantScoped(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const shared = "same-session-id"
	decls := []Decl{{Kind: KindTool, Name: "Read", Tokens: 100}}
	write := func(tenant string, calls int, fp uint64) {
		t.Helper()
		w := &invWriter{db: db, seen: map[string]*invSession{}}
		if err := w.write([]invMsg{{tenant: tenant, session: shared, ts: 1, inv: &Inventory{
			Digest: "same-digest", Decls: decls,
			Used: []Used{{Name: "Read", Calls: calls}}, UseFingerprint: fp,
		}}}); err != nil {
			t.Fatal(err)
		}
	}
	write("t1", 3, 7)
	write("t2", 500, 9)

	for tenant, want := range map[string]int{"t1": 3, "t2": 500} {
		var calls int
		if err := db.sql.QueryRow(`SELECT calls FROM tool_uses
			WHERE tenant_id = ? AND session_id = ?`, tenant, shared).Scan(&calls); err != nil {
			t.Fatalf("%s has no tool_uses row of its own: %v", tenant, err)
		}
		if calls != want {
			t.Errorf("%s calls = %d, want %d: the upsert crossed accounts", tenant, calls, want)
		}
		var n int
		if err := db.sql.QueryRow(`SELECT COUNT(*) FROM tool_declarations
			WHERE tenant_id = ? AND session_id = ?`, tenant, shared).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != len(decls) {
			t.Errorf("%s has %d declaration rows, want %d: its inventory was discarded as a "+
				"conflict with the other account's", tenant, n, len(decls))
		}
	}

	// The GC trigger removes the purged account's rows and only those, even though the other
	// account still holds request rows for the same session id.
	if _, err := db.sql.Exec(`INSERT INTO requests(ts, tenant_id, session_id)
		VALUES (1,'t1',?), (2,'t2',?)`, shared, shared); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`DELETE FROM requests WHERE tenant_id = 't2'`); err != nil {
		t.Fatal(err)
	}
	var gone, kept int
	if err := db.sql.QueryRow(`SELECT
		(SELECT COUNT(*) FROM tool_declarations WHERE tenant_id='t2'),
		(SELECT COUNT(*) FROM tool_declarations WHERE tenant_id='t1')`).Scan(&gone, &kept); err != nil {
		t.Fatal(err)
	}
	if gone != 0 {
		t.Errorf("a purged account kept %d declaration rows: its tool names outlived its "+
			"request rows because another account shares the session id", gone)
	}
	if kept == 0 {
		t.Error("the purge deleted the OTHER account's inventory")
	}
}

// maxDeclSet bounds a cost the CALLER chooses: the scan BPE-tokenizes every element of
// `tools` on the request goroutine, and a body may be 32 MiB (proxy.maxRequestBytes).
// Measured cold: 187 ms per MB, 2.05 s at 16 MB — per DISTINCT set, so varying the set on
// every request defeats the memo and pays it every time. Above the bound there is no
// inventory, which the report renders as "not captured".
func TestOversizeDeclarationSetIsNotScanned(t *testing.T) {
	big := strings.Repeat("x", maxDeclSet)
	body := []byte(`{"tools":[{"name":"Read","description":"` + big + `"}]}`)
	if inv := ScanInventory("anthropic", body); inv != nil {
		t.Fatalf("scanned a %d-byte declaration set: the per-request cost is caller-chosen",
			len(body))
	}
	// The real shape is unaffected.
	if inv := ScanInventory("anthropic", []byte(`{"tools":[{"name":"Read"}]}`)); inv == nil {
		t.Fatal("a normal catalogue is no longer scanned")
	}
}

// The design basis for storing this table under tenant scoping rather than under the
// content-capture consent flag is that it holds IDENTIFIERS. A skill name is charset-checked
// by skillEntryName, but a tool or MCP-server name is read straight off `tools[].name` before
// the provider has validated anything — so the bound and the credential scrub are what make
// the claim true rather than a hope about well-behaved clients.
func TestDeclarationNamesAreIdentifiersNotContent(t *testing.T) {
	secret := "sk-ant-api03-" + strings.Repeat("Zt7QwLm4", 4)
	sentence := strings.Repeat("a whole sentence of user text ", 20)
	body := []byte(`{"tools":[
		{"name":` + quote(t, secret) + `},
		{"name":` + quote(t, sentence) + `},
		{"name":` + quote(t, "mcp__"+sentence+"__x") + `}],
		"messages":[{"role":"assistant","content":[
			{"type":"tool_use","name":` + quote(t, secret) + `}]}]}`)
	inv := ScanInventory("anthropic", body)
	if inv == nil {
		t.Fatal("no inventory")
	}
	for _, d := range inv.Decls {
		if len(d.Name) > maxDeclName || len(d.Server) > maxDeclName {
			t.Errorf("unbounded name/server stored: %d/%d bytes", len(d.Name), len(d.Server))
		}
		if strings.Contains(d.Name, secret) || strings.Contains(d.Server, secret) {
			t.Error("a credential-shaped declaration name reached the row unscrubbed")
		}
	}
	for _, u := range inv.Used {
		if len(u.Name) > maxDeclName {
			t.Errorf("unbounded used name stored: %d bytes", len(u.Name))
		}
		if strings.Contains(u.Name, secret) {
			t.Error("a credential-shaped tool_use name reached the row unscrubbed")
		}
	}
	// A real name is untouched, or the inventory no longer matches what the filter removes.
	inv = ScanInventory("anthropic", []byte(`{"tools":[{"name":"mcp__github__create_issue"}]}`))
	if inv == nil || inv.Decls[0].Name != "mcp__github__create_issue" ||
		inv.Decls[0].Server != "github" {
		t.Fatalf("a legitimate name was altered: %+v", inv)
	}
}

func quote(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The NEGATIVE is memoized too. A client sending "system": [] — or blocks with no text field
// — sends the same empty array on every request of the session, and returning early on empty
// text without storing anything made every one of them pay the gjson walk on the request
// goroutine, on a function whose doc promises one hash per request on a hit.
func TestScanSystemMemoizesTheEmptyCase(t *testing.T) {
	body := []byte(`{"system":[],"model":"claude","messages":[]}`)
	sysMu.Lock()
	sysCache, sysBytes = map[uint64]*SystemPrompt{}, 0
	sysMu.Unlock()
	if sp := scanSystem(body); sp != nil {
		t.Fatalf("an empty system array scanned as a prompt: %+v", sp)
	}
	sysMu.Lock()
	n := len(sysCache)
	sysMu.Unlock()
	if n != 1 {
		t.Errorf("sysCache holds %d entries after an empty system prompt, want 1 — the walk "+
			"is repeated on every request", n)
	}
	// And the stored nil reads back as a hit, not as a fresh miss returning nil by accident.
	if sp := scanSystem(body); sp != nil {
		t.Errorf("the memoized negative came back as %+v", sp)
	}
}
