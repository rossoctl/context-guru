package apply_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func autoDecl(name string) string {
	return fmt.Sprintf(`{"name":%q,"description":"Does the %s job. %s","input_schema":{"type":"object","properties":{"x":{"type":"string","description":"%s"}}}}`,
		name, name, strings.Repeat("Detailed usage notes. ", 12), strings.Repeat("param doc ", 20))
}

// autoBody: Bash (named in prose), Read (called), three hidden-eligible tools.
func autoBody(assistant bool, extra string) []byte {
	msgs := `[{"role":"user","content":"hi"}`
	if assistant {
		msgs += `,{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}]},` +
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}`
	}
	msgs += `]`
	return []byte(`{"model":"claude-opus-5","tools":[` + strings.Join([]string{
		autoDecl("Bash"), autoDecl("Read"), autoDecl("mcp__a__one"), autoDecl("mcp__a__two"), autoDecl("Artifact")}, ",") +
		`],"system":[{"type":"text","text":"Use Bash for shell work."}],` + extra + `"messages":` + msgs + `}`)
}

func toolNamesOf(b []byte) string {
	var n []string
	gjson.GetBytes(b, "tools").ForEach(func(_, t gjson.Result) bool { n = append(n, t.Get("name").String()); return true })
	return strings.Join(n, ",")
}

var elig = []string{"Bash", "context_guru_expand", "mcp__a__one", "mcp__a__two", "Artifact"}

func TestAutoFirstTurnRemovesAndFixesSet(t *testing.T) {
	st := store.NewMemory(store.Options{})
	out, tok, n := apply.AutoFilterTools(st, "s", autoBody(false, ""), elig, false)
	if got := toolNamesOf(out); got != "Bash,Read,context_guru_tools" || n != 3 || tok <= 0 {
		t.Fatalf("first turn: tools=%s n=%d tok=%d", got, n, tok)
	}
	// Later turns, history now says nothing is eligible: set must not change (byte-identical).
	a, _, _ := apply.AutoFilterTools(st, "s", autoBody(true, ""), nil, false)
	b, _, _ := apply.AutoFilterTools(st, "s", autoBody(true, ""), []string{"Bash"}, false)
	if gjson.GetBytes(a, "tools").Raw != gjson.GetBytes(out, "tools").Raw || gjson.GetBytes(a, "tools").Raw != gjson.GetBytes(b, "tools").Raw {
		t.Fatalf("tools drifted:\n%s\n%s", toolNamesOf(out), toolNamesOf(a))
	}
}

func TestAutoWarmUndecidedSessionUntouchedColdDecides(t *testing.T) {
	st := store.NewMemory(store.Options{})
	body := autoBody(true, "")
	if out, _, n := apply.AutoFilterTools(st, "w", body, elig, false); n != 0 || string(out) != string(body) {
		t.Fatal("warm mid-session request must be untouched")
	}
	out, _, n := apply.AutoFilterTools(st, "w", body, elig, true)
	if n == 0 || strings.Contains(toolNamesOf(out), "mcp__a__one") == false && !strings.Contains(toolNamesOf(out), "context_guru_tools") {
		t.Fatalf("cold turn should decide, got %s", toolNamesOf(out))
	}
}

func TestAutoNeverRemoves(t *testing.T) {
	st := store.NewMemory(store.Options{})
	out, _, _ := apply.AutoFilterTools(st, "n", autoBody(true, ""), append([]string{"Read"}, elig...), true)
	got := toolNamesOf(out)
	for _, keep := range []string{"Bash", "Read"} { // prose-named, called this session
		if !strings.Contains(got, keep) {
			t.Errorf("%s removed: %s", keep, got)
		}
	}
	// tool_choice forcing: no decision at all.
	body := autoBody(false, `"tool_choice":{"type":"tool","name":"Artifact"},`)
	if o, _, n := apply.AutoFilterTools(store.NewMemory(store.Options{}), "f", body, elig, false); n != 0 || string(o) != string(body) {
		t.Fatal("forcing tool_choice must leave the request alone")
	}
}

func TestAutoNeverWorseGuard(t *testing.T) {
	tiny := []byte(`{"tools":[{"name":"Bash","description":"b"},{"name":"X","description":"x"}],"messages":[{"role":"user","content":"hi"}]}`)
	if o, _, n := apply.AutoFilterTools(store.NewMemory(store.Options{}), "g", tiny, []string{"X"}, false); n != 0 || string(o) != string(tiny) {
		t.Fatal("removal smaller than the stub must not happen")
	}
}

func TestAutoRestoreIsOneRewriteThenStable(t *testing.T) {
	st := store.NewMemory(store.Options{})
	first, _, _ := apply.AutoFilterTools(st, "r", autoBody(false, ""), elig, false)
	// expand injected after (proxy order) and a restore of one name.
	withExpand, _ := sjson.SetRawBytes(first, "tools.-1", []byte(`{"name":"context_guru_expand"}`))
	cont, ans := apply.RestoreTools(st, "r", withExpand, []string{"mcp__a__one", "nope"})
	if !strings.Contains(ans, "Restored: mcp__a__one") || !strings.Contains(ans, "nope") {
		t.Fatalf("answer: %s", ans)
	}
	if got := toolNamesOf(cont); got != "Bash,Read,mcp__a__one,context_guru_tools,context_guru_expand" {
		t.Fatalf("continuation tools: %s", got)
	}
	// The client's next request must produce the same bytes as the continuation (no 2nd rewrite).
	next, _, _ := apply.AutoFilterTools(st, "r", autoBody(true, ""), nil, false)
	next, _ = sjson.SetRawBytes(next, "tools.-1", []byte(`{"name":"context_guru_expand"}`)) // expand.Inject runs after
	if gjson.GetBytes(next, "tools").Raw != gjson.GetBytes(cont, "tools").Raw {
		t.Fatalf("restore not byte-stable:\n%s\n%s", toolNamesOf(cont), toolNamesOf(next))
	}
	// Restoring everything drops the stub and equals the client's original declarations.
	all, _ := apply.RestoreTools(st, "r", cont, []string{"mcp__a__two", "Artifact"})
	want, _ := sjson.SetRawBytes(autoBody(false, ""), "tools.-1", []byte(`{"name":"context_guru_expand"}`))
	if gjson.GetBytes(all, "tools").Raw != gjson.GetBytes(want, "tools").Raw {
		t.Fatalf("full restore != original: %s", toolNamesOf(all))
	}
}

func TestRestoreCalls(t *testing.T) {
	msg := []byte(`{"content":[{"type":"tool_use","id":"c1","name":"context_guru_tools","input":{"names":["a","b"]}},{"type":"text","text":"x"}]}`)
	c := apply.RestoreCalls(msg)
	if len(c) != 1 || c[0].CallID != "c1" || len(c[0].Names) != 2 {
		t.Fatalf("%+v", c)
	}
}

// A restricted subagent thread shares the session with the main thread but has a narrower tool list.
// Its stub must name only what was removed from ITS tools, and a restore must never add a tool its
// allowlist excluded (Bash, or a hidden tool it was never given).
func TestAutoSubagentThreadIsNeverWidened(t *testing.T) {
	st := store.NewMemory(store.Options{})
	if _, _, n := apply.AutoFilterTools(st, "sub", autoBody(false, ""), elig, false); n == 0 {
		t.Fatal("main thread should decide")
	}
	subTools := autoDecl("Read") + "," + autoDecl("Artifact")
	sub := []byte(`{"model":"m","tools":[` + subTools + `],"system":"x","messages":[{"role":"user","content":"hi"}]}`)
	out, _, n := apply.AutoFilterTools(st, "sub", sub, nil, false)
	if got := toolNamesOf(out); n != 1 || got != "Read,context_guru_tools" {
		t.Fatalf("subagent tools: %s (n=%d)", got, n)
	}
	enum := gjson.GetBytes(out, "tools.1.input_schema.properties.names.items.enum").Raw
	if enum != `["Artifact"]` || strings.Contains(gjson.GetBytes(out, "tools.1.description").String(), "mcp__a__one") {
		t.Fatalf("stub leaks the main thread's hidden tools: %s", gjson.GetBytes(out, "tools.1").Raw)
	}
	// A hidden tool this thread was never given cannot be restored into it.
	same, ans := apply.RestoreTools(st, "sub", out, []string{"mcp__a__one", "Bash"})
	if toolNamesOf(same) != "Read,context_guru_tools" || !strings.Contains(ans, "nothing changed") {
		t.Fatalf("widened: %s / %s", toolNamesOf(same), ans)
	}
	// Its own removal is restored in its original relative position, and nothing else appears.
	back, _ := apply.RestoreTools(st, "sub", out, []string{"Artifact"})
	if got := toolNamesOf(back); got != "Read,Artifact" {
		t.Fatalf("restore: %s", got)
	}
}
