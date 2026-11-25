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

// These are the dashboard labels dash.AgentFor gave before this refactor. "opencode" is not in the known
// list: "opencode/<version>" gets its label from the first token, as before, and a User-Agent that holds
// "opencode" later keeps its first token.
func TestLabelsAreUnchanged(t *testing.T) {
	for ua, want := range map[string]string{
		"claude-cli/2.1.0 (external, cli)": "claude-cli", "Claude-Code/1": "claude-code", "codex_cli_rs/0.5": "codex",
		"Cursor/1": "cursor", "cline/3": "cline", "aider/1": "aider", "gemini-cli/1": "gemini-cli",
		"bobshell/1.0": "bob", "opencode/1.2.3": "opencode", "ai-sdk/x opencode/2": "ai-sdk",
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

func TestOpenCodeNameKeepsEveryOtherName(t *testing.T) {
	if got := IdentifyUserAgent("ai-sdk/x opencode/2").Name; got != "" {
		t.Errorf("a User-Agent with opencode mid-string: Name = %q, want empty", got)
	}
	c := Identify(hdr("User-Agent", "claude-cli/2.1.0 (external, cli)", "x-opencode-session-id", "ses_b"))
	if c.Name != ClientClaude || c.SessionID != "" {
		t.Errorf("a named client keeps its Name and gets no OpenCode id: %+v", c)
	}
	c = Identify(hdr("User-Agent", "ai-sdk/x", "x-opencode-session-id", "ses_b"))
	if c.Name != ClientOpenCode || c.Label != "ai-sdk" || c.SessionID != "ses_b" {
		t.Errorf("an unnamed client with OpenCode headers: %+v", c)
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

func TestModelAgentOfKeepsThePiProviderToken(t *testing.T) {
	for _, tc := range [][3]string{{"pi", "openai", "openai"}, {"pi", "anthropic", "anthropic"}, {"pi", "bedrock", "bedrock"}, {"claude-cli", "anthropic", "claude-cli"}, {"openai", "openai", "openai"}} {
		if got := ModelAgentOf(tc[0], tc[1]); got != tc[2] {
			t.Errorf("ModelAgentOf(%q, %q) = %q, want %q", tc[0], tc[1], got, tc[2])
		}
	}
}

func TestCompactionMark(t *testing.T) {
	if Identify(nil).Compaction {
		t.Error("no headers must give no mark")
	}
	if !Identify(hdr("User-Agent", "node", "x-context-guru-request-kind", "compaction")).Compaction {
		t.Error("the plugin header must mark a compaction")
	}
	meta := `{"request_kind":"compaction"}`
	c := Identify(hdr("User-Agent", "codex_cli_rs/1", "x-codex-turn-metadata", meta))
	if !c.Compaction || !MarkedCompaction(c, nil) {
		t.Errorf("Codex turn metadata in the header must mark: %+v", c)
	}
	// A WebSocket request carries it in the body: the header mark is false, MarkedCompaction reads the body.
	c = Identify(hdr("User-Agent", "codex_cli_rs/1"))
	body := []byte(`{"client_metadata":{"x-codex-turn-metadata":"{\"request_kind\":\"compaction\"}"}}`)
	if c.Compaction || !MarkedCompaction(c, body) {
		t.Errorf("body mark: Compaction=%v marked=%v", c.Compaction, MarkedCompaction(c, body))
	}
	// Any other client's body mark and header mark do not count.
	other := Identify(hdr("User-Agent", "curl/8", "x-codex-turn-metadata", meta))
	if other.Compaction || MarkedCompaction(other, body) {
		t.Errorf("a non-Codex caller must not mark: %+v", other)
	}
}

func TestClientSubagentID(t *testing.T) {
	h := func(kv ...string) http.Header {
		out := http.Header{}
		for i := 0; i < len(kv); i += 2 {
			out.Set(kv[i], kv[i+1])
		}
		return out
	}
	for _, tc := range []struct {
		name string
		in   http.Header
		want string
	}{
		{"no header", h(), ""},
		{"claude subagent", h("User-Agent", "claude-cli/2.1", "x-claude-code-agent-id", "agentS"), "agentS"},
		{"codex main", h("User-Agent", "codex_cli_rs/0.1", "thread-id", "t0"), ""},
		{"codex sub-agent", h("User-Agent", "codex_cli_rs/0.1", "thread-id", "t1", "x-codex-parent-thread-id", "t0"), "t1"},
		{"codex parent, no own id", h("User-Agent", "codex_cli_rs/0.1", "x-codex-parent-thread-id", "t0"), ""},
		{"opencode root", h("User-Agent", "opencode/2.4.1", "x-opencode-session-id", "ses_a"), ""},
		{"opencode child", h("User-Agent", "opencode/2.4.1", "x-opencode-session-id", "ses_b", "x-opencode-parent-session-id", "ses_a"), "ses_b"},
	} {
		if got := Identify(tc.in).SubagentID(); got != tc.want {
			t.Errorf("%s: SubagentID = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestClientModelAgentEqualsTheStoredRowValue(t *testing.T) {
	for _, tc := range []struct {
		name, provider, want string
		h                    http.Header
	}{
		{"claude code", "anthropic", "claude-cli", hdr("User-Agent", "claude-cli/2.1.0 (external, cli)")},
		{"codex", "openai", "codex", hdr("User-Agent", "codex_cli_rs/0.50.0")},
		{"opencode", "anthropic", "opencode", hdr("User-Agent", "opencode/1.2.3")},
		{"pi on anthropic", "anthropic", "anthropic", hdr("User-Agent", "Anthropic/JS 0.60.0", "x-context-guru-client", "pi")},
		{"pi on openai", "openai", "openai", hdr("User-Agent", "OpenAI/JS 5.0.0", "x-context-guru-client", "pi")},
		{"bob", "anthropic", "bob", hdr("User-Agent", "bobshell/1.0")},
		{"empty", "anthropic", "unknown", nil},
	} {
		c := Identify(tc.h)
		if got := c.ModelAgent(tc.provider); got != tc.want || got != ModelAgentOf(c.Label, tc.provider) {
			t.Errorf("%s: ModelAgent = %q, want %q", tc.name, got, tc.want)
		}
	}
}
