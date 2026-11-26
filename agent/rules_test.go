package agent

import (
	"testing"

	"github.com/rossoctl/context-guru/agent/claudecode"
)

func TestBuiltinToolsIncludeToolSearch(t *testing.T) {
	c := Client{}
	for _, name := range append([]string{"ToolSearch"}, claudecode.BuiltinTools...) {
		if !c.IsBuiltinTool(name) {
			t.Errorf("IsBuiltinTool(%q) = false", name)
		}
	}
	// A name that only looks built-in stays off the list (see claudecode.BuiltinTools).
	for _, name := range []string{"Workflow", "ScheduleWakeup", "mcp__x__Read", ""} {
		if c.IsBuiltinTool(name) {
			t.Errorf("IsBuiltinTool(%q) = true", name)
		}
	}
}

func TestSubagentRules(t *testing.T) {
	c := Client{}
	if !c.IsSubagentTool("Agent") || !c.IsSubagentTool("Task") || c.IsSubagentTool("Bash") {
		t.Error("subagent tools are Agent and Task")
	}
	if c.SubagentTypeField() != "subagent_type" {
		t.Errorf("SubagentTypeField = %q", c.SubagentTypeField())
	}
	if !c.IsBuiltinSubagentType("Explore") || c.IsBuiltinSubagentType("my-reviewer") {
		t.Error("builtin subagent types: Explore yes, a user's own no")
	}
}

// The ask-human tools are per client. A request with no named client takes both lists, as the
// code did before the lists moved.
func TestAsksHumanIsPerClient(t *testing.T) {
	claude := IdentifyUserAgent("claude-cli/2.1.319 (external, cli)")
	codex := IdentifyUserAgent("codex_cli_rs/0.160.0")
	for _, tc := range []struct {
		name string
		c    Client
		tool string
		want bool
	}{
		{"claude asks", claude, "AskUserQuestion", true},
		{"claude plan", claude, "ExitPlanMode", true},
		{"claude does not read the codex name", claude, "request_user_input", false},
		{"codex asks", codex, "request_user_input", true},
		{"codex does not read the claude name", codex, "AskUserQuestion", false},
		{"no client: claude name", Client{}, "AskUserQuestion", true},
		{"no client: codex name", Client{}, "request_user_input", true},
		{"unknown client: both", IdentifyUserAgent("curl/8"), "request_user_input", true},
		{"no ask tool", claude, "Bash", false},
	} {
		if got := tc.c.AsksHuman(tc.tool); got != tc.want {
			t.Errorf("%s: AsksHuman(%q) = %v, want %v", tc.name, tc.tool, got, tc.want)
		}
	}
	if !(Client{}).IsQuestionTool("request_user_input") || (Client{}).IsQuestionTool("ExitPlanMode") {
		t.Error("question tools: request_user_input yes, ExitPlanMode no")
	}
}

// Every client without its own rule gets Claude Code's injected text, so a request the code read
// as injected before the lists moved reads the same.
func TestInjectedTextFallsBackToClaudeCodes(t *testing.T) {
	for _, c := range []Client{{}, IdentifyUserAgent("claude-cli/2.1.319"), IdentifyUserAgent("codex_cli_rs/0.160.0"), IdentifyUserAgent("curl/8")} {
		inj := c.Injected()
		for _, in := range []string{
			"[Request interrupted by user]",
			"This session is being continued from a previous conversation that ran out of context.",
			"[SYSTEM NOTIFICATION - NOT USER INPUT] a task finished",
			"Continue from where you left off.",
			"The coordinator sent a message while you were working: hi",
			"Skill /fetch is already loaded above; instructions unchanged.",
		} {
			if !inj.HasPrefix(in) {
				t.Errorf("%q (client %q): HasPrefix = false", in, c.Label)
			}
		}
		for _, in := range []string{"<system-reminder>x", "a <local-command-caveat>y", "<command-name>/x"} {
			if !inj.HasTag(in) {
				t.Errorf("%q (client %q): HasTag = false", in, c.Label)
			}
		}
		for _, in := range []string{"please fix the bug", "Skill /a b is already loaded above; instructions unchanged"} {
			if inj.HasPrefix(in) {
				t.Errorf("%q read as injected", in)
			}
			if inj.HasTag(in) {
				t.Errorf("%q has a tag", in)
			}
		}
	}
}

func TestUserIDIsClaudeCodesBodyField(t *testing.T) {
	body := []byte(`{"metadata":{"user_id":"{\"session_id\":\"s1\"}"}}`)
	if got := (Client{}).UserID(body); got != `{"session_id":"s1"}` {
		t.Errorf("UserID = %q", got)
	}
	if got := (Client{}).UserID([]byte(`{"metadata":{"taskId":"x"}}`)); got != "" {
		t.Errorf("a body with no user_id gave %q", got)
	}
}
