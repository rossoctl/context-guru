package tooldefer

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/internal/tokens"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var ckey = ProbeKey{"u", "m", "r#client-search"}

func creq(b []byte) Req {
	r := req(b)
	r.ClientProbe = ckey
	return r
}

// A search round: the model calls ToolSearch, the client answers with its own result.
func searchRound(id, query, result string) []string {
	return []string{
		`{"role":"assistant","content":[{"type":"tool_use","id":"` + id + `","name":"ToolSearch","input":{"query":"` + query + `","max_results":3}}]}`,
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id + `","content":` + result + `}]}`,
	}
}

func clientEnv(t *testing.T, path string) (Env, *File) {
	e, f := envAt(t, threeUnused(), path)
	passProbe(e, ckey)
	return e, f
}

// With ToolSearch in the request and a passing client probe, no server-side search is added.
func TestClientModeAddsNoServerSideSearch(t *testing.T) {
	e, _ := clientEnv(t, filepath.Join(t.TempDir(), FileName))
	o := Apply(e, creq(body(catalogue(), "")))
	if !o.Changed || !o.Client || strings.Contains(string(o.Body), SearchName) {
		t.Fatalf("client mode: %s", show(o))
	}
	if !strings.Contains(gjson.GetBytes(o.Body, "messages.1.content").String(), "select:<exact name>") {
		t.Fatal("the names list does not point at ToolSearch")
	}
}

// Without ToolSearch the server-side search stays (today's behavior). So does a client probe that
// has not passed.
func TestServerSideSearchStaysWithoutToolSearch(t *testing.T) {
	var tools []string
	for _, n := range all {
		if n != "ToolSearch" {
			tools = append(tools, decl(n))
		}
	}
	e, _ := clientEnv(t, filepath.Join(t.TempDir(), FileName))
	passProbe(e, ProbeKey{"u", "m", "r"})
	o := Apply(e, creq(body(tools, "")))
	if !o.Changed || o.Client || !strings.Contains(string(o.Body), SearchName) {
		t.Fatalf("no ToolSearch: %s", show(o))
	}
	e2, _ := envAt(t, threeUnused(), filepath.Join(t.TempDir(), FileName)) // client probe unknown
	passProbe(e2, ProbeKey{"u", "m", "r"})
	o = Apply(e2, creq(body(catalogue(), "")))
	if !o.Changed || o.Client || !o.NeedClientProbe || !strings.Contains(string(o.Body), SearchName) {
		t.Fatalf("client probe unknown: %s", show(o))
	}
}

func init() {
	// The test catalogue's ToolSearch is a verified client version.
	knownDescHashes[DescHash(body(catalogue(), ""))] = true
}

const noMatch = `"No matching deferred tools found"`

func result(b []byte, i int) string {
	return gjson.GetBytes(b, "messages."+itoa(i)+".content.0.content").Raw
}

// Only the exact select form is answered. Claude Code's own result stays; ours are appended.
func TestClientModeAddsOnlyOurToolsToSelectQueries(t *testing.T) {
	e, _ := clientEnv(t, filepath.Join(t.TempDir(), FileName))
	first := Apply(e, creq(body(catalogue(), "")))
	if strings.Join(first.Deferred, ",") != "Monitor,Workflow,mcp__a__b" {
		t.Fatalf("set: %v", first.Deferred)
	}
	cc := `{"name":"CronList","description":"List cron jobs","input_schema":{"type":"object"},"defer_loading":true}`
	tools := append(catalogue(), cc)
	// A pure Claude Code query is left alone.
	own := searchRound("t1", "select:CronList", `[{"type":"tool_reference","tool_name":"CronList"}]`)
	o := Apply(e, creq(body(tools, "", own...)))
	if len(o.Loads) != 0 || result(o.Body, 3) != `[{"type":"tool_reference","tool_name":"CronList"}]` {
		t.Fatalf("a pure client query was changed: %s", result(o.Body, 3))
	}
	// Ours alone: the client said "no match"; we add the reference. Spacing and case are as the client reads them.
	for id, q := range map[string]string{"t2": "select:Workflow", "t3": "Select: Workflow"} {
		o = Apply(e, creq(body(tools, "", searchRound(id, q, noMatch)...)))
		if result(o.Body, 3) != `[{"type":"tool_reference","tool_name":"Workflow"}]` || strings.Join(o.Loads, ",") != "Workflow" {
			t.Fatalf("%q: result %s loads %v", q, result(o.Body, 3), o.Loads)
		}
	}
	// Mixed: the client's reference stays first, ours follows. The cap (3 in searchRound) covers both.
	mix := searchRound("t4", "select:CronList,Workflow,Monitor", `[{"type":"tool_reference","tool_name":"CronList"}]`)
	o = Apply(e, creq(body(tools, "", mix...)))
	want := `[{"type":"tool_reference","tool_name":"CronList"},{"type":"tool_reference","tool_name":"Workflow"},{"type":"tool_reference","tool_name":"Monitor"}]`
	if result(o.Body, 3) != want {
		t.Fatalf("mixed = %s", result(o.Body, 3))
	}
	// An error result is not touched.
	o = Apply(e, creq(body(tools, "", searchRound("t5", "select:Workflow", `"Error: boom"`)...)))
	if result(o.Body, 3) != `"Error: boom"` {
		t.Fatalf("error result changed: %s", result(o.Body, 3))
	}
}

// A keyword query that does not name our tools passes through. One that names them is a format
// change: the hash is disabled and the thread stops deferring.
func TestClientModeNonSelectQueryDisables(t *testing.T) {
	e, _ := clientEnv(t, filepath.Join(t.TempDir(), FileName))
	Apply(e, creq(body(catalogue(), "")))
	o := Apply(e, creq(body(catalogue(), "", searchRound("t1", "cron jobs", noMatch)...)))
	if !o.Changed || result(o.Body, 3) != noMatch || o.ClientDisabled != "" {
		t.Fatalf("keyword query: %s", show(o))
	}
	o = Apply(e, creq(body(catalogue(), "", searchRound("t2", "+Workflow script", noMatch)...)))
	if o.Changed || o.ClientDisabled != ReasonNonSelectQuery || e.Gate.DisabledNonSel.Load() != 1 {
		t.Fatalf("a non-select query for our tool: %s (%q)", show(o), o.ClientDisabled)
	}
	h := DescHash(body(catalogue(), ""))
	if !e.Gate.ClientDisabled("", h) {
		t.Fatal("hash not marked disabled")
	}
	// The live thread stays undeferred, and a new thread defers nothing.
	if o = Apply(e, creq(body(catalogue(), "", searchRound("t3", "x", noMatch)...))); o.Changed {
		t.Fatalf("live thread still deferred: %s", show(o))
	}
	r := creq(body(catalogue(), ""))
	r.Thread = "new"
	if o = Apply(e, r); o.Changed || o.Why != "client_search_disabled" {
		t.Fatalf("new thread: %s", show(o))
	}
}

// An unknown description hash defers nothing, and is counted once.
func TestClientModeUnknownDescriptionDefersNothing(t *testing.T) {
	e, _ := clientEnv(t, filepath.Join(t.TempDir(), FileName))
	b := []byte(strings.Replace(string(body(catalogue(), "")), "Does the ToolSearch job.", "A changed description.", 1))
	for i := 0; i < 3; i++ {
		r := creq(b)
		r.Thread = itoa(i)
		o := Apply(e, r)
		if o.Changed || o.Why != "client_search_disabled" {
			t.Fatalf("%s", show(o))
		}
	}
	if e.Gate.DisabledUnknown.Load() != 1 {
		t.Fatalf("counted %d, want 1", e.Gate.DisabledUnknown.Load())
	}
}

// A break is a deferred tool's own error that says it is not loaded. A direct call that worked is a
// use, and so is a call to a tool tool_choice kept loaded.
func TestClientModeBreakRule(t *testing.T) {
	call := `{"role":"assistant","content":[{"type":"tool_use","id":"w1","name":"Workflow","input":{}}]}`
	bad := `{"role":"user","content":[{"type":"tool_result","tool_use_id":"w1","is_error":true,"content":"tool not loaded"}]}`
	fine := `{"role":"user","content":[{"type":"tool_result","tool_use_id":"w1","content":"done"}]}`
	run := func(forced bool, tail ...string) Outcome {
		e, _ := clientEnv(t, filepath.Join(t.TempDir(), FileName))
		Apply(e, creq(body(catalogue(), "")))
		b := body(catalogue(), "", tail...)
		if forced {
			b, _ = sjson.SetBytes(b, "tool_choice", map[string]string{"type": "tool", "name": "Workflow"})
		}
		return Apply(e, creq(b))
	}
	if o := run(false, call, bad); o.Changed || o.ClientDisabled != ReasonBreak {
		t.Fatalf("true break: %s (%q)", show(o), o.ClientDisabled)
	}
	if o := run(false, call, fine); !o.Changed || o.ClientDisabled != "" || strings.Join(o.Direct, ",") != "Workflow" {
		t.Fatalf("direct call: %s direct=%v", show(o), o.Direct)
	}
	if o := run(true, call, bad); !o.Changed || o.ClientDisabled != "" {
		t.Fatalf("call after tool_choice: %s", show(o))
	}
	ok := append(searchRound("t1", "select:Workflow", noMatch), call, fine)
	if o := run(false, ok...); !o.Changed || len(o.Direct) != 0 {
		t.Fatalf("good load: %s direct=%v", show(o), o.Direct)
	}
}

// An error result or an unknown shape books nothing and stays as the client wrote it.
func TestClientModeErrorResultBooksNothing(t *testing.T) {
	e, _ := clientEnv(t, filepath.Join(t.TempDir(), FileName))
	Apply(e, creq(body(catalogue(), "")))
	o := Apply(e, creq(body(catalogue(), "", searchRound("t1", "select:Workflow", `"Error: boom"`)...)))
	if len(o.Loads) != 0 || o.Answered != 0 || result(o.Body, 3) != `"Error: boom"` {
		t.Fatalf("error result: loads=%v answered=%d res=%s", o.Loads, o.Answered, result(o.Body, 3))
	}
	// The same call, answered "no match" later, is still answerable: nothing was stored for it.
	o = Apply(e, creq(body(catalogue(), "", searchRound("t1", "select:Workflow", noMatch)...)))
	if len(o.Loads) != 1 {
		t.Fatalf("not answered later: %v", o.Loads)
	}
}

// After a client-mode load, the weight of the tool leaves DeferredTokens, and the list is the client list.
func TestClientModeWeighsTheLoad(t *testing.T) {
	e, _ := clientEnv(t, filepath.Join(t.TempDir(), FileName))
	before := Apply(e, creq(body(catalogue(), "")))
	after := Apply(e, creq(body(catalogue(), "", searchRound("t1", "select:Workflow", noMatch)...)))
	if !(after.DeferredTokens < before.DeferredTokens) || after.ListTokens != before.ListTokens || before.ListTokens == 0 {
		t.Fatalf("deferred %d -> %d, list %d -> %d", before.DeferredTokens, after.DeferredTokens, before.ListTokens, after.ListTokens)
	}
	if before.ListTokens != tokens.Count(ClientToolNamesList(before.Deferred)) {
		t.Fatal("list weight is not the client list's")
	}
}

// The added list has the same bytes on every later request, also after a restart, and also when
// Claude Code's own result changes (the stored list wins).
func TestClientModeReplaysByteForByteAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	e, f := clientEnv(t, path)
	Apply(e, creq(body(catalogue(), "")))
	rounds := searchRound("t2", "select:Workflow", noMatch)
	one := Apply(e, creq(body(catalogue(), "", rounds...)))
	if len(one.Loads) != 1 {
		t.Fatalf("not answered: %s", show(one))
	}
	more := append(rounds, `{"role":"assistant","content":"ok"}`, `{"role":"user","content":"next"}`)
	two := Apply(e, creq(body(catalogue(), "", more...)))
	if two.Answered != 0 || len(two.Loads) != 0 {
		t.Fatalf("a call was answered twice: %+v", two.Loads)
	}
	for _, i := range []string{"tools", "messages.1", "messages.3"} {
		if gjson.GetBytes(two.Body, i).Raw != gjson.GetBytes(one.Body, i).Raw {
			t.Fatalf("%s changed between requests", i)
		}
	}
	f.Close()
	e2, _ := envAt(t, hist{}, path)
	passProbe(e2, ckey)
	three := Apply(e2, creq(body(catalogue(), "", more...)))
	if string(three.Body) != string(two.Body) || three.Decided {
		t.Fatalf("restart changed the request: %s", show(three))
	}
}

// A thread that started with a server-side search keeps it: client mode is for new threads only.
func TestClientModeNeverChangesALiveThread(t *testing.T) {
	e, _ := clientEnv(t, filepath.Join(t.TempDir(), FileName))
	passProbe(e, ProbeKey{"u", "m", "r"})
	delete(e.Prober.m, ckey)
	first := Apply(e, creq(body(catalogue(), "")))
	if first.Client || !strings.Contains(string(first.Body), SearchName) {
		t.Fatalf("first: %s", show(first))
	}
	passProbe(e, ckey) // the client probe passes later
	later := Apply(e, creq(body(catalogue(), "", `{"role":"assistant","content":"ok"}`, `{"role":"user","content":"x"}`)))
	if later.Client || !strings.Contains(string(later.Body), SearchName) {
		t.Fatalf("live thread moved to client mode: %s", show(later))
	}
}

// A thread in client mode whose request lost ToolSearch is sent as it is.
func TestClientModeWithoutToolSearchSendsOriginal(t *testing.T) {
	e, _ := clientEnv(t, filepath.Join(t.TempDir(), FileName))
	Apply(e, creq(body(catalogue(), "")))
	var tools []string
	for _, n := range all {
		if n != "ToolSearch" {
			tools = append(tools, decl(n))
		}
	}
	o := Apply(e, creq(body(tools, "", `{"role":"assistant","content":"ok"}`, `{"role":"user","content":"x"}`)))
	if o.Changed || o.Why != "client_search_gone" {
		t.Fatalf("%s", show(o))
	}
}

func TestClientProbeVerdict(t *testing.T) {
	b := ClientProbeBody("claude-sonnet-5-5", "ab12")
	if gjson.GetBytes(b, "tools.#(name==\"tool_search_tool_bm25\")").Exists() || !gjson.GetBytes(b, "tools.1.defer_loading").Bool() {
		t.Fatalf("probe body: %s", b)
	}
	if gjson.GetBytes(b, "messages.2.content.0.content.0.tool_name").String() != "cg_probe_ab12" {
		t.Fatal("no tool_reference to the probe tool")
	}
	if ClientProbeVerdict([]byte(`{"content":[{"type":"tool_use","name":"cg_probe_ab12","input":{"code":7}}]}`), "ab12") != Pass {
		t.Fatal("a call to the loaded tool must pass")
	}
	if ClientProbeVerdict([]byte(`{"content":[{"type":"tool_use","name":"ToolSearch","input":{"query":"x"}}]}`), "ab12") != Fail {
		t.Fatal("searching again must fail")
	}
}
