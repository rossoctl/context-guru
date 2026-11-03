package tooldefer

import (
	"fmt"
	"strings"
	"testing"

	cgschema "github.com/rossoctl/context-guru/schema"
	"github.com/tidwall/gjson"
)

// chatDecl and respDecl are a function tool well over MinTokens, on Chat Completions and Responses.
func chatDecl(name string) string {
	return fmt.Sprintf(`{"type":"function","function":{"name":%q,"description":"Does the %s job. %s","parameters":{"type":"object","properties":{"x":{"type":"string"}}}}}`,
		name, name, strings.Repeat("Detailed usage notes for this tool. ", 25))
}

func respDecl(name string) string {
	return fmt.Sprintf(`{"type":"function","name":%q,"description":"Does the %s job. %s","parameters":{"type":"object","properties":{"x":{"type":"string"}}}}`,
		name, name, strings.Repeat("Detailed usage notes for this tool. ", 25))
}

var gptTools = []string{"Bash", "Read", "Workflow", "Monitor"}

func chatBody(msgs ...string) []byte {
	var tools []string
	for _, n := range gptTools {
		tools = append(tools, chatDecl(n))
	}
	m := `{"role":"system","content":"You are a coding agent."},{"role":"user","content":"hi"}`
	for _, x := range msgs {
		m += "," + x
	}
	return []byte(`{"model":"gpt-5.5","tools":[` + strings.Join(tools, ",") + `],"messages":[` + m + `]}`)
}

func respBody(items ...string) []byte {
	var tools []string
	for _, n := range gptTools {
		tools = append(tools, respDecl(n))
	}
	in := `{"role":"user","content":"hi"}`
	for _, x := range items {
		in += "," + x
	}
	return []byte(`{"model":"gpt-5.5","instructions":"You are a coding agent.","tools":[` + strings.Join(tools, ",") +
		`],"input":[` + in + `]}`)
}

// unusedHist is three past sessions that declared Workflow and Monitor and used neither.
func unusedHist() hist {
	d := []string{"Bash", "Read", "Workflow", "Monitor"}
	return hist{sessions: []Session{sess(d, "Bash"), sess(d), sess(d)}}
}

func sreq(b []byte) Req {
	return Req{Session: "s", Project: "/w", Model: "gpt-5.5", Orig: b}
}

func wireToolNames(wire string, b []byte) string {
	var out []string
	gjson.GetBytes(b, "tools").ForEach(func(_, t gjson.Result) bool {
		n, _ := toolName(wire, t)
		out = append(out, n)
		return true
	})
	return strings.Join(out, ",")
}

func TestViewIsAnthropicShaped(t *testing.T) {
	b := chatBody(`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"Bash","arguments":"{}"}}]}`)
	v := View(cgschema.WireOpenAI, b)
	if got := names(v); got != "Bash,Read,Workflow,Monitor" {
		t.Fatalf("view tools = %s", got)
	}
	if !usedInTranscript(v)["Bash"] {
		t.Fatal("the view lost the Bash call")
	}
	if n := DeclaredNames(v); !n["Workflow"] || len(n) != 4 {
		t.Fatalf("declared = %v", n)
	}
	r := View(cgschema.WireResponses, respBody(`{"type":"function_call","call_id":"c1","name":"Read","arguments":"{}"}`))
	if !usedInTranscript(r)["Read"] || names(r) != "Bash,Read,Workflow,Monitor" {
		t.Fatalf("responses view: %s", r)
	}
}

func TestApplySearchRemovesAndLists(t *testing.T) {
	for _, wire := range []string{cgschema.WireOpenAI, cgschema.WireResponses} {
		e := env(t, unusedHist())
		b := chatBody()
		if wire == cgschema.WireResponses {
			b = respBody()
		}
		o, _ := ApplySearch(e, wire, sreq(b))
		if !o.Changed || !o.Decided || strings.Join(o.Deferred, ",") != "Monitor,Workflow" {
			t.Fatalf("%s: %s", wire, show(o))
		}
		if got := wireToolNames(wire, o.Body); got != "Bash,Read,"+ClientSearchName {
			t.Fatalf("%s: tools = %s", wire, got)
		}
		list := ClientNamesList([]string{"Monitor", "Workflow"})
		switch wire {
		case cgschema.WireOpenAI:
			if gjson.GetBytes(o.Body, "messages.1.content").String() != list || gjson.GetBytes(o.Body, "messages.0.role").String() != "system" {
				t.Fatalf("names list not at messages[1]: %s", gjson.GetBytes(o.Body, "messages").Raw)
			}
		default:
			if gjson.GetBytes(o.Body, "input.0.content.0.text").String() != list || gjson.GetBytes(o.Body, "input.0.role").String() != "developer" {
				t.Fatalf("names list not at input[0]: %s", gjson.GetBytes(o.Body, "input").Raw)
			}
		}
		if o.DeferredTokens < 2*MinTokens {
			t.Fatalf("%s: deferred tokens %d", wire, o.DeferredTokens)
		}
		// A later turn: the same bytes for tools and the list.
		later := chatBody(`{"role":"assistant","content":"ok"}`, `{"role":"user","content":"more"}`)
		if wire == cgschema.WireResponses {
			later = respBody(`{"role":"assistant","content":"ok"}`, `{"role":"user","content":"more"}`)
		}
		o2, _ := ApplySearch(e, wire, sreq(later))
		if o2.Decided || gjson.GetBytes(o2.Body, "tools").Raw != gjson.GetBytes(o.Body, "tools").Raw {
			t.Fatalf("%s: tools moved: %s", wire, show(o2))
		}
	}
}

func TestApplySearchLeavesAnthropicAndOtherCasesAlone(t *testing.T) {
	e := env(t, unusedHist())
	r := sreq(chatBody())
	r.Model = "claude-sonnet-5-5"
	if o, _ := ApplySearch(e, cgschema.WireOpenAI, r); o.Changed || o.Why != "anthropic" {
		t.Fatalf("claude: %s", show(o))
	}
	r = sreq(chatBody())
	r.AccountOff = true
	if o, _ := ApplySearch(e, cgschema.WireOpenAI, r); o.Changed || o.Why != "account_off" {
		t.Fatalf("account off: %s", show(o))
	}
	if o, _ := ApplySearch(e, cgschema.WireAnthropic, sreq(chatBody())); o.Changed {
		t.Fatalf("anthropic wire: %s", show(o))
	}
	b := []byte(strings.Replace(string(respBody()), `"input":`, `"previous_response_id":"r1","input":`, 1))
	if o, _ := ApplySearch(e, cgschema.WireResponses, sreq(b)); o.Changed || o.Why != "previous_response_id" {
		t.Fatalf("previous_response_id: %s", show(o))
	}
	// Without 3 sessions of history nothing is removed.
	e2 := env(t, hist{})
	if o, _ := ApplySearch(e2, cgschema.WireOpenAI, sreq(chatBody())); o.Changed {
		t.Fatalf("no history: %s", show(o))
	}
	// A 400 turn-off.
	e.Gate.Break("", "gpt-5.5")
	if o, _ := ApplySearch(e, cgschema.WireOpenAI, sreq(chatBody())); o.Changed || o.Why != "turned_off_after_400" {
		t.Fatalf("broken: %s", show(o))
	}
}

// A tool the search added back stays, at the end of tools, on every later request, and the follow-up
// the host builds carries the same tools.
func TestAddBackKeepsTheToolAtTheEnd(t *testing.T) {
	for _, wire := range []string{cgschema.WireOpenAI, cgschema.WireResponses} {
		e := env(t, unusedHist())
		b := chatBody()
		if wire == cgschema.WireResponses {
			b = respBody()
		}
		o, _ := ApplySearch(e, wire, sreq(b))
		rs := e.RecordStore()
		if now := AddBack(rs, "s", "", []string{"Workflow", "Bash", "Workflow"}); strings.Join(now, ",") != "Workflow" {
			t.Fatalf("%s: added now = %v", wire, now)
		}
		if now := AddBack(rs, "s", "", []string{"Workflow"}); len(now) != 0 {
			t.Fatalf("%s: added twice: %v", wire, now)
		}
		follow, ok := AppendTools(wire, o.Body, b, []string{"Workflow"})
		if !ok {
			t.Fatal("append failed")
		}
		o2, _ := ApplySearch(e, wire, sreq(b))
		if got := wireToolNames(wire, o2.Body); got != "Bash,Read,"+ClientSearchName+",Workflow" {
			t.Fatalf("%s: tools after add-back = %s", wire, got)
		}
		if gjson.GetBytes(o2.Body, "tools").Raw != gjson.GetBytes(follow, "tools").Raw {
			t.Fatalf("%s: the follow-up's tools differ from the next request's", wire)
		}
		if strings.Join(o2.Deferred, ",") != "Monitor" {
			t.Fatalf("%s: still removed = %v", wire, o2.Deferred)
		}
		// The names list keeps the thread's whole set: its bytes never change.
		if wire == cgschema.WireOpenAI && gjson.GetBytes(o2.Body, "messages.1").Raw != gjson.GetBytes(o.Body, "messages.1").Raw {
			t.Fatal("names list moved after an add-back")
		}
	}
}

func TestSearchCallsAndAnswer(t *testing.T) {
	chat := []byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[` +
		`{"id":"c1","type":"function","function":{"name":"context_guru_tools","arguments":"{\"names\":[\"Workflow\",\"Nope\"]}"}},` +
		`{"id":"c2","type":"function","function":{"name":"context_guru_expand","arguments":"{}"}}]}}]}`)
	calls, other := SearchCalls(cgschema.WireOpenAI, chat, "context_guru_expand")
	if other || len(calls) != 1 || calls[0].ID != "c1" || strings.Join(calls[0].Names, ",") != "Workflow,Nope" {
		t.Fatalf("calls=%v other=%v", calls, other)
	}
	if _, other := SearchCalls(cgschema.WireOpenAI, chat); !other {
		t.Fatal("expand counted as ours without being named")
	}
	resp := []byte(`{"output":[{"type":"reasoning"},{"type":"function_call","call_id":"c9","name":"context_guru_tools","arguments":"{\"names\":[\"Monitor\"]}"},` +
		`{"type":"function_call","call_id":"c8","name":"Bash","arguments":"{}"}]}`)
	calls, other = SearchCalls(cgschema.WireResponses, resp)
	if !other || len(calls) != 1 || calls[0].ID != "c9" {
		t.Fatalf("responses: calls=%v other=%v", calls, other)
	}
	a := SearchAnswer([]string{"Workflow", "Nope"}, map[string]bool{"Workflow": true})
	if !strings.Contains(a, "Loaded: Workflow.") || !strings.Contains(a, "Nope") {
		t.Fatalf("answer = %s", a)
	}
}

// A search the client received (made together with a client tool call) is answered on the next
// request, the tool is added back, and the repair has the same bytes on every request.
func TestApplySearchRepairsAClientAnsweredSearch(t *testing.T) {
	e := env(t, unusedHist())
	if o, _ := ApplySearch(e, cgschema.WireOpenAI, sreq(chatBody())); !o.Changed {
		t.Fatalf("first: %s", show(o))
	}
	b := chatBody(`{"role":"assistant","content":null,"tool_calls":[`+
		`{"id":"c1","type":"function","function":{"name":"context_guru_tools","arguments":"{\"names\":[\"Monitor\"]}"}},`+
		`{"id":"c2","type":"function","function":{"name":"Bash","arguments":"{}"}}]}`,
		`{"role":"tool","tool_call_id":"c1","content":"unknown tool: context_guru_tools"}`,
		`{"role":"tool","tool_call_id":"c2","content":"done"}`)
	o, loaded := ApplySearch(e, cgschema.WireOpenAI, sreq(b))
	if strings.Join(loaded, ",") != "Monitor" {
		t.Fatalf("loaded = %v (%s)", loaded, show(o))
	}
	want := SearchAnswer([]string{"Monitor"}, map[string]bool{"Monitor": true, "Workflow": true})
	if got := gjson.GetBytes(o.Body, "messages.4.content").String(); got != want {
		t.Fatalf("answer = %q", got)
	}
	if gjson.GetBytes(o.Body, "messages.5.content").String() != "done" {
		t.Fatal("the client's own tool result changed")
	}
	if !strings.HasSuffix(wireToolNames(cgschema.WireOpenAI, o.Body), ClientSearchName+",Monitor") {
		t.Fatalf("tools = %s", wireToolNames(cgschema.WireOpenAI, o.Body))
	}
	o2, loaded2 := ApplySearch(e, cgschema.WireOpenAI, sreq(b))
	if len(loaded2) != 0 || string(o2.Body) != string(o.Body) {
		t.Fatal("the repair is not byte-stable across requests")
	}
}

func TestApplySearchForcedToolIsAddedBack(t *testing.T) {
	e := env(t, unusedHist())
	ApplySearch(e, cgschema.WireOpenAI, sreq(chatBody()))
	b := []byte(strings.Replace(string(chatBody()), `"messages":`, `"tool_choice":{"type":"function","function":{"name":"Workflow"}},"messages":`, 1))
	o, loaded := ApplySearch(e, cgschema.WireOpenAI, sreq(b))
	if strings.Join(loaded, ",") != "Workflow" || !strings.Contains(wireToolNames(cgschema.WireOpenAI, o.Body), "Workflow") {
		t.Fatalf("forced tool not declared: %s loaded=%v", show(o), loaded)
	}
}

// Fail open: a body the rewrite cannot place a list in is sent as it is.
func TestApplySearchFailsOpen(t *testing.T) {
	e := env(t, unusedHist())
	b := []byte(strings.Replace(string(chatBody()), `"messages":[`, `"messages":{"bad":[`, 1))
	b = []byte(strings.TrimSuffix(string(b), "}") + "}}")
	o, _ := ApplySearch(e, cgschema.WireOpenAI, sreq(b))
	if o.Changed || string(o.Body) != string(b) {
		t.Fatalf("not fail open: %s", show(o))
	}
}

func TestAggregateChatSSE(t *testing.T) {
	raw := "data: {\"id\":\"x\",\"model\":\"gpt-5.5\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"context_guru_tools\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"names\\\":[\\\"Mon\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"itor\\\"]}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n"
	msg, ok := AggregateChatSSE([]byte(raw))
	if !ok {
		t.Fatal("not rebuilt")
	}
	calls, other := SearchCalls(cgschema.WireOpenAI, msg)
	if other || len(calls) != 1 || strings.Join(calls[0].Names, ",") != "Monitor" {
		t.Fatalf("calls=%v other=%v msg=%s", calls, other, msg)
	}
	if gjson.GetBytes(msg, "usage.prompt_tokens").Int() != 10 || gjson.GetBytes(msg, "choices.0.finish_reason").String() != "tool_calls" {
		t.Fatalf("msg = %s", msg)
	}
}

// Our search never sends defer_loading: through our gateway, GLM drops a Chat Completions tool that
// carries it inside `function`, and gpt-5.5 Responses refuses it without tool_search. A deferred tool
// is removed instead. Checked on every request of a thread: first, after an add-back, and a repair.
func TestApplySearchNeverSendsDeferLoading(t *testing.T) {
	for _, wire := range []string{cgschema.WireOpenAI, cgschema.WireResponses} {
		e := env(t, unusedHist())
		b := chatBody()
		if wire == cgschema.WireResponses {
			b = respBody()
		}
		o, _ := ApplySearch(e, wire, sreq(b))
		if !o.Changed {
			t.Fatalf("%s: %s", wire, show(o))
		}
		AddBack(e.RecordStore(), "s", "", []string{"Workflow"})
		o2, _ := ApplySearch(e, wire, sreq(b))
		follow, _ := AppendTools(wire, o.Body, b, []string{"Workflow"})
		for i, out := range [][]byte{o.Body, o2.Body, follow} {
			if strings.Contains(string(out), "defer_loading") {
				t.Fatalf("%s: body %d carries defer_loading: %s", wire, i, out)
			}
		}
	}
}
