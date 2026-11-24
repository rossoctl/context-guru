package waitcause

import (
	"github.com/rossoctl/context-guru/agent"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
)

func who(ua string) agent.Client { return agent.IdentifyUserAgent(ua) }

func TestHumanFallback(t *testing.T) {
	for ua, want := range map[string]bool{
		"":                              true, // no client named: as before
		"claude-cli/2.1.294 (external)": true,
		"BobShell/1.2":                  true, // Bob is on hold: it keeps its behavior
		"codex_cli_rs/0.162.1":          false,
		"opencode/2.4.1":                false,
		"python-httpx/0.27":             false,
	} {
		if got := HumanFallback(who(ua)); got != want {
			t.Errorf("HumanFallback(%q) = %v, want %v", ua, got, want)
		}
	}
	if !HumanFallback(agent.Client{}) {
		t.Errorf("a zero Client (a host that passes none) must keep human")
	}
}

// An end_turn with nothing open reads "human" for Claude Code and "unknown" for another client. Every
// other cause is the same for both.
func TestClassifyWaitingForClientFallback(t *testing.T) {
	for _, tc := range []struct {
		name, ua string
		names    []string
		bg       int
		want     string
	}{
		{"claude end_turn", "claude-cli/2.1", nil, 0, WaitingHuman},
		{"codex end_turn", "codex_cli_rs/0.162", nil, 0, WaitingUnknown},
		{"opencode end_turn", "opencode/2.4.1", nil, 0, WaitingUnknown},
		{"bob end_turn", "BobShell/1", nil, 0, WaitingHuman},
		{"codex background open", "codex_cli_rs/0.162", nil, 1, WaitingBackground},
		{"codex own tool", "codex_cli_rs/0.162", []string{"exec_command"}, 0, WaitingOwnTool},
		{"codex question tool is a person", "codex_cli_rs/0.162", []string{"request_user_input"}, 0, WaitingHuman},
	} {
		got, _ := ClassifyWaitingForClient(who(tc.ua), nil, tc.names, "end_turn", false, true, tc.bg, 0)
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
	if PromptOf([]string{"request_user_input"}) != PromptQuestion {
		t.Errorf("request_user_input must read as a question prompt")
	}
}

func codexExec(call, out string) []string {
	return []string{
		`{"type":"function_call","call_id":"` + call + `","name":"exec_command","arguments":"{\"cmd\":\"make\"}"}`,
		`{"type":"function_call_output","call_id":"` + call + `","output":"` + out + `"}`,
	}
}

func codexPoll(call, session, out string) []string {
	return []string{
		`{"type":"function_call","call_id":"` + call + `","name":"write_stdin","arguments":"{\"session_id\":` + session + `}"}`,
		`{"type":"function_call_output","call_id":"` + call + `","output":"` + out + `"}`,
	}
}

func TestCodexBackgroundTerminal(t *testing.T) {
	running := `Chunk ID: a1\nWall time: 10.0 seconds\nProcess running with session ID 1001\nOutput:\nbuilding`
	exited := `Chunk ID: b2\nWall time: 3.0 seconds\nProcess exited with code 0\nOutput:\ndone`
	quick := `Chunk ID: c3\nWall time: 0.1 seconds\nProcess exited with code 0\nOutput:\nok`
	join := func(parts ...[]string) []byte {
		var items []string
		for _, p := range parts {
			items = append(items, p...)
		}
		return responsesBody(strings.Join(items, ","))
	}
	for _, tc := range []struct {
		name string
		body []byte
		want int
	}{
		{"a process left running is open", join(codexExec("c1", running)), 1},
		{"a quick command is not open", join(codexExec("c1", quick)), 0},
		{"a poll that sees the process still running keeps it open", join(codexExec("c1", running), codexPoll("c2", "1001", running)), 1},
		{"a poll that sees the exit closes it", join(codexExec("c1", running), codexPoll("c2", "1001", exited)), 0},
		{"an exit of another session does not close it", join(codexExec("c1", running), codexPoll("c2", "2002", exited)), 1},
	} {
		obs := New().Observe("t", "s", "", bschemas.OpenAI, tc.body)
		if obs.OpenBackground != tc.want {
			t.Errorf("%s: OpenBackground = %d, want %d", tc.name, obs.OpenBackground, tc.want)
		}
		if scan := ScanBody(nil, bschemas.OpenAI, tc.body); scan == nil || scan.OpenBackground != tc.want {
			t.Errorf("%s: ScanBody disagrees with Observe", tc.name)
		}
	}
}

// Claude Code must read exactly as before this change: for every input, the client form with a Claude
// Code client gives the same answer as the form that has no client (the behavior on main).
func TestClaudeCodeWaitCauseIsUnchanged(t *testing.T) {
	cc := who("claude-cli/2.1.294 (external, cli)")
	names := [][]string{nil, {"Bash"}, {"AskUserQuestion"}, {"ExitPlanMode"}, {"Read", "Task"}, {"SendMessage"}, {"StructuredOutput"}}
	for _, stop := range []string{"end_turn", "tool_use", "max_tokens", ""} {
		for _, n := range names {
			for _, failed := range []bool{false, true} {
				for _, usageOK := range []bool{false, true} {
					for bg := 0; bg < 2; bg++ {
						for tm := 0; tm < 2; tm++ {
							wantW, wantT := ClassifyWaitingForTerminal([]string{"StructuredOutput"}, n, stop, failed, usageOK, bg, tm)
							gotW, gotT := ClassifyWaitingForClient(cc, []string{"StructuredOutput"}, n, stop, failed, usageOK, bg, tm)
							if gotW != wantW || gotT != wantT {
								t.Errorf("stop=%q names=%v failed=%v usageOK=%v bg=%d tm=%d: got %q/%q, main gives %q/%q", stop, n, failed, usageOK, bg, tm, gotW, gotT, wantW, wantT)
							}
						}
					}
				}
			}
		}
	}
	if w, _ := ClassifyWaitingForClient(cc, nil, nil, "end_turn", false, true, 0, 0); w != WaitingHuman {
		t.Errorf("a Claude Code end_turn with nothing open must read human, got %q", w)
	}
}
