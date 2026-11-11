package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/adjudicate"
	"github.com/rossoctl/context-guru/internal/tokens"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
	"github.com/rossoctl/context-guru/tooldefer"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Tool deferral in the Bob plugin's bob-ultrahigh preset, offline, on Bob's own route (plan P2). The
// preset text is testdata/bob_ultrahigh.yaml; the plugin's test checks that BOB_PRESETS holds the
// same text, and bobultrahigh_test.go that it is ultrahigh with the 4 Bob changes.
// The tools are Bob 2.3.0's own (testdata/bob_tools.json says where they come from).
//
// Limits of the fixture: a one-line system prompt (Bob's real one is long), no MCP tools, and the
// tool set of a single mode. A mode switch in the middle of a task, which changes `tools`, is not
// covered.

// bobKeep is the preset's keep list. bobCandidates are the tools it may defer.
var (
	bobKeep = []string{"read_file", "write_file", "apply_diff", "search_and_replace", "insert_content",
		"execute_command", "grep", "glob", "list_files", "ask_followup_question", "use_skill",
		"switch_mode", "update_todo_list", "spawn_subagent", "start_subtask"}
	bobCandidates = []string{"FindReferencingSymbols", "FindSymbol", "create_chart", "create_html_artifact",
		"office_edit", "office_read", "search_ibm_docs"}
)

func bobFixtureTools(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "bob_tools.json"))
	if err != nil {
		t.Fatal(err)
	}
	tools := gjson.GetBytes(b, "tools")
	if n := len(tools.Array()); n != len(bobKeep)+len(bobCandidates) {
		t.Fatalf("fixture has %d tools, want %d", n, len(bobKeep)+len(bobCandidates))
	}
	return tools.Raw
}

// bobDeferBody is a Chat Completions request shaped like Bob's. extra messages follow the user's.
func bobDeferBody(t *testing.T, extra ...string) []byte {
	t.Helper()
	msgs := `{"role":"system","content":"You are Bob, a coding agent."},{"role":"user","content":"Fix the failing test."}`
	for _, m := range extra {
		msgs += "," + m
	}
	return []byte(`{"model":"premium-ide","stream":false,"tools":` + bobFixtureTools(t) + `,"messages":[` + msgs + `]}`)
}

// bobDeferHandler runs the preset on Bob's route. used, per past session, names the tools that
// session called; every past session declared all 22 tools.
func bobDeferHandler(t *testing.T, preset []byte, upstream string, used ...[]string) *Handler {
	t.Helper()
	h := bobDeferHandlerBare(t, preset, upstream)
	ds := h.toolDefer()
	for i, s := range []string{"p1", "p2", "p3"} {
		var calls []string
		if i < len(used) {
			for j, n := range used[i] {
				calls = append(calls, csCall("u"+itoa(j), n, `{}`))
			}
		}
		var extra []string
		if len(calls) > 0 {
			extra = append(extra, `{"role":"assistant","content":null,"tool_calls":[`+strings.Join(calls, ",")+`]}`)
		}
		ds.hist.Observe("", s, "", tooldefer.View("openai", bobDeferBody(t, extra...)), int64(i+1))
	}
	return h
}

// bobDeferHandlerBare is the handler with no history: only real requests build it.
func bobDeferHandlerBare(t *testing.T, preset []byte, upstream string) *Handler {
	t.Helper()
	cfg, err := config.LoadBytes(preset)
	if err != nil {
		t.Fatal(err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{BobUpstream: upstream,
		ToolDeferPath: filepath.Join(t.TempDir(), tooldefer.FileName), Prices: tdPrices{}})
	t.Cleanup(h.Close)
	return h
}

func bobPreset(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "bob_ultrahigh.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func bobDeferPost(t *testing.T, srv *httptest.Server, task string, body []byte) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+bobModelPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer bob-test")
	req.Header.Set("X-Task-Id", task)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(b)
}

// removedTools is the tools in sent's input that sent's `tools` no longer declares, sorted.
func removedTools(t *testing.T, sent []byte) []string {
	t.Helper()
	now := map[string]bool{}
	for _, n := range strings.Split(csTools(sent), ",") {
		now[n] = true
	}
	var gone []string
	gjson.Get(bobFixtureTools(t), "#.function.name").ForEach(func(_, n gjson.Result) bool {
		if !now[n.String()] {
			gone = append(gone, n.String())
		}
		return true
	})
	sort.Strings(gone)
	return gone
}

// The history comes from Bob's own traffic: 3 tasks, told apart only by X-Task-Id (their first
// messages are the same, so a hash of the transcript would merge them into one session), each
// calling one keep-list tool. A 4th task then defers the 7 candidates. Nothing is seeded by hand.
func TestBobDeferHistoryFromThreeBobTasks(t *testing.T) {
	up := &csUpstream{script: []string{csText("ok")}}
	us := httptest.NewServer(up)
	defer us.Close()
	h := bobDeferHandlerBare(t, bobPreset(t), us.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	used := bobDeferBody(t, `{"role":"assistant","content":null,"tool_calls":[`+csCall("c1", "read_file", `{"path":"a.go"}`)+`]}`,
		`{"role":"tool","tool_call_id":"c1","content":"package a"}`)
	for _, task := range []string{"bob-task-a", "bob-task-b", "bob-task-c"} {
		if st, out := bobDeferPost(t, srv, task, used); st != http.StatusOK {
			t.Fatalf("%s: client got %d %s", task, st, out)
		}
	}
	bobDeferPost(t, srv, "bob-task-d", bobDeferBody(t))
	b := up.all()
	if got := strings.Join(removedTools(t, b[len(b)-1]), ","); got != strings.Join(bobCandidates, ",") {
		t.Fatalf("4th task deferred %q, want %s: the 3 tasks must be 3 sessions (X-Task-Id)",
			got, strings.Join(bobCandidates, ","))
	}
}

// T1 and the deferred set: the 7 heavy tools no past session used are removed, our search tool is
// declared, and the names list goes in after Bob's system message.
func TestBobDeferRemovesTheUnusedHeavyTools(t *testing.T) {
	up := &csUpstream{script: []string{csText("ok")}}
	us := httptest.NewServer(up)
	defer us.Close()
	h := bobDeferHandler(t, bobPreset(t), us.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	orig := bobDeferBody(t)
	if st, out := bobDeferPost(t, srv, "bob-task-1", orig); st != http.StatusOK {
		t.Fatalf("client got %d %s", st, out)
	}
	sent := up.all()[0]
	// The preset has offload components and the sweep, so the proxy also declares its expand and
	// adjudicate tools: Bob's 15 kept tools, then our 3, in this order.
	if got, want := csTools(sent), strings.Join(append(append([]string{}, bobKeep...),
		expand.ToolName, adjudicate.ToolName, tooldefer.ClientSearchName), ","); got != want {
		t.Fatalf("tools sent = %s, want %s", got, want)
	}
	if !tooldefer.HasClientSearch("openai", sent) {
		t.Fatalf("no %s in tools: %s", tooldefer.ClientSearchName, csTools(sent))
	}
	if got := strings.Join(removedTools(t, sent), ","); got != strings.Join(bobCandidates, ",") {
		t.Fatalf("deferred = %s, want the unused candidates %s", got, strings.Join(bobCandidates, ","))
	}
	// Every deferred tool is over the rule's floor, weighed on Bob's own declaration.
	gjson.GetBytes(orig, "tools").ForEach(func(_, tl gjson.Result) bool {
		n := tl.Get("function.name").String()
		for _, c := range bobCandidates {
			if c == n && tokens.Count(tl.Raw) < tooldefer.MinTokens {
				t.Errorf("%s weighs %d tokens, under the %d floor", n, tokens.Count(tl.Raw), tooldefer.MinTokens)
			}
		}
		return true
	})
	if got := gjson.GetBytes(sent, "messages.1.content").String(); got != tooldefer.ClientNamesList(bobCandidates) {
		t.Fatalf("messages[1] is not the names list: %q", got)
	}
	if gjson.GetBytes(sent, "messages.0.content").String() != "You are Bob, a coding agent." {
		t.Fatalf("Bob's system message moved: %s", gjson.GetBytes(sent, "messages").Raw)
	}
}

// A candidate a past session used stays loaded: the set is the unused candidates only.
func TestBobDeferKeepsACandidateAPastSessionUsed(t *testing.T) {
	up := &csUpstream{script: []string{csText("ok")}}
	us := httptest.NewServer(up)
	defer us.Close()
	h := bobDeferHandler(t, bobPreset(t), us.URL, nil, []string{"create_chart"})
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	bobDeferPost(t, srv, "bob-task-1", bobDeferBody(t))
	want := "FindReferencingSymbols,FindSymbol,create_html_artifact,office_edit,office_read,search_ibm_docs"
	if got := strings.Join(removedTools(t, up.all()[0]), ","); got != want {
		t.Fatalf("deferred = %s, want %s", got, want)
	}
}

// T2: no keep-list tool is removed, though no past session used any of them.
func TestBobDeferNeverRemovesAKeepListTool(t *testing.T) {
	up := &csUpstream{script: []string{csText("ok")}}
	us := httptest.NewServer(up)
	defer us.Close()
	h := bobDeferHandler(t, bobPreset(t), us.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	bobDeferPost(t, srv, "bob-task-1", bobDeferBody(t))
	sent := map[string]bool{}
	for _, n := range strings.Split(csTools(up.all()[0]), ",") {
		sent[n] = true
	}
	for _, k := range bobKeep {
		if !sent[k] {
			t.Errorf("keep-list tool %s was removed", k)
		}
	}
}

// A streamed reply that calls our search is answered in band on Bob's route: Bob gets the tool's
// own call, never ours, and the follow-up declares the tool at the end of `tools`.
func TestBobDeferStreamedSearchIsAnsweredInBand(t *testing.T) {
	r1 := ccRole("r1") + ccCallStart("r1", 0, "c1", tooldefer.ClientSearchName) +
		ccCallArgs("r1", 0, `{"names":["office_edit"]}`) + ccFinish("r1", "tool_calls") + ccUsage("r1") + ccDone
	r2 := ccRole("r2") + ccCallStart("r2", 0, "c2", "office_edit") + ccCallArgs("r2", 0, `{"path":"/w/a.docx"}`) +
		ccFinish("r2", "tool_calls") + ccUsage("r2") + ccDone
	up := &csUpstream{script: []string{r1, r2}}
	us := httptest.NewServer(up)
	defer us.Close()
	h := bobDeferHandler(t, bobPreset(t), us.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	body, _ := sjson.SetBytes(bobDeferBody(t), "stream", true)
	st, out := bobDeferPost(t, srv, "bob-task-1", body)
	if st != http.StatusOK || strings.Contains(out, tooldefer.ClientSearchName) {
		t.Fatalf("client got %d %q", st, out)
	}
	msg, ok := expand.AggregateSSE("openai", []byte(out))
	if !ok || gjson.GetBytes(msg, "choices.0.message.tool_calls.#").Int() != 1 ||
		gjson.GetBytes(msg, "choices.0.message.tool_calls.0.function.name").String() != "office_edit" {
		t.Fatalf("Bob's answer: %s", msg)
	}
	b := up.all()
	if len(b) != 2 || !strings.HasSuffix(csTools(b[1]), tooldefer.ClientSearchName+",office_edit") {
		t.Fatalf("rounds = %d, follow-up tools = %s", len(b), csTools(b[len(b)-1]))
	}
	if !strings.Contains(gjson.GetBytes(b[1], "messages.@reverse.0.content").String(), "Loaded: office_edit") {
		t.Fatalf("follow-up has no answer: %s", gjson.GetBytes(b[1], "messages").Raw)
	}
	if g := h.deferGate(); g.AddBacks.Load() != 1 {
		t.Fatalf("add_backs = %d, want 1", g.AddBacks.Load())
	}
}

// KNOWN GAP. Bob's "Tool does not exist" reply does NOT turn deferral off on this route. This pins
// today's behavior; it is not a decision. The "does not exist" check (tooldefer.ReadCalls) runs only on the
// Anthropic client-search path (tooldefer.Apply), not on our own search (ApplySearch). Here a
// direct call to a removed tool costs the model a turn; the names list tells it to search first.
// The thread keeps its set, so its cached prefix stays.
func TestBobDeferKnownGapDirectCallIsNotDetected(t *testing.T) {
	up := &csUpstream{script: []string{csText("ok")}}
	us := httptest.NewServer(up)
	defer us.Close()
	h := bobDeferHandler(t, bobPreset(t), us.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	bobDeferPost(t, srv, "bob-task-1", bobDeferBody(t))
	// Bob 2.3.0's reply to a call of a tool it does not have (extension.js).
	next := bobDeferBody(t, `{"role":"assistant","content":null,"tool_calls":[`+csCall("c1", "office_edit", `{}`)+`]}`,
		`{"role":"tool","tool_call_id":"c1","content":"Tool does not exist. Only call available tools."}`)
	bobDeferPost(t, srv, "bob-task-1", next)
	b := up.all()
	if got := strings.Join(removedTools(t, b[len(b)-1]), ","); got != strings.Join(bobCandidates, ",") {
		t.Fatalf("KNOWN GAP: after Bob's \"Tool does not exist\" the set is %q, today it stays unchanged. "+
			"If this fails because the gap was fixed, update this test.", got)
	}
	if g := h.deferGate(); g.DisabledBreak.Load() != 0 || g.TurnedOff.Load() != 0 {
		t.Fatalf("KNOWN GAP: break=%d turned_off=%d, today both stay 0. "+
			"If this fails because the gap was fixed, update this test.", g.DisabledBreak.Load(), g.TurnedOff.Load())
	}
}

// The off-switch that does work on Bob's route is a 400: the gateway refuses the changed request,
// the proxy sends the original, Bob gets that answer, and deferral turns off for the model (/stats
// tool_defer.turned_off). A new thread then defers nothing.
func TestBobDefer400TurnsDeferralOff(t *testing.T) {
	up := &csUpstream{script: []string{csText("ok")}, reject400: true}
	us := httptest.NewServer(up)
	defer us.Close()
	h := bobDeferHandler(t, bobPreset(t), us.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	st, out := bobDeferPost(t, srv, "bob-task-1", bobDeferBody(t))
	if st != http.StatusOK || gjson.Get(out, "choices.0.message.content").String() != "ok" {
		t.Fatalf("client got %d %s", st, out)
	}
	b := up.all()
	if len(b) != 2 || removedTools(t, b[1]) != nil {
		t.Fatalf("rounds = %d; the retry must be the original", len(b))
	}
	if got := h.deferGate().TurnedOff.Load(); got != 1 {
		t.Fatalf("turned_off = %d, want 1", got)
	}
	bobDeferPost(t, srv, "bob-task-2", bobDeferBody(t))
	b = up.all()
	if len(b) != 3 || removedTools(t, b[2]) != nil {
		t.Fatalf("a new thread after the turn-off still defers: rounds = %d", len(b))
	}
}
