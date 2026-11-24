package agent

import (
	"net/http"
	"testing"
)

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

func TestNoHeadersIsTheGenericClient(t *testing.T) {
	want := Client{Label: "unknown", SDK: "unknown"}
	if got := Identify(nil); got != want {
		t.Errorf("Identify(nil) = %+v, want %+v", got, want)
	}
	if got := Identify(http.Header{}); got != want {
		t.Errorf("Identify(empty) = %+v, want %+v", got, want)
	}
	if got := IdentifyUserAgent(""); got != want {
		t.Errorf("IdentifyUserAgent(\"\") = %+v, want %+v", got, want)
	}
}

// These are the dashboard labels dash.AgentFor gave before this refactor.
func TestLabelsAreUnchanged(t *testing.T) {
	for ua, want := range map[string]string{
		"claude-cli/2.1.0 (external, cli)": "claude-cli", "Claude-Code/1": "claude-code", "codex_cli_rs/0.5": "codex",
		"Cursor/1": "cursor", "cline/3": "cline", "aider/1": "aider", "gemini-cli/1": "gemini-cli",
		"bobshell/1.0": "bob", "opencode/1.2.3": "opencode", "ai-sdk/x opencode/2": "opencode",
		"Anthropic/JS 0.60": "anthropic", "": "unknown", "singletoken": "singletoken",
		"averyveryveryveryverylongsingletokenwithoutseparators": "averyveryveryveryverylongsinglet",
	} {
		if got := IdentifyUserAgent(ua).Label; got != want {
			t.Errorf("Label(%q) = %q, want %q", ua, got, want)
		}
	}
}

// The compaction client rule: narrower than the label.
func TestNameIsNarrowerThanLabel(t *testing.T) {
	for ua, want := range map[string]string{
		"claude-cli/2.1.0 (external, cli)": ClientClaude, "claude-code/1": ClientClaude, "bobshell/1.0": ClientBob, "codex_cli_rs/0.5": ClientCodex,
		"codex_exec/1": ClientCodex, "something bob something": "", "my-codex-thing/1": "", "curl/8": "", "": "", "opencode/1": ClientOpenCode,
	} {
		if got := IdentifyUserAgent(ua).Name; got != want {
			t.Errorf("Name(%q) = %q, want %q", ua, got, want)
		}
	}
}

func TestEntrypoint(t *testing.T) {
	for ua, want := range map[string]string{
		"claude-cli/2.1.0 (external, cli)":     "cli",
		"claude-cli/2.1.0 (external, sdk-cli)": "headless",
		"claude-cli/2.1.0 (external, sdk-ts)":  "sdk",
		"claude-cli/2.1.0 (external, vscode)":  "other",
		"codex_cli_rs/0.5 (Mac)":               "cli",
		"curl/8":                               "other",
		"":                                     "",
	} {
		if got := IdentifyUserAgent(ua).Entrypoint; got != want {
			t.Errorf("Entrypoint(%q) = %q, want %q", ua, got, want)
		}
	}
}

func TestInvalidClaudeAgentIDIsDropped(t *testing.T) {
	if c := Identify(hdr("x-claude-code-agent-id", "bad id!")); c.AgentID != "" || c.AgentIDHeader != "" {
		t.Fatalf("got %+v", c)
	}
}

func TestClaudeCodeSubagent(t *testing.T) {
	c := Identify(hdr("User-Agent", "claude-cli/2.1.0 (external, cli)", "x-claude-code-agent-id", " sub1 "))
	if c.Name != ClientClaude || c.AgentID != "sub1" || c.AgentIDHeader != "x-claude-code-agent-id" {
		t.Fatalf("got %+v", c)
	}
	// The header is read whatever the User-Agent says, as the proxy did before.
	if c := Identify(hdr("x-claude-code-agent-id", "s")); c.AgentID != "s" || c.Name != "" {
		t.Fatalf("got %+v", c)
	}
	if c := Identify(hdr("User-Agent", "claude-cli/2.1.0 (external, cli)")); c.AgentID != "" || c.AgentIDHeader != "" {
		t.Fatalf("main thread got %+v", c)
	}
}

func TestCodexIds(t *testing.T) {
	c := Identify(hdr("User-Agent", "codex_cli_rs/0.162.1", "session-id", "root", "thread-id", "t1", "x-codex-parent-thread-id", "t0"))
	want := Client{Label: "codex", SDK: "codex", Name: ClientCodex, Entrypoint: "cli", SessionID: "root", AgentID: "t1", AgentIDHeader: "thread-id", ParentID: "t0"}
	if c != want {
		t.Fatalf("got %+v, want %+v", c, want)
	}
	// The originator header counts as the mark when the User-Agent is not Codex's.
	if c := Identify(hdr("User-Agent", "node", "originator", "Codex_Exec", "session-id", "root")); c.Name != ClientCodex || c.SessionID != "root" {
		t.Fatalf("originator: got %+v", c)
	}
	// A client that is not Codex gets no Codex ids.
	if c := Identify(hdr("User-Agent", "curl/8", "session-id", "x")); c.SessionID != "" || c.Name != "" {
		t.Fatalf("non-codex: got %+v", c)
	}
}

func TestOpenCodeIds(t *testing.T) {
	c := Identify(hdr("User-Agent", "opencode/1.2.3", "x-opencode-session-id", "ses_b", "x-opencode-parent-session-id", "ses_a"))
	if c.Name != ClientOpenCode || c.Label != "opencode" || c.SessionID != "ses_b" || c.ParentID != "ses_a" {
		t.Fatalf("got %+v", c)
	}
}

func TestPiMarkBeatsTheSDKUserAgent(t *testing.T) {
	c := Identify(hdr("User-Agent", "Anthropic/JS 0.60.0", "x-context-guru-client", " PI "))
	if c.Label != "pi" || c.SDK != "anthropic" || c.Name != ClientPi || c.Entrypoint != "other" {
		t.Fatalf("got %+v", c)
	}
	if c := Identify(hdr("User-Agent", "Anthropic/JS 0.60.0", "x-context-guru-client", "codex")); c.Name != "" || c.Label != "anthropic" {
		t.Fatalf("an unknown mark must be ignored, got %+v", c)
	}
}
