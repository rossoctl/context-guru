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
	if PromptOfClient(agent.Client{}, []string{"request_user_input"}) != PromptQuestion {
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
		obs := New().Observe("t", "s", "", bschemas.OpenAI, tc.body, ForClient(who("codex_cli_rs/0.162")))
		if obs.OpenBackground != tc.want {
			t.Errorf("%s: OpenBackground = %d, want %d", tc.name, obs.OpenBackground, tc.want)
		}
		if scan := ScanBody(nil, bschemas.OpenAI, tc.body, ForClient(who("codex_cli_rs/0.162"))); scan == nil || scan.OpenBackground != tc.want {
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

// A Claude Code Bash result that echoes the Codex terminal text opens no task. The same text in a Codex
// body does. A body with no client (a host that passes none) behaves as Claude Code.
func TestCodexTerminalMarkersReadOnlyForCodex(t *testing.T) {
	running := `Chunk ID: a1\nWall time: 10.0 seconds\nProcess running with session ID 1001\nOutput:\nbuilding`
	body := responsesBody(strings.Join(codexExec("c1", running), ","))
	claude := agent.IdentifyUserAgent("claude-cli/2.1.294 (external, cli)")
	codex := agent.IdentifyUserAgent("codex_cli_rs/0.162.1")
	for _, tc := range []struct {
		name string
		opts []Option
		want int
	}{
		{"claude code", []Option{ForClient(claude)}, 0},
		{"no client", nil, 0},
		{"codex", []Option{ForClient(codex)}, 1},
	} {
		if obs := New().Observe("t", "s", "", bschemas.OpenAI, body, tc.opts...); obs.OpenBackground != tc.want {
			t.Errorf("%s: Observe OpenBackground = %d, want %d", tc.name, obs.OpenBackground, tc.want)
		}
		if s := ScanBody(nil, bschemas.OpenAI, body, tc.opts...); s == nil || s.OpenBackground != tc.want {
			t.Errorf("%s: ScanBody disagrees", tc.name)
		}
	}
}

// A question tool is read by the client that sends it. Claude Code's AskUserQuestion is a person
// for a Claude Code request and a plain tool for a Codex one. A request with no named client takes
// both lists (the fallback).
func TestAskHumanToolsFollowTheClient(t *testing.T) {
	for _, tc := range []struct {
		name, ua, tool, want string
	}{
		{"claude question", "claude-cli/2.1", "AskUserQuestion", WaitingHuman},
		{"claude does not read the codex tool", "claude-cli/2.1", "request_user_input", WaitingOwnTool},
		{"codex does not read the claude tool", "codex_cli_rs/0.162", "AskUserQuestion", WaitingOwnTool},
		{"codex question", "codex_cli_rs/0.162", "request_user_input", WaitingHuman},
		{"no client: claude tool", "", "AskUserQuestion", WaitingHuman},
		{"no client: codex tool", "", "request_user_input", WaitingHuman},
	} {
		got, _ := ClassifyWaitingForClient(agent.IdentifyUserAgent(tc.ua), nil, []string{tc.tool}, "tool_use", false, true, 0, 0)
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := PromptOfClient(agent.IdentifyUserAgent("claude-cli/2.1"), []string{"request_user_input"}); got != PromptNone {
		t.Errorf("claude request: request_user_input prompt = %q, want none", got)
	}
}

// Text a client injects reads as system, not human, whichever of the client's markers it carries.
// Before the lists were joined, only four substrings did.
func TestInjectedTextReadsAsSystemWake(t *testing.T) {
	for _, text := range []string{
		"[Request interrupted by user]",
		"[Request interrupted by user for tool use]",
		"This session is being continued from a previous conversation that ran out of context.",
		"[SYSTEM NOTIFICATION - NOT USER INPUT] a background task finished",
		"Continue from where you left off.",
		"<local-command-caveat>Caveat: the messages below were generated by the user</local-command-caveat>",
		// The four substrings that were read before still are.
		"<system-reminder>\nContext low.\n</system-reminder>",
		"<command-name>/compact</command-name>",
		"<local-command-stdout>ok</local-command-stdout>",
		"Base directory for this skill: /x/y",
	} {
		obs := New().Observe("t", "s", "", bschemas.Anthropic, anthropicBody(userText(text)))
		if obs.WakeCause != WakeSystem {
			t.Errorf("%q: wake cause = %q, want %s", text, obs.WakeCause, WakeSystem)
		}
	}
	// Another agent's message is a teammate wake, not a system one.
	for _, text := range []string{
		"The coordinator sent a message while you were working: hi",
		"Another Claude session sent a message while you were working: hello",
		"A teammate sent a message while you were working: hey",
	} {
		obs := New().Observe("t", "s", "", bschemas.Anthropic, anthropicBody(userText(text)))
		if obs.WakeCause != WakeTeammate {
			t.Errorf("%q: wake cause = %q, want %s", text, obs.WakeCause, WakeTeammate)
		}
	}
	// The wake check keeps case: a person's prompt that starts like a marker stays human.
	for _, text := range []string{"api error again, please look", "continue from where you left off.", "[request interrupted by user]"} {
		obs := New().Observe("t", "s", "", bschemas.Anthropic, anthropicBody(userText(text)))
		if obs.WakeCause != WakeHuman {
			t.Errorf("%q: wake cause = %q, want %s", text, obs.WakeCause, WakeHuman)
		}
	}
	// A plain prompt stays human, whatever the client.
	for _, ua := range []string{"", "claude-cli/2.1", "codex_cli_rs/0.162"} {
		obs := New().Observe("t", "s", "", bschemas.Anthropic, anthropicBody(userText("please fix the bug")), ForClient(agent.IdentifyUserAgent(ua)))
		if obs.WakeCause != WakeHuman {
			t.Errorf("client %q: a typed prompt reads %q, want %s", ua, obs.WakeCause, WakeHuman)
		}
	}
}
