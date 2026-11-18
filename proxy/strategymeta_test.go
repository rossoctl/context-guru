package proxy

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/testutil"
	"github.com/rossoctl/context-guru/keepalive"
)

// The wait tool's `timeout` and `run_in_background` are read, and nothing else of the input.
func TestToolParamsOf(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  ToolParams
	}{
		{"both", `{"command":"make","timeout":600000,"run_in_background":true}`, ToolParams{600000, true, true, true}},
		{"timeout only", `{"command":"ls","timeout":120000}`, ToolParams{TimeoutMs: 120000, HasTimeout: true}},
		{"background false is stated", `{"run_in_background":false}`, ToolParams{HasBackground: true}},
		{"neither", `{"file_path":"/x"}`, ToolParams{}},
		{"timeout as a string is not a number", `{"timeout":"600000"}`, ToolParams{}},
		{"zero or negative timeout is not stated", `{"timeout":0}`, ToolParams{}},
		{"background as a string", `{"run_in_background":"true"}`, ToolParams{}},
		{"not JSON", `{"timeout":`, ToolParams{}},
		{"empty", ``, ToolParams{}},
	} {
		if got := toolParamsOf(tc.input); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// A buffered response, of each dialect, gives the parameters of its last call.
func TestResponseLastToolParamsBuffered(t *testing.T) {
	anth := []byte(`{"content":[
		{"type":"tool_use","id":"a","name":"Bash","input":{"command":"x","timeout":1000}},
		{"type":"tool_use","id":"b","name":"Bash","input":{"command":"y","timeout":600000,"run_in_background":true}}
	],"stop_reason":"tool_use"}`)
	if got := responseLastToolParams(false, anth); got != (ToolParams{600000, true, true, true}) {
		t.Errorf("anthropic: %+v", got)
	}
	oa := []byte(`{"choices":[{"message":{"tool_calls":[{"id":"c","type":"function","function":{"name":"shell","arguments":"{\"timeout\":30000}"}}]}}]}`)
	if got := responseLastToolParams(false, oa); got != (ToolParams{TimeoutMs: 30000, HasTimeout: true}) {
		t.Errorf("openai chat: %+v", got)
	}
	none := []byte(`{"content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}`)
	if got := responseLastToolParams(false, none); got != (ToolParams{}) {
		t.Errorf("no call: %+v", got)
	}
}

// A streamed Anthropic call carries its input in pieces. They are joined for the LAST tool_use block.
func TestResponseLastToolParamsStreamed(t *testing.T) {
	ev := func(s string) string { return "data: " + s + "\n\n" }
	stream := ev(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"a","name":"Read","input":{}}}`) +
		ev(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"timeout\":5}"}}`) +
		ev(`{"type":"content_block_stop","index":0}`) +
		ev(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"b","name":"Bash","input":{}}}`) +
		ev(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"make all\", \"time"}}`) +
		ev(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"out\":900000,\"run_in_back"}}`) +
		ev(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"ground\":true}"}}`) +
		ev(`{"type":"content_block_stop","index":1}`) +
		ev(`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`)
	if got := responseLastToolParams(true, []byte(stream)); got != (ToolParams{900000, true, true, true}) {
		t.Errorf("streamed: %+v", got)
	}
	// A call with no input at all: unstated, not zero.
	empty := ev(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"a","name":"TaskList","input":{}}}`)
	if got := responseLastToolParams(true, []byte(empty)); got != (ToolParams{}) {
		t.Errorf("empty input: %+v", got)
	}
	// An OpenAI stream is not read: a gap, never a guess.
	oa := ev(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"shell","arguments":"{\"timeout\":5}"}}]}}]}`)
	if got := responseLastToolParams(true, []byte(oa)); got != (ToolParams{}) {
		t.Errorf("openai stream: %+v", got)
	}
}

// The result type holds one number and one flag per parameter, so no input text can ride in it.
func TestToolParamsKeepNothingElse(t *testing.T) {
	got := toolParamsOf(`{"command":"curl -H 'Authorization: sk-secret'","timeout":1000}`)
	if got != (ToolParams{TimeoutMs: 1000, HasTimeout: true}) {
		t.Fatalf("got %+v", got)
	}
}

// A whole response reaches Usage with the parameters, unless the window was spliced.
func TestResponseUsageCarriesToolParams(t *testing.T) {
	body := []byte(`{"id":"m","usage":{"input_tokens":5,"output_tokens":3},"content":[
		{"type":"tool_use","id":"a","name":"Bash","input":{"command":"x","timeout":240000}}],"stop_reason":"tool_use"}`)
	u, _, _ := responseUsageWhy("application/json", body, false)
	if !u.ToolParams.HasTimeout || u.ToolParams.TimeoutMs != 240000 {
		t.Fatalf("usage tool params = %+v", u.ToolParams)
	}
	u, _, _ = responseUsageWhy("application/json", body, true)
	if u.ToolParams != (ToolParams{}) {
		t.Fatalf("a spliced window must leave the parameters unstated, got %+v", u.ToolParams)
	}
}

// sseToolReply is a streamed Anthropic reply that ends on one tool call whose input arrives in pieces.
func sseToolReply(name string, inputPieces ...string) []byte {
	ev := func(s string) string { return "data: " + s + "\n\n" }
	out := ev(`{"type":"message_start","message":{"usage":{"input_tokens":5,"cache_read_input_tokens":3000,"cache_creation_input_tokens":100}}}`) +
		ev(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"`+name+`","input":{}}}`)
	for _, p := range inputPieces {
		b, _ := json.Marshal(p)
		out += ev(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":` + string(b) + `}}`)
	}
	return []byte(out + ev(`{"type":"content_block_stop","index":0}`) +
		ev(`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`))
}

const metaUserID = `{\"device_id\":\"dev-feedbeef\",\"account_uuid\":\"acct-0000-1111\",\"session_id\":\"sess-meta-1\"}`

func metaBody() string {
	return `{"model":"claude-sonnet-4-5","max_tokens":10,"stream":true,"metadata":{"user_id":"` + metaUserID + `"},` +
		`"messages":[{"role":"user","content":"build it"}]}`
}

// One request carries the strategy metadata end to end: the tool's parameters, the prompt, the
// time zone, the turn, the concurrency, and a person's hash that is not the raw id.
func TestRequestRowCarriesTheStrategyMetadata(t *testing.T) {
	srv, rec := bobUpstreamHandler(t, replyWith(true, sseToolReply("Bash",
		`{"command":"make all","timeo`, `ut":900000,"run_in_background":true}`)))
	row := rowAt(t, srv, rec, "/anthropic/v1/messages", metaBody(), map[string]string{"anthropic-version": "2023-06-01", "User-Agent": "claude-cli/2.1.0 (external, cli)"})
	if row.WaitingFor != "own_tool" || row.WaitTool != "Bash" {
		t.Fatalf("wait = %q %q, want own_tool Bash", row.WaitingFor, row.WaitTool)
	}
	if row.WaitToolTimeoutMs == nil || *row.WaitToolTimeoutMs != 900000 || row.WaitToolBackground == nil || !*row.WaitToolBackground {
		t.Fatalf("tool parameters = %v %v, want 900000 true", row.WaitToolTimeoutMs, row.WaitToolBackground)
	}
	if row.WaitPrompt != "none" {
		t.Fatalf("wait_prompt = %q, want none", row.WaitPrompt)
	}
	if row.ClientEntrypoint != "cli" {
		t.Fatalf("client_entrypoint = %q, want cli", row.ClientEntrypoint)
	}
	if row.TZOffsetMin == nil {
		t.Fatal("a local proxy must record its UTC offset")
	}
	if _, want := time.Now().Zone(); *row.TZOffsetMin != want/60 {
		t.Fatalf("tz_offset_min = %d, want %d", *row.TZOffsetMin, want/60)
	}
	if row.Turn != 1 || row.ActiveThreads != 1 {
		t.Fatalf("turn %d active %d, want 1 1", row.Turn, row.ActiveThreads)
	}
	var h string
	if err := rec.DB().SQL().QueryRow(`SELECT user_hash FROM requests WHERE id = ?`, row.ID).Scan(&h); err != nil || len(h) != 16 {
		t.Fatalf("user_hash = %q (%v), want 16 hex chars", h, err)
	}
	if strings.Contains(h, "acct") || strings.Contains(h, "feedbeef") {
		t.Fatalf("user_hash shows the id: %q", h)
	}
	// The second request on the thread is turn 2.
	row2 := rowAt(t, srv, rec, "/anthropic/v1/messages", metaBody(), nil)
	if row2.Turn != 2 {
		t.Fatalf("second request turn = %d, want 2", row2.Turn)
	}
}

// A question tool is a prompt a person must answer.
func TestRequestRowMarksAQuestion(t *testing.T) {
	srv, rec := bobUpstreamHandler(t, replyWith(true, sseToolReply("AskUserQuestion", `{"questions":[]}`)))
	row := rowAt(t, srv, rec, "/anthropic/v1/messages", metaBody(), nil)
	if row.WaitingFor != "human" || row.WaitPrompt != "question" {
		t.Fatalf("wait = %q prompt %q, want human question", row.WaitingFor, row.WaitPrompt)
	}
	if row.WaitToolTimeoutMs != nil {
		t.Fatalf("a human wait has no wait tool, got timeout %v", *row.WaitToolTimeoutMs)
	}
}

// The signal route keeps the event in the dashboard, and a permission prompt marks the request.
func TestClientSignalIsStoredAndMarksThePermissionPrompt(t *testing.T) {
	t.Setenv("CONTEXT_GURU_CLIENT_SIGNALS", "on")
	srv, rec := bobUpstreamHandler(t, replyWith(true, sseToolReply("Bash", `{"command":"rm x"}`)))
	row := rowAt(t, srv, rec, "/anthropic/v1/messages", metaBody(), map[string]string{"x-context-guru-session": "sess-meta-1"})
	post := func(body string) {
		resp, err := srv.Client().Post(srv.URL+"/signal", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("/signal returned %d", resp.StatusCode)
		}
	}
	post(`{"session_id":"sess-meta-1","hook_event_name":"Notification","notification_type":"permission_prompt","cwd":"/secret/path"}`)
	post(`{"session_id":"sess-meta-1","hook_event_name":"SessionEnd","reason":"logout"}`)
	var n int
	testutil.WaitForState(t, "two signals stored", func() bool {
		return rec.DB().SQL().QueryRow(`SELECT COUNT(*) FROM client_signals WHERE session_id = 'sess-meta-1'`).Scan(&n) == nil && n == 2
	}, func() string { return "signals: " + strconv.Itoa(n) })
	var event, reason string
	if err := rec.DB().SQL().QueryRow(`SELECT event, reason FROM client_signals WHERE event = 'SessionEnd'`).Scan(&event, &reason); err != nil || reason != "logout" {
		t.Fatalf("SessionEnd row = %q %q (%v)", event, reason, err)
	}
	var prompt string
	testutil.WaitForState(t, "the row marked", func() bool {
		return rec.DB().SQL().QueryRow(`SELECT wait_prompt FROM requests WHERE id = ?`, row.ID).Scan(&prompt) == nil && prompt == "permission"
	}, func() string { return "wait_prompt: " + prompt })
	// Nothing from the hook payload but the closed fields reached the table.
	var leak int
	_ = rec.DB().SQL().QueryRow(`SELECT COUNT(*) FROM client_signals WHERE reason LIKE '%secret%' OR session_id LIKE '%secret%'`).Scan(&leak)
	if leak != 0 {
		t.Fatal("the hook's cwd reached the signal table")
	}
}

// A hosted proxy leaves the time zone NULL; a local one records its own. The ping plan is copied as built.
func TestCaptureFillsTheZoneOnlyOnALocalProxy(t *testing.T) {
	start := time.Date(2026, 7, 1, 12, 0, 0, 0, time.FixedZone("x", 3*3600+1800))
	local := &capture{start: start, kaPlan: &keepalive.PlanInForce{Built: true, MaxPings: 8, MinPrefixTokens: 20000, IdleSeconds: 240}}
	e := &dash.Event{}
	local.fillStrategyMeta(e)
	if e.TZOffsetMin == nil || *e.TZOffsetMin != 210 {
		t.Fatalf("local tz = %v, want 210", e.TZOffsetMin)
	}
	if e.KAPlannedPings == nil || *e.KAPlannedPings != 8 || *e.KAMinPrefixTokens != 20000 || *e.KAIdleS != 240 {
		t.Fatalf("plan not copied: %v %v %v", e.KAPlannedPings, e.KAMinPrefixTokens, e.KAIdleS)
	}
	hosted := &capture{start: start, hosted: true}
	h := &dash.Event{}
	hosted.fillStrategyMeta(h)
	if h.TZOffsetMin != nil || h.KAPlannedPings != nil {
		t.Fatalf("hosted row = tz %v plan %v, want both NULL", h.TZOffsetMin, h.KAPlannedPings)
	}
}

func TestClientEntrypoint(t *testing.T) {
	for ua, want := range map[string]string{
		"claude-cli/2.1.0 (external, cli)":     "cli",
		"claude-cli/2.1.0 (external, sdk-cli)": "headless",
		"claude-cli/2.1.0 (external, sdk-ts)":  "sdk",
		"claude-cli/2.1.0 (external, vscode)":  "other",
		"codex_cli_rs/0.5 (Mac)":               "cli",
		"curl/8":                               "other",
		"":                                     "",
	} {
		if got := clientEntrypoint(ua); got != want {
			t.Errorf("%q = %q, want %q", ua, got, want)
		}
	}
}

func TestPermissionModeOf(t *testing.T) {
	for in, want := range map[string]string{"": "", "plan": "plan", "bypassPermissions": "bypassPermissions", "dontAsk": "dontAsk", "newmode": "other"} {
		if got := permissionModeOf(in); got != want {
			t.Errorf("%q = %q, want %q", in, got, want)
		}
	}
}

func TestResponseSpawns(t *testing.T) {
	buf := []byte(`{"content":[
		{"type":"tool_use","id":"a","name":"Agent","input":{"subagent_type":"Explore","prompt":"secret text"}},
		{"type":"tool_use","id":"b","name":"Agent","input":{"subagent_type":"my-reviewer","prompt":"x"}},
		{"type":"tool_use","id":"c","name":"Bash","input":{"command":"ls"}}]}`)
	if got := responseSpawns(false, buf); got != (Spawn{N: 2, Type: "my-reviewer"}) {
		t.Errorf("buffered: %+v", got)
	}
	ev := func(s string) string { return "data: " + s + "\n\n" }
	stream := ev(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"a","name":"Task","input":{}}}`) +
		ev(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"subagent_type\":\"Pla"}}`) +
		ev(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"n\",\"prompt\":\"p\"}"}}`) +
		ev(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"b","name":"Bash","input":{}}}`)
	if got := responseSpawns(true, []byte(stream)); got != (Spawn{N: 1, Type: "Plan"}) {
		t.Errorf("streamed: %+v", got)
	}
}
