package tooldefer

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/internal/deferred"
	"github.com/rossoctl/context-guru/internal/tokens"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
)

// decl is a tool declaration well over MinTokens.
func decl(name string) string {
	return fmt.Sprintf(`{"name":%q,"description":"Does the %s job. %s","input_schema":{"type":"object","properties":{"x":{"type":"string","description":"%s"}}}}`,
		name, name, strings.Repeat("Detailed usage notes for this tool. ", 20), strings.Repeat("param doc ", 30))
}

func smallDecl(name string) string {
	return fmt.Sprintf(`{"name":%q,"description":"tiny","input_schema":{"type":"object"}}`, name)
}

// body builds a Claude Code shaped request: user turn, the environment system message at
// messages[1], then extra messages.
func body(tools []string, extra string, msgs ...string) []byte {
	m := `{"role":"user","content":[{"type":"text","text":"hi"}]},{"role":"system","content":"# Environment\nPrimary working directory: /w"}`
	for _, x := range msgs {
		m += "," + x
	}
	return []byte(`{"model":"claude-sonnet-5-5","tools":[` + strings.Join(tools, ",") + `],` + extra + `"messages":[` + m + `]}`)
}

// show is an Outcome without its body, for failure messages.
func show(o Outcome) string {
	return fmt.Sprintf("changed=%v deferred=%v why=%q decided=%v rebuilt=%v probe=%v tools=%s",
		o.Changed, o.Deferred, o.Why, o.Decided, o.Rebuilt, o.NeedProbe, names(o.Body))
}

func names(b []byte) string {
	var out []string
	gjson.GetBytes(b, "tools").ForEach(func(_, t gjson.Result) bool {
		n := t.Get("name").String()
		if t.Get("defer_loading").Bool() {
			n += "*"
		}
		out = append(out, n)
		return true
	})
	return strings.Join(out, ",")
}

// hist is an in-memory History for the rule tests.
type hist struct {
	sessions []Session
	project  map[string]bool // tools the project used
}

func (h hist) UserSessions(_, _ string, n int) []Session {
	if len(h.sessions) > n {
		return h.sessions[:n]
	}
	return h.sessions
}
func (h hist) ProjectUsed(_, _, tool, _ string, _ int) bool { return inSet(h.project, tool) }

func sess(declared []string, used ...string) Session {
	s := Session{Declared: map[string]bool{}, Used: map[string]bool{}}
	for _, d := range declared {
		s.Declared[d] = true
	}
	for _, u := range used {
		s.Used[u] = true
	}
	return s
}

var all = []string{"Bash", "Read", "Edit", "Write", "Agent", "Skill", "ToolSearch", "AskUserQuestion",
	"context_guru_expand", "Workflow", "Monitor", "mcp__a__b", "Tiny", "Kept", "ProjUsed", "UsedOnce"}

func catalogue() []string {
	var t []string
	for _, n := range all {
		if n == "Tiny" {
			t = append(t, smallDecl(n))
		} else {
			t = append(t, decl(n))
		}
	}
	return t
}

func threeUnused() hist {
	return hist{sessions: []Session{sess(all), sess(all), sess(all, "UsedOnce")},
		project: map[string]bool{"ProjUsed": true}}
}

// Every rule row, in one table.
func TestDecideRuleRows(t *testing.T) {
	in := Input{Session: "s", Project: "/w", Body: body(catalogue(), ""), Keep: []string{"Kept"}}
	got := strings.Join(Decide(threeUnused(), in), ",")
	if got != "Monitor,Workflow,mcp__a__b" {
		t.Fatalf("deferred = %s, want Monitor,Workflow,mcp__a__b", got)
	}
	for _, n := range []string{"Bash", "Read", "Edit", "Write", "Agent", "Skill", "ToolSearch", "AskUserQuestion", "context_guru_expand"} {
		if strings.Contains(","+got+",", ","+n+",") {
			t.Errorf("never-defer tool %s deferred", n)
		}
	}
}

func TestDecideNeedsThreeSessions(t *testing.T) {
	in := Input{Session: "s", Body: body(catalogue(), "")}
	h := hist{sessions: []Session{sess(all), sess(all)}}
	if got := Decide(h, in); len(got) != 0 {
		t.Fatalf("2 sessions deferred %v", got)
	}
	if got := Decide(nil, in); len(got) != 0 {
		t.Fatalf("no history deferred %v", got)
	}
}

// A tool the user's sessions did not ALL declare is not deferred: "declared and used in none".
func TestDecideNeedsDeclaredInEverySession(t *testing.T) {
	in := Input{Session: "s", Body: body([]string{decl("Bash"), decl("Workflow")}, "")}
	h := hist{sessions: []Session{sess([]string{"Bash", "Workflow"}), sess([]string{"Bash"}), sess([]string{"Bash", "Workflow"})}}
	if got := Decide(h, in); len(got) != 0 {
		t.Fatalf("deferred %v; Workflow was not declared in every session", got)
	}
}

func TestDecideUsedInThreadAndLoadedCountsAsUse(t *testing.T) {
	use := `{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Workflow","input":{}}]}`
	load := `{"role":"assistant","content":[{"type":"server_tool_use","id":"srvtoolu_1","name":"tool_search_tool_bm25","input":{"query":"Monitor"}},` +
		`{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_1","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"Monitor"}]}}]}`
	in := Input{Session: "s", Body: body(catalogue(), "", use, load)}
	got := strings.Join(Decide(threeUnused(), in), ",")
	if strings.Contains(got, "Workflow") || strings.Contains(got, "Monitor") {
		t.Fatalf("used or loaded tool deferred: %s", got)
	}
}

func TestDecideForcedToolChoice(t *testing.T) {
	in := Input{Session: "s", Body: body(catalogue(), `"tool_choice":{"type":"tool","name":"Workflow"},`)}
	if got := strings.Join(Decide(threeUnused(), in), ","); strings.Contains(got, "Workflow") {
		t.Fatalf("forced tool deferred: %s", got)
	}
}

// The 200-token gate reads the ORIGINAL weight: a tool the client already sent with defer_loading
// is left alone (never re-deferred, never weighed at 0 and refused), and a small tool is kept.
func TestDecideWeighsOriginal(t *testing.T) {
	in := Input{Session: "s", Body: body(catalogue(), "")}
	got := strings.Join(Decide(threeUnused(), in), ",")
	if strings.Contains(got, "Tiny") {
		t.Fatalf("a tool under %d tokens was deferred: %s", MinTokens, got)
	}
	// The same decision on the REWRITTEN body would see defer_loading and skip; Apply always
	// passes Orig, which TestApplyDecidesOnOriginal pins.
}

// mcpTool is one MCP tool of about 120 tokens.
func mcpTool(server string, i int) string {
	return fmt.Sprintf(`{"name":"mcp__%s__t%d","description":"%s","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}`,
		server, i, strings.Repeat("Looks up one record of the service. ", 12))
}

func serverTools(server string, n int) []string {
	var out []string
	for i := 0; i < n; i++ {
		out = append(out, mcpTool(server, i))
	}
	return out
}

func toolNames(tools []string) []string {
	var out []string
	for _, t := range tools {
		out = append(out, gjson.Get(t, "name").String())
	}
	return out
}

// MCP tools are decided per server. A server of 30 small tools (each under 200 tokens) is deferred
// whole; a server whose total is under 200 is not.
func TestDecideMCPPerServer(t *testing.T) {
	big := serverTools("big", 30)
	small := serverTools("small", 1)
	if n := tokens.Count(big[0]); n >= MinTokens || n < 80 {
		t.Fatalf("fixture: one tool weighs %d tokens", n)
	}
	tools := append(append([]string{decl("Bash")}, big...), small...)
	declared := append([]string{"Bash"}, append(toolNames(big), toolNames(small)...)...)
	h := hist{sessions: []Session{sess(declared), sess(declared), sess(declared)}}
	got := Decide(h, Input{Session: "s", Body: body(tools, "")})
	if len(got) != 30 || !strings.HasPrefix(got[0], "mcp__big__") {
		t.Fatalf("deferred %d tools: %v", len(got), got)
	}
	for _, n := range got {
		if strings.HasPrefix(n, "mcp__small__") {
			t.Fatalf("a server under %d tokens in total was deferred: %v", MinTokens, got)
		}
	}
}

// Using one tool of a server in a past session keeps the whole server loaded.
func TestDecideMCPOneUseKeepsTheServer(t *testing.T) {
	big := serverTools("big", 30)
	tools := append([]string{decl("Bash")}, big...)
	declared := append([]string{"Bash"}, toolNames(big)...)
	h := hist{sessions: []Session{sess(declared), sess(declared, "mcp__big__t7"), sess(declared)}}
	if got := Decide(h, Input{Session: "s", Body: body(tools, "")}); len(got) != 0 {
		t.Fatalf("a used server was deferred: %v", got)
	}
	// The project veto works per server too.
	h = hist{sessions: []Session{sess(declared), sess(declared), sess(declared)}, project: map[string]bool{"mcp__big__t3": true}}
	if got := Decide(h, Input{Session: "s", Project: "/w", Body: body(tools, "")}); len(got) != 0 {
		t.Fatalf("project veto ignored for the server: %v", got)
	}
	// A server not declared in every session is not deferred.
	h = hist{sessions: []Session{sess(declared), sess([]string{"Bash"}), sess(declared)}}
	if got := Decide(h, Input{Session: "s", Body: body(tools, "")}); len(got) != 0 {
		t.Fatalf("a server missing from one session was deferred: %v", got)
	}
	// A tool of the server loaded in this thread keeps the server loaded.
	load := `{"role":"assistant","content":[{"type":"tool_use","id":"x","name":"mcp__big__t1","input":{}}]}`
	h = hist{sessions: []Session{sess(declared), sess(declared), sess(declared)}}
	if got := Decide(h, Input{Session: "s", Body: body(tools, "", load)}); len(got) != 0 {
		t.Fatalf("a server used in this thread was deferred: %v", got)
	}
}

func TestRewriteShape(t *testing.T) {
	b := body([]string{decl("Bash"), decl("Workflow"), decl("Monitor")}, "")
	out, n, ok := Rewrite(b, map[string]bool{"Workflow": true, "Monitor": true})
	if !ok || strings.Join(n, ",") != "Monitor,Workflow" {
		t.Fatalf("ok=%v names=%v", ok, n)
	}
	if got := names(out); got != "Bash,Workflow*,Monitor*,tool_search_tool_bm25" {
		t.Fatalf("tools = %s", got)
	}
	if gjson.GetBytes(out, "tools.3.type").String() != SearchType {
		t.Fatal("search tool type not pinned")
	}
	// The names list is in the system message at messages[1], not at the end.
	m1 := gjson.GetBytes(out, "messages.1.content").String()
	if !strings.HasPrefix(m1, "# Environment") || !strings.HasSuffix(m1, NamesList([]string{"Monitor", "Workflow"})) {
		t.Fatalf("messages[1] = %q", m1)
	}
	if gjson.GetBytes(out, "messages.#").Int() != 2 {
		t.Fatal("a message was added although messages[1] is a system message")
	}
	// Same input, same bytes.
	again, _, _ := Rewrite(b, map[string]bool{"Monitor": true, "Workflow": true})
	if string(again) != string(out) {
		t.Fatal("rewrite is not deterministic")
	}
}

func TestRewriteAppendsToClaudeCodeDeferredBlock(t *testing.T) {
	cc := `{"role":"user","content":"hi"},{"role":"system","content":[{"type":"text","text":"` + deferred.Header + `\\nCronList","cache_control":{"type":"ephemeral"}}]}`
	b := []byte(`{"model":"claude-x","tools":[` + decl("Bash") + `,` + decl("Workflow") + `],"messages":[` + cc + `]}`)
	out, _, ok := Rewrite(b, map[string]bool{"Workflow": true})
	if !ok {
		t.Fatal("not rewritten")
	}
	txt := gjson.GetBytes(out, "messages.1.content.0.text").String()
	if !strings.HasPrefix(txt, deferred.Header) || !strings.Contains(txt, "Deferred tools:\nWorkflow") {
		t.Fatalf("block = %q", txt)
	}
	if !gjson.GetBytes(out, "messages.1.content.0.cache_control").Exists() {
		t.Fatal("the block's breakpoint was lost")
	}
}

func TestRewriteInsertsSystemMessageAtIndexOne(t *testing.T) {
	b := []byte(`{"model":"claude-x","tools":[` + decl("Bash") + `,` + decl("Workflow") + `],"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]}`)
	out, _, ok := Rewrite(b, map[string]bool{"Workflow": true})
	if !ok {
		t.Fatal("not rewritten")
	}
	var roles []string
	gjson.GetBytes(out, "messages").ForEach(func(_, m gjson.Result) bool {
		roles = append(roles, m.Get("role").String())
		return true
	})
	if strings.Join(roles, ",") != "user,system,assistant,user" {
		t.Fatalf("roles = %v", roles)
	}
	one := []byte(`{"model":"claude-x","tools":[` + decl("Bash") + `,` + decl("Workflow") + `],"messages":[{"role":"user","content":"a"}]}`)
	out, _, _ = Rewrite(one, map[string]bool{"Workflow": true})
	if gjson.GetBytes(out, "messages.1.role").String() != "system" {
		t.Fatalf("one-message body: %s", gjson.GetBytes(out, "messages").Raw)
	}
}

// A deferred tool cannot carry cache_control: the mark moves to the last loaded tool.
func TestRewriteMovesCacheControl(t *testing.T) {
	marked := strings.TrimSuffix(decl("Workflow"), "}") + `,"cache_control":{"type":"ephemeral"}}`
	b := body([]string{decl("Bash"), decl("Read"), marked}, "")
	out, _, ok := Rewrite(b, map[string]bool{"Workflow": true})
	if !ok {
		t.Fatal("not rewritten")
	}
	if gjson.GetBytes(out, "tools.2.cache_control").Exists() {
		t.Fatal("a deferred tool kept cache_control")
	}
	if gjson.GetBytes(out, "tools.1.cache_control.type").String() != "ephemeral" {
		t.Fatalf("mark not moved to the last loaded tool: %s", gjson.GetBytes(out, "tools").Raw)
	}
	if n := strings.Count(string(out), `"cache_control"`); n != 1 {
		t.Fatalf("%d cache_control marks, want 1", n)
	}
}

// Never defer the last loaded client tool.
func TestRewriteKeepsLastLoaded(t *testing.T) {
	b := body([]string{decl("Workflow"), decl("Monitor")}, "")
	out, n, ok := Rewrite(b, map[string]bool{"Workflow": true, "Monitor": true})
	if !ok || strings.Join(n, ",") != "Workflow" {
		t.Fatalf("names = %v", n)
	}
	if got := names(out); got != "Workflow*,Monitor,tool_search_tool_bm25" {
		t.Fatalf("tools = %s", got)
	}
	// One client tool only: nothing to defer.
	if _, _, ok := Rewrite(body([]string{decl("Workflow")}, ""), map[string]bool{"Workflow": true}); ok {
		t.Fatal("the only client tool was deferred")
	}
}

func TestRewriteLeavesServerAndClientDeferredTools(t *testing.T) {
	cd := strings.TrimSuffix(decl("CronList"), "}") + `,"defer_loading":true}`
	b := body([]string{decl("Bash"), `{"type":"web_search_20250305","name":"web_search"}`, cd, decl("Workflow")}, "")
	out, n, ok := Rewrite(b, map[string]bool{"web_search": true, "CronList": true, "Workflow": true})
	if !ok || strings.Join(n, ",") != "Workflow" {
		t.Fatalf("names = %v", n)
	}
	if gjson.GetBytes(out, "tools.1.defer_loading").Exists() {
		t.Fatal("a server tool was marked")
	}
}

// No cache_control mark is lost: with the last loaded tool already marked, the moved mark goes on
// the loaded tool before it, and two marked deferred tools keep both marks.
func TestRewriteKeepsEveryCacheMark(t *testing.T) {
	mk := func(d string) string { return strings.TrimSuffix(d, "}") + `,"cache_control":{"type":"ephemeral"}}` }
	b := body([]string{decl("Bash"), decl("Read"), mk(decl("Edit")), mk(decl("Workflow")), mk(decl("Monitor"))}, "")
	out, n, ok := Rewrite(b, map[string]bool{"Workflow": true, "Monitor": true})
	if !ok || strings.Join(n, ",") != "Monitor,Workflow" {
		t.Fatalf("names = %v", n)
	}
	if c := strings.Count(string(out), `"cache_control"`); c != 3 {
		t.Fatalf("%d cache_control marks, want 3: %s", c, gjson.GetBytes(out, "tools").Raw)
	}
	for i, want := range []bool{true, true, true, false, false} {
		if got := gjson.GetBytes(out, "tools."+itoa(i)+".cache_control").Exists(); got != want {
			t.Errorf("tools.%d cache_control = %v, want %v", i, got, want)
		}
	}
}

// A later turn that forces a deferred tool gets it loaded for that request, and the names list keeps
// its bytes.
func TestRewriteLeavesForcedToolLoaded(t *testing.T) {
	set := map[string]bool{"Workflow": true, "Monitor": true}
	plain, _, _ := Rewrite(body([]string{decl("Bash"), decl("Workflow"), decl("Monitor")}, ""), set)
	forced, n, ok := Rewrite(body([]string{decl("Bash"), decl("Workflow"), decl("Monitor")}, `"tool_choice":{"type":"tool","name":"Workflow"},`), set)
	if !ok || strings.Join(n, ",") != "Monitor" {
		t.Fatalf("names = %v", n)
	}
	if got := names(forced); got != "Bash,Workflow,Monitor*,tool_search_tool_bm25" {
		t.Fatalf("tools = %s", got)
	}
	if gjson.GetBytes(forced, "messages.1").Raw != gjson.GetBytes(plain, "messages.1").Raw {
		t.Fatal("the names list changed on the forced turn")
	}
	if !set["Workflow"] {
		t.Fatal("Rewrite changed the caller's set")
	}
}

func TestSearchOffValue(t *testing.T) {
	for v, want := range map[string]bool{"false": true, "FALSE": true, "False": true, "0": true, "no": true, "OFF": true,
		" false ": true, "auto": false, "auto:5": false, "true": false, "1": false, "": false} {
		if SearchOffValue(v) != want {
			t.Errorf("SearchOffValue(%q) = %v", v, !want)
		}
	}
}

func TestDeferralCause(t *testing.T) {
	for b, want := range map[string]bool{
		`{"error":{"message":"tools.2.custom.defer_loading: Extra inputs are not permitted"}}`:               true,
		`{"error":{"message":"At least one tool must have defer_loading=false."}}`:                           true,
		`{"error":{"message":"Tool 'X' cannot have both defer_loading=true and cache_control set."}}`:        true,
		`{"error":{"message":"Tool reference 'X' not found in available tools"}}`:                            true,
		`{"error":{"message":"tool type 'tool_search_tool_bm25_20990101' is not supported for this model"}}`: true,
		`{"error":{"message":"messages.3: text content blocks must be non-empty"}}`:                          false,
		`{"error":{"message":"model: unknown model"}}`:                                                       false,
		``: false,
	} {
		if DeferralCause([]byte(b)) != want {
			t.Errorf("DeferralCause(%s) = %v", b, !want)
		}
	}
}

func TestGateBreakEvictsOldestPerTenant(t *testing.T) {
	g := NewGate()
	now := time.Unix(0, 0)
	g.now = func() time.Time { return now }
	now = now.Add(time.Second)
	g.Break("quiet", "q")
	for i := 0; i < maxBrokenPerTenant; i++ {
		now = now.Add(time.Second)
		g.Break("noisy", "m"+itoa(i))
	}
	now = now.Add(time.Second)
	g.Break("noisy", "new")
	if g.Broken("noisy", "m0") || !g.Broken("noisy", "m1") || !g.Broken("noisy", "new") {
		t.Fatal("Break did not evict the tenant's oldest entry")
	}
	if !g.Broken("quiet", "q") {
		t.Fatal("another tenant's 400s pushed this tenant's turn-off out")
	}
	g.mu.Lock()
	n := len(g.broken["noisy"])
	g.mu.Unlock()
	if n > maxBrokenPerTenant {
		t.Fatalf("%d turn-offs for one tenant", n)
	}
	g.DropTenant("noisy")
	if g.Broken("noisy", "new") || !g.Broken("quiet", "q") {
		t.Fatal("DropTenant did not drop exactly the tenant's turn-offs")
	}
}

// ── Apply: the thread-fixed set ──────────────────────────────────────────

// env is a deferral Env over a memory-only session store and tool deferral's own file, which holds
// the records (the session store is not durable). Its file is in a fresh folder of the test.
func env(t *testing.T, h History) Env {
	t.Helper()
	e, _ := envAt(t, h, filepath.Join(t.TempDir(), FileName))
	return e
}

// envAt is env over the file at path: a second envAt on the same path is the same proxy after a
// restart (fresh memory, fresh prober, the same file).
func envAt(t *testing.T, h History, path string) (Env, *File) {
	t.Helper()
	f, err := OpenFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	p := NewProber()
	p.Go = func(fn func()) { fn() }
	return Env{Store: store.NewMemory(store.Options{}), Fallback: f.Store, History: h, Gate: NewGate(), Prober: p}, f
}

func passProbe(e Env, k ProbeKey) {
	e.Prober.mu.Lock()
	e.Prober.m[k] = &probeEntry{v: Pass, at: e.Prober.now()}
	e.Prober.mu.Unlock()
}

func req(b []byte) Req {
	return Req{Session: "s", Thread: "", Project: "/w", Model: "claude-sonnet-5-5", Orig: b, Probe: ProbeKey{"u", "m", "r"},
		Keep: []string{"Kept"}}
}

func TestApplyDecidesOnceAndKeepsTheSet(t *testing.T) {
	h := threeUnused()
	e := env(t, h)
	passProbe(e, ProbeKey{"u", "m", "r"})
	first := body(catalogue(), "")
	o := Apply(e, req(first))
	if !o.Changed || !o.Decided || strings.Join(o.Deferred, ",") != "Monitor,Workflow,mcp__a__b" {
		t.Fatalf("first: %s", show(o))
	}
	// Later turn: the history now says nothing is unused. The set must not change.
	h2 := hist{sessions: []Session{sess(all, all...), sess(all, all...), sess(all, all...)}}
	e.History = h2
	later := body(catalogue(), "", `{"role":"assistant","content":"ok"}`, `{"role":"user","content":"more"}`)
	o2 := Apply(e, req(later))
	if !o2.Changed || o2.Decided || strings.Join(o2.Deferred, ",") != strings.Join(o.Deferred, ",") {
		t.Fatalf("set moved: %s", show(o2))
	}
	if gjson.GetBytes(o2.Body, "tools").Raw != gjson.GetBytes(o.Body, "tools").Raw {
		t.Fatal("tools bytes changed between turns")
	}
	if gjson.GetBytes(o2.Body, "messages.1").Raw != gjson.GetBytes(o.Body, "messages.1").Raw {
		t.Fatal("names list bytes changed between turns")
	}
}

// A thread first seen mid-way without a record and without a probe verdict is a first request
// (N11), decided "nothing" for its life, so it sends the original.
func TestApplyMidThreadUnknownProbeSendsOriginal(t *testing.T) {
	e := env(t, threeUnused())
	later := body(catalogue(), "", `{"role":"assistant","content":"ok"}`, `{"role":"user","content":"more"}`)
	o := Apply(e, req(later))
	if o.Changed || !o.NeedProbe {
		t.Fatalf("%s", show(o))
	}
}

func TestApplyFirstRequestUnprobedDecidesNothingForTheThread(t *testing.T) {
	e := env(t, threeUnused())
	o := Apply(e, req(body(catalogue(), "")))
	if o.Changed || !o.NeedProbe || o.Why != "probe_unknown" {
		t.Fatalf("%s", show(o))
	}
	// The probe passes later: this thread stays as it started (nothing deferred).
	passProbe(e, ProbeKey{"u", "m", "r"})
	o2 := Apply(e, req(body(catalogue(), "", `{"role":"assistant","content":"ok"}`, `{"role":"user","content":"x"}`)))
	if o2.Changed {
		t.Fatalf("a thread that started undeferred became deferred: %s", show(o2))
	}
}

// B1. A restart with a memory-only session store: the record comes back from tool deferral's own
// file, so tools and messages[1] are byte-identical and nothing is rebuilt.
func TestApplyRestartKeepsTheRecordInTheFile(t *testing.T) {
	h := threeUnused()
	path := filepath.Join(t.TempDir(), FileName)
	e, f := envAt(t, h, path)
	passProbe(e, ProbeKey{"u", "m", "r"})
	first := Apply(e, req(body(catalogue(), "")))
	later := body(catalogue(), "", `{"role":"assistant","content":"ok"}`, `{"role":"user","content":"more"}`)
	before := Apply(e, req(later))
	f.Close() // the process ends: the file is flushed
	// Restart: a fresh session store, a fresh prober (no verdict), an EMPTY history, the same file.
	e2, _ := envAt(t, hist{}, path)
	after := Apply(e2, req(later))
	if after.Decided || after.Rebuilt || string(after.Body) != string(before.Body) {
		t.Fatalf("restart changed the request:\nbefore %s\nafter  %s (%s)", names(before.Body), names(after.Body), show(after))
	}
	if strings.Join(after.Deferred, ",") != strings.Join(first.Deferred, ",") {
		t.Fatalf("set after restart = %v, want %v", after.Deferred, first.Deferred)
	}
}

// B1. With no durable place at all (the session store is memory only, and no file), nothing is
// deferred, so a restart has nothing to flip.
func TestApplyNoDurablePlaceDefersNothing(t *testing.T) {
	e := env(t, threeUnused())
	e.Fallback = nil
	passProbe(e, ProbeKey{"u", "m", "r"})
	if o := Apply(e, req(body(catalogue(), ""))); o.Changed || o.Why != "no_durable_store" {
		t.Fatalf("%s", show(o))
	}
	// A durable session store (the dashboard's Disk) is used as before.
	e.Store = e2Durable(t)
	if o := Apply(e, req(body(catalogue(), ""))); !o.Changed {
		t.Fatalf("with a durable session store: %s", show(o))
	}
	if _, ok := Load(e.Store, "s", ""); !ok {
		t.Fatal("the record is not in the durable session store")
	}
}

// e2Durable is a session store with a Disk attached, as with the dashboard database.
func e2Durable(t *testing.T) store.Store {
	f, err := OpenFile(filepath.Join(t.TempDir(), "dash.db"), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f.Store
}

// A record lost mid-thread (evicted) in a thread our search loaded from: rebuilt, and the bytes
// match.
func TestApplyLostRecordRebuildsTheSameSet(t *testing.T) {
	h := threeUnused()
	e := env(t, h)
	passProbe(e, ProbeKey{"u", "m", "r"})
	o := Apply(e, req(body(catalogue(), "")))
	load := `{"role":"assistant","content":[{"type":"server_tool_use","id":"srvtoolu_1","name":"tool_search_tool_bm25","input":{"query":"Workflow","limit":1}},` +
		`{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_1","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"Workflow"}]}},` +
		`{"type":"tool_use","id":"t9","name":"Workflow","input":{}}]}`
	res := `{"role":"user","content":[{"type":"tool_result","tool_use_id":"t9","content":"done"}]}`
	later := body(catalogue(), "", load, res)
	before := Apply(e, req(later))
	e2 := env(t, h) // a different file: the record is gone
	after := Apply(e2, req(later))
	if !after.Rebuilt || string(after.Body) != string(before.Body) {
		t.Fatalf("rebuild changed the request:\nbefore %s\nafter  %s (%s)", names(before.Body), names(after.Body), show(after))
	}
	if strings.Join(after.Deferred, ",") != strings.Join(o.Deferred, ",") {
		t.Fatalf("set after rebuild = %v, want %v", after.Deferred, o.Deferred)
	}
}

// N11. A thread whose first request already holds assistant turns (a fork, a resume under a new
// session id), with no record and no search of ours, is a first request: it is decided.
func TestApplyForkIsAFirstRequest(t *testing.T) {
	e := env(t, threeUnused())
	passProbe(e, ProbeKey{"u", "m", "r"})
	fork := body(catalogue(), "", `{"role":"assistant","content":"ok"}`, `{"role":"user","content":"more"}`)
	if o := Apply(e, req(fork)); !o.Decided || o.Rebuilt || !o.Changed {
		t.Fatalf("%s", show(o))
	}
}

// H3. The opt-out blocks NEW threads only. With `claude -p` the flag can land after the thread's
// first request: that thread keeps its record, and a new thread of the session is not deferred.
func TestApplyOptOutAfterFirstRequestKeepsTheThread(t *testing.T) {
	e := env(t, threeUnused())
	passProbe(e, ProbeKey{"u", "m", "r"})
	first := Apply(e, req(body(catalogue(), "")))
	if !first.Changed {
		t.Fatalf("%s", show(first))
	}
	// The hook's flag arrives now, for a seen session.
	e.Gate.NoteToolSearch("", "s", "false", e.RecordStore(), true)
	later := Apply(e, req(body(catalogue(), "", `{"role":"assistant","content":"ok"}`, `{"role":"user","content":"x"}`)))
	if !later.Changed || strings.Join(later.Deferred, ",") != strings.Join(first.Deferred, ",") {
		t.Fatalf("the flag flipped a live thread: %s", show(later))
	}
	sub := req(body(catalogue(), ""))
	sub.Thread = "agent-2"
	if o := Apply(e, sub); o.Changed || o.Why != "tool_search_false" {
		t.Fatalf("a new thread after the flag: %s", show(o))
	}
	// The flag is in the durable store: a restart keeps it.
	if !SearchOff(e.RecordStore(), "s") {
		t.Fatal("the flag is not stored")
	}
}

// B4b and H3. A flag for a session the proxy has not seen waits, per tenant, for that session's
// first request; one tenant's flags cannot push out another's.
func TestGatePendingOptOutIsPerTenant(t *testing.T) {
	g := NewGate()
	for i := 0; i < maxPendingPerTenant+10; i++ {
		g.NoteToolSearch("noisy", "s"+itoa(i), "false", nil, false)
	}
	g.NoteToolSearch("quiet", "q", "false", nil, false)
	for i := 0; i < maxPendingPerTenant+10; i++ {
		g.NoteToolSearch("noisy", "x"+itoa(i), "false", nil, false)
	}
	st := e2Durable(t)
	if !g.TakePending("quiet", "q", st) || !SearchOff(st, "q") {
		t.Fatal("another tenant's flags pushed this one out")
	}
	if g.TakePending("quiet", "q", st) {
		t.Fatal("a pending flag was taken twice")
	}
	if g.TakePending("other", "s1", st) {
		t.Fatal("a flag leaked across tenants")
	}
	g.mu.Lock()
	n := len(g.pending["noisy"])
	g.mu.Unlock()
	if n > maxPendingPerTenant {
		t.Fatalf("%d pending for one tenant", n)
	}
}

func TestApplyOffSwitches(t *testing.T) {
	k := ProbeKey{"u", "m", "r"}
	b := body(catalogue(), "")
	for name, mut := range map[string]func(*Env, *Req){
		"account_off":          func(_ *Env, r *Req) { r.AccountOff = true },
		"not_anthropic":        func(_ *Env, r *Req) { r.Model = "gpt-5.5" },
		"tool_search_false":    func(e *Env, r *Req) { e.Gate.NoteToolSearch(r.Tenant, r.Session, "false", nil, false) },
		"turned_off_after_400": func(e *Env, r *Req) { e.Gate.Break(r.Tenant, r.Model) },
		"no_durable_store":     func(e *Env, _ *Req) { e.Fallback = nil },
		"no_session":           func(_ *Env, r *Req) { r.Session = "" },
	} {
		e := env(t, threeUnused())
		passProbe(e, k)
		r := req(b)
		mut(&e, &r)
		if o := Apply(e, r); o.Changed || o.Why != name {
			t.Errorf("%s: changed=%v why=%q", name, o.Changed, o.Why)
		}
	}
	// "auto" is not a decision.
	e := env(t, threeUnused())
	passProbe(e, k)
	e.Gate.NoteToolSearch("", "s", "auto", nil, false)
	if o := Apply(e, req(b)); !o.Changed {
		t.Errorf("ENABLE_TOOL_SEARCH=auto turned deferral off: %s", show(o))
	}
}

// Subagents are their own thread: each keeps its own set.
func TestApplyThreadsAreSeparate(t *testing.T) {
	h := threeUnused()
	e := env(t, h)
	passProbe(e, ProbeKey{"u", "m", "r"})
	main := Apply(e, req(body(catalogue(), "")))
	sub := req(body([]string{decl("Read"), decl("Bash")}, ""))
	sub.Thread = "agent-1"
	so := Apply(e, sub)
	if so.Changed {
		t.Fatalf("subagent with no deferrable tool changed: %s", show(so))
	}
	if r, ok := Load(e.RecordStore(), "s", "agent-1"); !ok || len(r.Deferred) != 0 {
		t.Fatalf("subagent record: %+v %v", r, ok)
	}
	if r, _ := Load(e.RecordStore(), "s", ""); strings.Join(r.Deferred, ",") != strings.Join(main.Deferred, ",") {
		t.Fatal("subagent changed the main thread's record")
	}
}

// The weights come from the ORIGINAL body, and a Body already rewritten by the pipeline is what
// gets the marks.
func TestApplyDecidesOnOriginal(t *testing.T) {
	e := env(t, threeUnused())
	passProbe(e, ProbeKey{"u", "m", "r"})
	orig := body(catalogue(), "")
	r := req(orig)
	r.Body = []byte(strings.Replace(string(orig), `"hi"`, `"HI"`, 1))
	o := Apply(e, r)
	if !o.Changed || o.DeferredTokens < 3*MinTokens || !strings.Contains(string(o.Body), `"HI"`) {
		t.Fatalf("%s", show(o))
	}
}

func TestStoreKeyIsMetadata(t *testing.T) {
	if !strings.HasPrefix(Key("s", "t"), store.ToolDeferPrefix) {
		t.Fatal("key not under the pinned prefix")
	}
	found := false
	for _, p := range store.StatePrefixes {
		found = found || p == store.ToolDeferPrefix
	}
	if !found {
		t.Fatal("the disk store does not keep the deferral record")
	}
}

// #866 N4. A record lost mid-thread, and the history drifted in between (another session used the
// tool our search had loaded): the rule's set leaves out a tool the transcript proves deferred. The
// set is unknown, so the original is sent and nothing is saved.
func TestApplyRebuildThatDriftedSendsTheOriginal(t *testing.T) {
	h := threeUnused()
	e := env(t, h)
	passProbe(e, ProbeKey{"u", "m", "r"})
	Apply(e, req(body(catalogue(), "")))
	load := `{"role":"assistant","content":[{"type":"server_tool_use","id":"srvtoolu_1","name":"tool_search_tool_bm25","input":{"query":"Workflow","limit":1}},` +
		`{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_1","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"Workflow"}]}},` +
		`{"type":"tool_use","id":"t9","name":"Workflow","input":{}}]}`
	res := `{"role":"user","content":[{"type":"tool_result","tool_use_id":"t9","content":"done"}]}`
	later := body(catalogue(), "", load, res)
	drifted := hist{sessions: []Session{sess(all, "Workflow"), sess(all), sess(all)}, project: h.project}
	e2 := env(t, drifted) // a different file: the record is gone
	o := Apply(e2, req(later))
	if o.Changed || o.Why != "rebuild_unproven" || string(o.Body) != string(later) {
		t.Fatalf("a drifted rebuild was sent: %s", show(o))
	}
	if _, ok := Load(e2.RecordStore(), "s", ""); ok {
		t.Fatal("a drifted rebuild was saved")
	}
	if e2.Gate.Unproven.Load() != 1 {
		t.Fatal("not counted")
	}
	// The same history as at the decision gives the proven set: Rebuild accepts it.
	if set, ok := Rebuild(h, Input{Session: "s", Project: "/w", Body: later, Keep: []string{"Kept"}}); !ok ||
		strings.Join(set, ",") != "Monitor,Workflow,mcp__a__b" {
		t.Fatalf("Rebuild = %v %v", set, ok)
	}
}

// ── the always-defer list (#878) ────────────────────────────────────────────

// usedEverywhere is a history where every tool is declared and used, and the project used Workflow:
// the history rule defers nothing.
func usedEverywhere() hist {
	return hist{sessions: []Session{sess(all, all...), sess(all, all...), sess(all, all...)},
		project: map[string]bool{"Workflow": true, "mcp__a__b": true}}
}

// A listed tool skips the history rule and the project veto, and obeys every other rule.
func TestDecideAlwaysDeferRuleCases(t *testing.T) {
	use := `{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Monitor","input":{}}]}`
	for _, c := range []struct {
		name   string
		h      History
		always []string
		keep   []string
		extra  string
		msgs   []string
		want   string
	}{
		{"skips the history rule", usedEverywhere(), []string{"Monitor"}, nil, "", nil, "Monitor"},
		{"skips the project veto", usedEverywhere(), []string{"Workflow"}, nil, "", nil, "Workflow"},
		{"needs no history", nil, []string{"Monitor"}, nil, "", nil, "Monitor"},
		{"needs no 3 sessions", hist{sessions: []Session{sess(all)}}, []string{"Monitor"}, nil, "", nil, "Monitor"},
		{"a whole MCP server", usedEverywhere(), []string{"mcp__a"}, nil, "", nil, "mcp__a__b"},
		{"never-defer wins", usedEverywhere(), []string{"Bash", "context_guru_expand"}, nil, "", nil, ""},
		{"keep wins", usedEverywhere(), []string{"Monitor"}, []string{"Monitor"}, "", nil, ""},
		{"keep on the server wins", usedEverywhere(), []string{"mcp__a"}, []string{"mcp__a"}, "", nil, ""},
		{"used in the thread", usedEverywhere(), []string{"Monitor"}, nil, "", []string{use}, ""},
		{"forced by tool_choice", usedEverywhere(), []string{"Monitor"}, nil, `"tool_choice":{"type":"tool","name":"Monitor"},`, nil, ""},
		{"under 200 tokens", usedEverywhere(), []string{"Tiny"}, nil, "", nil, ""},
		{"empty list", usedEverywhere(), nil, nil, "", nil, ""},
		{"adds to the history rule", threeUnused(), []string{"ProjUsed"}, []string{"Kept"}, "", nil, "Monitor,ProjUsed,Workflow,mcp__a__b"},
	} {
		in := Input{Session: "s", Project: "/w", Body: body(catalogue(), c.extra, c.msgs...), Keep: c.keep, AlwaysDefer: c.always}
		if got := strings.Join(Decide(c.h, in), ","); got != c.want {
			t.Errorf("%s: deferred %q, want %q", c.name, got, c.want)
		}
	}
}

// A server is one unit for the list too: its 200-token minimum is its total, and one listed tool of
// it does not defer the rest.
func TestDecideAlwaysDeferMCPServer(t *testing.T) {
	big, small := serverTools("big", 30), serverTools("small", 1)
	tools := append(append([]string{decl("Bash")}, big...), small...)
	got := Decide(nil, Input{Session: "s", Body: body(tools, ""), AlwaysDefer: []string{"mcp__big", "mcp__small"}})
	if len(got) != 30 || !strings.HasPrefix(got[0], "mcp__big__") {
		t.Fatalf("deferred %v; want the 30 tools of mcp__big and not mcp__small (under %d tokens)", got, MinTokens)
	}
	if got := Decide(nil, Input{Session: "s", Body: body(tools, ""), AlwaysDefer: []string{"mcp__big__t1"}}); len(got) != 0 {
		t.Fatalf("one listed tool deferred its server: %v", got)
	}
	// A tool of the server used in this thread keeps the whole server loaded.
	load := `{"role":"assistant","content":[{"type":"tool_use","id":"x","name":"mcp__big__t1","input":{}}]}`
	if got := Decide(nil, Input{Session: "s", Body: body(tools, "", load), AlwaysDefer: []string{"mcp__big"}}); len(got) != 0 {
		t.Fatalf("a server used in this thread was deferred: %v", got)
	}
}

// The list applies at the thread's first request, and the set stays for the thread.
func TestApplyAlwaysDeferAtTheFirstRequest(t *testing.T) {
	e := env(t, usedEverywhere())
	passProbe(e, ProbeKey{"u", "m", "r"})
	r := req(body(catalogue(), ""))
	r.AlwaysDefer = []string{"Monitor"}
	if o := Apply(e, r); !o.Changed || !o.Decided || strings.Join(o.Deferred, ",") != "Monitor" {
		t.Fatalf("first: %s", show(o))
	}
	// The list changes mid-thread: the thread keeps its set.
	later := req(body(catalogue(), "", `{"role":"assistant","content":"ok"}`, `{"role":"user","content":"more"}`))
	later.AlwaysDefer = nil
	if o := Apply(e, later); !o.Changed || o.Decided || strings.Join(o.Deferred, ",") != "Monitor" {
		t.Fatalf("later: %s", show(o))
	}
	// defer: off (the account opt-out) defers nothing, list or not.
	off := req(body(catalogue(), ""))
	off.Thread, off.AlwaysDefer, off.AccountOff = "t2", []string{"Monitor"}, true
	if o := Apply(e, off); o.Changed || o.Why != "account_off" {
		t.Fatalf("defer: off: %s", show(o))
	}
}

// After a restart that lost the record, Rebuild gives the same set: the list reads no history.
func TestApplyAlwaysDeferRebuildsTheSameSet(t *testing.T) {
	h := usedEverywhere()
	e := env(t, h)
	passProbe(e, ProbeKey{"u", "m", "r"})
	r := req(body(catalogue(), ""))
	r.AlwaysDefer = []string{"Monitor", "Workflow"}
	o := Apply(e, r)
	load := `{"role":"assistant","content":[{"type":"server_tool_use","id":"srvtoolu_1","name":"tool_search_tool_bm25","input":{"query":"Workflow","limit":1}},` +
		`{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_1","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"Workflow"}]}},` +
		`{"type":"tool_use","id":"t9","name":"Workflow","input":{}}]}`
	res := `{"role":"user","content":[{"type":"tool_result","tool_use_id":"t9","content":"done"}]}`
	later := req(body(catalogue(), "", load, res))
	later.AlwaysDefer = r.AlwaysDefer
	before := Apply(e, later)
	e2 := env(t, h) // a different file: the record is gone
	after := Apply(e2, later)
	if !after.Rebuilt || string(after.Body) != string(before.Body) {
		t.Fatalf("rebuild changed the request:\nbefore %s\nafter  %s (%s)", names(before.Body), names(after.Body), show(after))
	}
	if strings.Join(after.Deferred, ",") != strings.Join(o.Deferred, ",") || strings.Join(o.Deferred, ",") != "Monitor,Workflow" {
		t.Fatalf("set after rebuild = %v, first = %v; want Monitor,Workflow", after.Deferred, o.Deferred)
	}
}
