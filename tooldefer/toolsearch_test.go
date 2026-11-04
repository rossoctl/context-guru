package tooldefer

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
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

// A pure Claude Code query passes through; a query that touches a proxy-deferred tool is answered.
func TestClientModeAnswersOnlyProxyQueries(t *testing.T) {
	e, _ := clientEnv(t, filepath.Join(t.TempDir(), FileName))
	first := Apply(e, creq(body(catalogue(), "")))
	if strings.Join(first.Deferred, ",") != "Monitor,Workflow,mcp__a__b" {
		t.Fatalf("set: %v", first.Deferred)
	}
	// CronList is deferred by the client itself.
	cc := `{"name":"CronList","description":"List cron jobs","input_schema":{"type":"object"},"defer_loading":true}`
	tools := append(catalogue(), cc)
	own := searchRound("t1", "select:CronList", `[{"type":"tool_reference","tool_name":"CronList"}]`)
	o := Apply(e, creq(body(tools, "", own...)))
	if len(o.Loads) != 0 || strings.Contains(string(o.Body), `"tool_name":"Workflow"`) {
		t.Fatalf("a pure client query was changed: %s", gjson.GetBytes(o.Body, "messages").Raw)
	}
	if gjson.GetBytes(o.Body, "messages.3.content.0.content").Raw != `[{"type":"tool_reference","tool_name":"CronList"}]` {
		t.Fatal("client result did not stay as it was")
	}
	// A proxy tool, with the client's own "not found" result.
	prox := searchRound("t2", "select:Workflow,CronList", `"No matching deferred tools found"`)
	o = Apply(e, creq(body(tools, "", prox...)))
	got := gjson.GetBytes(o.Body, "messages.3.content.0.content").Raw
	want := `[{"type":"tool_reference","tool_name":"Workflow"},{"type":"tool_reference","tool_name":"CronList"}]`
	if got != want || strings.Join(o.Loads, ",") != "Workflow" || o.Answered != 1 {
		t.Fatalf("answer = %s loads=%v answered=%d", got, o.Loads, o.Answered)
	}
	// A keyword query finds a proxy tool too.
	kw := searchRound("t3", "workflow orchestrates", `"No matching deferred tools found"`)
	o = Apply(e, creq(body(tools, "", kw...)))
	if got := gjson.GetBytes(o.Body, "messages.3.content.0.content.0.tool_name").String(); got != "Workflow" {
		t.Fatalf("keyword answer = %s", gjson.GetBytes(o.Body, "messages.3").Raw)
	}
}

// The answer has the same bytes on every later request, also when the history or the cands change,
// and also after a restart.
func TestClientModeReplaysByteForByteAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	e, f := clientEnv(t, path)
	Apply(e, creq(body(catalogue(), "")))
	rounds := searchRound("t2", "select:Workflow", `"No matching deferred tools found"`)
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
