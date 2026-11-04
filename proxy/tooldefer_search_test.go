package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
	"github.com/rossoctl/context-guru/tooldefer"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Our own tool search (#825), end to end on Chat Completions: a GPT model, the ultrahigh preset and
// three past sessions that declared Workflow and Monitor and used neither.

func csDecl(name string) string {
	return fmt.Sprintf(`{"type":"function","function":{"name":%q,"description":"Does the %s job. %s","parameters":{"type":"object","properties":{"x":{"type":"string"}}}}}`,
		name, name, strings.Repeat("Detailed usage notes for this tool. ", 25))
}

func csBody(session string, extra ...string) []byte {
	msgs := `{"role":"system","content":"You are a coding agent."},{"role":"user","content":"hi"}`
	for _, m := range extra {
		msgs += "," + m
	}
	return []byte(`{"model":"gpt-5.5","max_tokens":64,"user":"` + session + `","tools":[` + csDecl("Bash") + `,` + csDecl("Read") + `,` +
		csDecl("Workflow") + `,` + csDecl("Monitor") + `],"messages":[` + msgs + `]}`)
}

// csUpstream answers like an OpenAI-compatible gateway. Each answer is taken from script in turn
// (the last one repeats); reject400 refuses any body that declares our search tool.
type csUpstream struct {
	mu        sync.Mutex
	bodies    [][]byte
	script    []string
	reject400 bool
}

func (u *csUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.bodies = append(u.bodies, b)
	i := len(u.bodies) - 1
	if i >= len(u.script) {
		i = len(u.script) - 1
	}
	answer, reject := u.script[i], u.reject400
	u.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if strings.HasPrefix(answer, "data:") || strings.HasPrefix(answer, "event:") {
		w.Header().Set("Content-Type", "text/event-stream")
	}
	if reject && bytes.Contains(b, []byte(tooldefer.ClientSearchName)) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"Invalid tools","type":"invalid_request_error"}}`)
		return
	}
	fmt.Fprint(w, answer)
}

func (u *csUpstream) all() [][]byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([][]byte(nil), u.bodies...)
}

func csText(s string) string {
	return `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"` + s +
		`"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":64}}}`
}

func csCalls(calls ...string) string {
	return `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[` +
		strings.Join(calls, ",") + `]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":5}}`
}

func csCall(id, name, args string) string {
	return fmt.Sprintf(`{"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}`, id, name, args)
}

func csHandler(t *testing.T, preset, upstream string) *Handler {
	t.Helper()
	cfg, err := config.LoadBytes([]byte("preset: " + preset + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{OpenAIUpstream: upstream,
		ToolDeferPath: filepath.Join(t.TempDir(), tooldefer.FileName), Prices: tdPrices{}})
	t.Cleanup(h.Close)
	ds := h.toolDefer()
	for i, s := range []string{"p1", "p2", "p3"} {
		ds.hist.Observe("", s, "", tooldefer.View("openai", csBody(s)), int64(i+1))
	}
	return h
}

func csPost(t *testing.T, srv *httptest.Server, session string, body []byte) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/openai/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("x-context-guru-session", session)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(b)
}

func csTools(b []byte) string {
	var out []string
	gjson.GetBytes(b, "tools").ForEach(func(_, t gjson.Result) bool {
		out = append(out, t.Get("function.name").String())
		return true
	})
	return strings.Join(out, ",")
}

// The search is answered in band: the follow-up declares the tool, the client gets the model's call
// to it, and the tool stays declared on the next request, with the same tools bytes.
func TestClientSearchInBand(t *testing.T) {
	up := &csUpstream{script: []string{
		csCalls(csCall("c1", tooldefer.ClientSearchName, `{"names":["Workflow"]}`)),
		csCalls(csCall("c2", "Workflow", `{"x":"go"}`)),
		csText("done"),
	}}
	us := httptest.NewServer(up)
	defer us.Close()
	h := csHandler(t, "ultrahigh", us.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	st, out := csPost(t, srv, "s1", csBody("s1"))
	if st != http.StatusOK || gjson.Get(out, "choices.0.message.tool_calls.0.function.name").String() != "Workflow" {
		t.Fatalf("client got %d %s", st, out)
	}
	if strings.Contains(out, tooldefer.ClientSearchName) {
		t.Fatalf("our search call reached the client: %s", out)
	}
	b := up.all()
	if len(b) != 2 {
		t.Fatalf("upstream rounds = %d, want 2", len(b))
	}
	first, follow := b[0], b[1]
	if got := csTools(first); got != "Bash,Read,context_guru_expand,"+tooldefer.ClientSearchName {
		t.Fatalf("first tools = %s", got)
	}
	if !strings.HasPrefix(gjson.GetBytes(first, "messages.1.content").String(), "Context Guru deferred these tools") {
		t.Fatalf("names list missing: %s", gjson.GetBytes(first, "messages").Raw)
	}
	if got := csTools(follow); got != "Bash,Read,context_guru_expand,"+tooldefer.ClientSearchName+",Workflow" {
		t.Fatalf("follow-up tools = %s", got)
	}
	if !strings.Contains(gjson.GetBytes(follow, "messages.@reverse.0.content").String(), "Loaded: Workflow") {
		t.Fatalf("follow-up has no answer: %s", gjson.GetBytes(follow, "messages").Raw)
	}
	// The client's next request: Workflow stays declared, with the follow-up's tools bytes.
	next := csBody("s1", `{"role":"assistant","content":null,"tool_calls":[`+csCall("c2", "Workflow", `{"x":"go"}`)+`]}`,
		`{"role":"tool","tool_call_id":"c2","content":"ok"}`)
	csPost(t, srv, "s1", next)
	b = up.all()
	if gjson.GetBytes(b[2], "tools").Raw != gjson.GetBytes(follow, "tools").Raw {
		t.Fatalf("tools moved after the add-back:\n%s\n%s", csTools(b[2]), csTools(follow))
	}
	// No body our search sends carries defer_loading (GLM drops such a tool; #825): it removes.
	for i, sent := range up.all() {
		if bytes.Contains(sent, []byte("defer_loading")) {
			t.Fatalf("upstream body %d carries defer_loading", i)
		}
	}
	// The add-back is a wrong guess, counted by tooldefer.WrongGuesses through the ledger, once.
	if w := h.toolDefer().ledger.Snapshot().WrongGuesses; w != 1 {
		t.Fatalf("wrong_guesses = %d, want 1", w)
	}
	if g := h.deferGate(); g.AddBacks.Load() != 1 || g.MixedSearches.Load() != 0 {
		t.Fatalf("add_backs=%d mixed=%d", g.AddBacks.Load(), g.MixedSearches.Load())
	}
	// A load counts as a use: the history says this session used Workflow.
	if s := h.toolDefer().hist.UserSessions("", "", 1); len(s) == 0 || !s[0].Used["Workflow"] {
		t.Fatalf("the load is not a use in the history: %+v", s)
	}
}

// A search together with a client tool call goes to the client whole; the tool is added back at
// once, and the next request answers our call.
func TestClientSearchMixedCall(t *testing.T) {
	up := &csUpstream{script: []string{
		csCalls(csCall("c1", tooldefer.ClientSearchName, `{"names":["Monitor"]}`), csCall("c2", "Bash", `{"x":"ls"}`)),
		csText("ok"),
	}}
	us := httptest.NewServer(up)
	defer us.Close()
	h := csHandler(t, "ultrahigh", us.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	_, out := csPost(t, srv, "s1", csBody("s1"))
	if n := len(gjson.Get(out, "choices.0.message.tool_calls").Array()); n != 2 || len(up.all()) != 1 {
		t.Fatalf("client got %d calls after %d rounds: %s", n, len(up.all()), out)
	}
	next := csBody("s1", `{"role":"assistant","content":null,"tool_calls":[`+
		csCall("c1", tooldefer.ClientSearchName, `{"names":["Monitor"]}`)+`,`+csCall("c2", "Bash", `{"x":"ls"}`)+`]}`,
		`{"role":"tool","tool_call_id":"c1","content":"Error: no such tool"}`,
		`{"role":"tool","tool_call_id":"c2","content":"a.txt"}`)
	csPost(t, srv, "s1", next)
	sent := up.all()[1]
	if !strings.HasSuffix(csTools(sent), tooldefer.ClientSearchName+",Monitor") {
		t.Fatalf("tools = %s", csTools(sent))
	}
	if got := gjson.GetBytes(sent, "messages.4.content").String(); !strings.HasPrefix(got, "Loaded: Monitor") {
		t.Fatalf("our call not answered: %q", got)
	}
	// Counted at the response that searched, not again when the next request repairs it.
	if w := h.toolDefer().ledger.Snapshot().WrongGuesses; w != 1 {
		t.Fatalf("wrong_guesses = %d, want 1", w)
	}
	if g := h.deferGate(); g.AddBacks.Load() != 1 || g.MixedSearches.Load() != 1 {
		t.Fatalf("add_backs=%d mixed=%d", g.AddBacks.Load(), g.MixedSearches.Load())
	}
}

// Fail open: a 400 to the changed request sends the client's original, and turns our search off for
// the model.
func TestClientSearchRetriesOriginalOn400(t *testing.T) {
	up := &csUpstream{script: []string{csText("ok")}, reject400: true}
	us := httptest.NewServer(up)
	defer us.Close()
	h := csHandler(t, "ultrahigh", us.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	st, out := csPost(t, srv, "s1", csBody("s1"))
	if st != http.StatusOK {
		t.Fatalf("client saw %d: %s", st, out)
	}
	b := up.all()
	if len(b) != 2 || csTools(b[1]) != "Bash,Read,Workflow,Monitor,context_guru_expand" {
		t.Fatalf("retry was not the original: %d rounds, %s", len(b), csTools(b[len(b)-1]))
	}
	if !h.deferGate().Broken("", "gpt-5.5") {
		t.Fatal("not turned off after the 400")
	}
}

// Off outside ultrahigh, and off for Claude models on this route.
func TestClientSearchOffOutsideUltrahigh(t *testing.T) {
	up := &csUpstream{script: []string{csText("ok")}}
	us := httptest.NewServer(up)
	defer us.Close()
	h := csHandler(t, "xhigh", us.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	csPost(t, srv, "s1", csBody("s1"))
	if got := csTools(up.all()[0]); strings.Contains(got, tooldefer.ClientSearchName) || !strings.Contains(got, "Workflow") {
		t.Fatalf("xhigh changed tools: %s", got)
	}
	h2 := csHandler(t, "ultrahigh", us.URL)
	srv2 := httptest.NewServer(h2.Mux())
	defer srv2.Close()
	csPost(t, srv2, "s1", bytes.Replace(csBody("s1"), []byte(`"gpt-5.5"`), []byte(`"claude-sonnet-5-5"`), 1))
	if got := csTools(up.all()[1]); strings.Contains(got, tooldefer.ClientSearchName) {
		t.Fatalf("claude model got our search: %s", got)
	}
}

// A 400 on the rewritten request sends the original; when the original's answer still calls our
// search tool (the model saw it earlier in the thread), the client gets that answer as it is: no
// panic, no dropped connection, no in-band round.
func TestClientSearch400ThenSearchCallDoesNotPanic(t *testing.T) {
	up := &csUpstream{reject400: true, script: []string{
		csCalls(csCall("c1", tooldefer.ClientSearchName, `{"names":["Workflow"]}`)),
	}}
	us := httptest.NewServer(up)
	defer us.Close()
	h := csHandler(t, "ultrahigh", us.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	st, out := csPost(t, srv, "s1", csBody("s1"))
	if st != http.StatusOK || gjson.Get(out, "choices.0.message.tool_calls.0.function.name").String() != tooldefer.ClientSearchName {
		t.Fatalf("client got %d %s", st, out)
	}
	if n := len(up.all()); n != 2 {
		t.Fatalf("upstream rounds = %d, want 2 (the rewritten request, then the original)", n)
	}
}

// Responses, a client that chains on previous_response_id from turn 2: turn 2's search call is
// answered in band, never streamed to the client, and the follow-up keeps the chain.
func TestClientSearchChainedResponsesTurn(t *testing.T) {
	respDecl := func(name string) string {
		return fmt.Sprintf(`{"type":"function","name":%q,"description":"Does the %s job. %s","parameters":{"type":"object"}}`,
			name, name, strings.Repeat("Detailed usage notes for this tool. ", 60))
	}
	body := func(input string) []byte {
		return []byte(`{"model":"gpt-5.5","instructions":"agent","tools":[` + respDecl("Bash") + `,` + respDecl("Read") + `,` +
			respDecl("Workflow") + `,` + respDecl("Monitor") + `],` + input + `}`)
	}
	answer := func(items string) string {
		return `{"id":"resp_x","object":"response","status":"completed","output":[` + items +
			`],"usage":{"input_tokens":100,"output_tokens":5,"input_tokens_details":{"cached_tokens":0}}}`
	}
	msg := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}`
	up := &csUpstream{script: []string{
		answer(msg),
		answer(`{"type":"function_call","call_id":"c1","name":"` + tooldefer.ClientSearchName + `","arguments":"{\"names\":[\"Workflow\"]}"}`),
		answer(`{"type":"function_call","call_id":"c2","name":"Workflow","arguments":"{}"}`),
	}}
	us := httptest.NewServer(up)
	defer us.Close()
	h := csHandler(t, "ultrahigh", us.URL)
	ds := h.toolDefer()
	for i, s := range []string{"r1", "r2", "r3"} {
		ds.hist.Observe("", s, "", tooldefer.View("responses", body(`"input":[{"role":"user","content":"x"}]`)), int64(i+10))
	}
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	post := func(b []byte) string {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/openai/v1/responses", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("x-context-guru-session", "rs1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return string(out)
	}
	post(body(`"input":[{"role":"user","content":"hi"}]`))
	out := post(body(`"previous_response_id":"resp_1","input":[{"role":"user","content":"run the workflow"}]`))
	if strings.Contains(out, tooldefer.ClientSearchName) || !strings.Contains(out, `"Workflow"`) {
		t.Fatalf("client got: %s", out)
	}
	b := up.all()
	if len(b) != 3 {
		t.Fatalf("upstream rounds = %d, want 3", len(b))
	}
	names := func(x []byte) string {
		var o []string
		gjson.GetBytes(x, "tools").ForEach(func(_, t gjson.Result) bool { o = append(o, t.Get("name").String()); return true })
		return strings.Join(o, ",")
	}
	// The chained turn declares turn 1's tools: the search tool in, the deferred tools out.
	if gjson.GetBytes(b[1], "tools").Raw != gjson.GetBytes(b[0], "tools").Raw {
		t.Fatalf("chained turn tools = %s, turn 1 = %s", names(b[1]), names(b[0]))
	}
	if got := names(b[1]); !strings.Contains(got, tooldefer.ClientSearchName) || strings.Contains(got, "Workflow") {
		t.Fatalf("chained turn tools = %s", got)
	}
	follow := b[2]
	if gjson.GetBytes(follow, "previous_response_id").String() != "resp_1" || !strings.HasSuffix(names(follow), "Workflow") {
		t.Fatalf("follow-up: prev=%s tools=%s", gjson.GetBytes(follow, "previous_response_id"), names(follow))
	}
	if !strings.Contains(gjson.GetBytes(follow, "input").Raw, "Loaded: Workflow") {
		t.Fatalf("follow-up has no answer: %s", gjson.GetBytes(follow, "input").Raw)
	}
}

// A search call the client gets at the round cap is kept by call id, so a chained Responses turn
// that carries only the client's answer is repaired.
func TestClientSearchRoundCapKeepsTheCall(t *testing.T) {
	search := func(id string) string {
		return csCalls(csCall(id, "context_guru_expand", `{"id":"nope"}`), csCall(id+"s", tooldefer.ClientSearchName, `{"names":["Monitor"]}`))
	}
	// Every round calls expand and our search: the loop answers both until the cap, then bails.
	up := &csUpstream{script: []string{search("a")}}
	us := httptest.NewServer(up)
	defer us.Close()
	h := csHandler(t, "ultrahigh", us.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	_, out := csPost(t, srv, "s1", csBody("s1"))
	if !strings.Contains(out, tooldefer.ClientSearchName) {
		t.Fatalf("the cap did not hand the call over: %d rounds, %s", len(up.all()), out)
	}
	rec, ok := tooldefer.LoadSearch(h.deferEnv(h.static.Load()).RecordStore(), "s1", "")
	if !ok || len(rec.Calls["as"]) != 1 || rec.Calls["as"][0] != "Monitor" {
		t.Fatalf("the leaked call is not kept: %+v", rec)
	}
}

// #879: on a streamed Chat Completions answer, text reaches the client as it arrives and only a call
// to our search is held. The follow-up streams as the rest of the same answer.
func TestClientSearchStreamsAndSplicesInBand(t *testing.T) {
	r1 := ccRole("r1") + ccText("r1", "Let me look.") + ccCallStart("r1", 0, "c1", tooldefer.ClientSearchName) +
		ccCallArgs("r1", 0, `{"names":["Workflow"]}`) + ccFinish("r1", "tool_calls") + ccUsage("r1") + ccDone
	r2 := ccRole("r2") + ccCallStart("r2", 0, "c2", "Workflow") + ccCallArgs("r2", 0, `{"x":"go"}`) +
		ccFinish("r2", "tool_calls") + ccUsage("r2") + ccDone
	up := &csUpstream{script: []string{r1, r2}}
	us := httptest.NewServer(up)
	defer us.Close()
	h := csHandler(t, "ultrahigh", us.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	body, _ := sjson.SetBytes(csBody("s1"), "stream", true)
	st, out := csPost(t, srv, "s1", body)
	if st != http.StatusOK || strings.Contains(out, tooldefer.ClientSearchName) {
		t.Fatalf("client got %d %q", st, out)
	}
	if !strings.HasPrefix(out, ccRole("r1")+ccText("r1", "Let me look.")) {
		t.Fatalf("the text must reach the client verbatim, ahead of the splice: %q", out)
	}
	if strings.Count(out, ccDone) != 1 || strings.Contains(out, `"id":"r2"`) {
		t.Fatalf("the client must see one answer: %q", out)
	}
	msg, ok := expand.AggregateSSE("openai", []byte(out))
	if !ok || gjson.GetBytes(msg, "choices.0.message.tool_calls.#").Int() != 1 ||
		gjson.GetBytes(msg, "choices.0.message.tool_calls.0.function.name").String() != "Workflow" ||
		gjson.GetBytes(msg, "choices.0.message.tool_calls.0.index").Exists() {
		t.Fatalf("the client's answer: %s", msg)
	}
	if !strings.Contains(out, `"tool_calls":[{"index":0,"id":"c2"`) {
		t.Fatalf("the follow-up's call keeps index 0 (the client holds no call before it): %q", out)
	}
	b := up.all()
	if len(b) != 2 || !strings.HasSuffix(csTools(b[1]), tooldefer.ClientSearchName+",Workflow") {
		t.Fatalf("rounds = %d", len(b))
	}
	if !strings.Contains(gjson.GetBytes(b[1], "messages.@reverse.0.content").String(), "Loaded: Workflow") {
		t.Fatalf("follow-up has no answer: %s", gjson.GetBytes(b[1], "messages").Raw)
	}
	if g := h.deferGate(); g.AddBacks.Load() != 1 || g.MixedSearches.Load() != 0 {
		t.Fatalf("add_backs=%d mixed=%d", g.AddBacks.Load(), g.MixedSearches.Load())
	}
}

// A streamed answer with no call of ours goes to the client byte for byte, in one upstream round.
func TestClientSearchStreamTextPassesThrough(t *testing.T) {
	r1 := ccRole("r1") + ccReason("r1", "hmm") + ccText("r1", "Hello") + ccFinish("r1", "stop") + ccUsage("r1") + ccDone
	up := &csUpstream{script: []string{r1}}
	us := httptest.NewServer(up)
	defer us.Close()
	h := csHandler(t, "ultrahigh", us.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	body, _ := sjson.SetBytes(csBody("s1"), "stream", true)
	if _, out := csPost(t, srv, "s1", body); out != r1 {
		t.Fatalf("the client must get the stream byte for byte:\n got  %q\n want %q", out, r1)
	}
	if !tooldefer.HasClientSearch("openai", up.all()[0]) {
		t.Fatal("the test needs a request that declares our search")
	}
}

// A streamed mixed answer: the client's call streams, ours is handed back after it, and the next
// request answers it, as on the buffered path.
func TestClientSearchStreamMixedCall(t *testing.T) {
	r1 := ccRole("r1") + ccCallStart("r1", 0, "c1", "Bash") + ccCallArgs("r1", 0, `{"x":"ls"}`) +
		ccCallStart("r1", 1, "c2", tooldefer.ClientSearchName) + ccCallArgs("r1", 1, `{"names":["Monitor"]}`) +
		ccFinish("r1", "tool_calls") + ccDone
	up := &csUpstream{script: []string{r1}}
	us := httptest.NewServer(up)
	defer us.Close()
	h := csHandler(t, "ultrahigh", us.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	body, _ := sjson.SetBytes(csBody("s1"), "stream", true)
	if _, out := csPost(t, srv, "s1", body); out != r1 || len(up.all()) != 1 {
		t.Fatalf("the client must get the mixed answer whole, in one round:\n got  %q", out)
	}
	if g := h.deferGate(); g.AddBacks.Load() != 1 || g.MixedSearches.Load() != 1 {
		t.Fatalf("add_backs=%d mixed=%d", g.AddBacks.Load(), g.MixedSearches.Load())
	}
}

// Codex declares its own tool_search (#876): the request goes through unchanged, ours is not added,
// and the tools its search loaded (echoed in `input`) count as a use in the history.
func TestClientSearchNativeSearchLeftAlone(t *testing.T) {
	up := &csUpstream{script: []string{csText("ok")}}
	us := httptest.NewServer(up)
	defer us.Close()
	h := csHandler(t, "ultrahigh", us.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	decl := func(n string) string {
		return fmt.Sprintf(`{"type":"function","name":%q,"description":"%s","parameters":{"type":"object"}}`,
			n, strings.Repeat("Detailed usage notes for this tool. ", 60))
	}
	body := []byte(`{"model":"gpt-5.5","instructions":"agent","tools":[` + decl("Bash") + `,` + decl("Workflow") +
		`,{"type":"tool_search","execution":"client"}],"input":[{"role":"user","content":"hi"},` +
		`{"type":"tool_search_call","call_id":"c1","execution":"client","arguments":{"query":"w"}},` +
		`{"type":"tool_search_output","call_id":"c1","execution":"client","tools":[{"type":"function","name":"Loaded1"}]}]}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/openai/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("x-context-guru-session", "n1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	sent := up.all()
	if len(sent) != 1 || bytes.Contains(sent[0], []byte(tooldefer.ClientSearchName)) ||
		bytes.Contains(sent[0], []byte("defer_loading")) || !bytes.Contains(sent[0], []byte(`"Workflow"`)) {
		t.Fatalf("native search request was changed: %s", sent)
	}
	if s := h.toolDefer().hist.UserSessions("", "", 1); len(s) == 0 || !s[0].Used["Loaded1"] {
		t.Fatalf("the native load is not a use: %+v", s)
	}
}
