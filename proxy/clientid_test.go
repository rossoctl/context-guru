package proxy

import (
	"github.com/rossoctl/context-guru/agent"
	"github.com/rossoctl/context-guru/compactionpoint"
	"github.com/rossoctl/context-guru/dash"
	"net/http/httptest"
	"testing"
)

func TestIdentifyNilRequestIsGeneric(t *testing.T) {
	if c := identify(nil); c.Label != "unknown" || c.Name != "" {
		t.Fatalf("identify(nil) = %+v", c)
	}
}

func TestClientHeaderIsNotForwarded(t *testing.T) {
	src := httptest.NewRequest("POST", "/", nil).Header
	src.Set("x-context-guru-client", "pi")
	dst := httptest.NewRequest("POST", "/", nil).Header
	copyHeaders(dst, src)
	if v := dst.Get("x-context-guru-client"); v != "" {
		t.Fatalf("x-context-guru-client was forwarded upstream: %q", v)
	}
}

func TestClientOfNamesMatchTheCompactionSwitch(t *testing.T) {
	for ua, want := range map[string]string{"claude-cli/2 (external, cli)": "claude", "bobshell/1": "bob", "codex_cli_rs/1": "codex", "opencode/1": "opencode", "": ""} {
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set("User-Agent", ua)
		if got := identify(r).Name; got != want {
			t.Errorf("%q: Name = %q, want %q", ua, got, want)
		}
	}
}

// This is the old compactionpoint.ClientOf table (#1320), now pinned against Identify. The Claude rule
// is also pinned to dash.AgentFor: both come from agent.Identify.
func TestCompactionClientKeepsTheOldPicks(t *testing.T) {
	for ua, want := range map[string]compactionpoint.Client{
		"claude-cli/2.1.319 (external, cli)": compactionpoint.ClientClaude,
		"Claude-Code/1.0":                    compactionpoint.ClientClaude,
		" claude-code/1":                     compactionpoint.ClientClaude,
		"x claude-cli":                       compactionpoint.ClientClaude,
		"cursor claude-code":                 compactionpoint.ClientClaude,
		"bobshell/1.0.3":                     compactionpoint.ClientBob,
		"codex_cli_rs/0.160.0":               compactionpoint.ClientCodex,
		"codex_exec":                         compactionpoint.ClientCodex,
		"curl/8":                             compactionpoint.ClientNone,
		"bobcat/1.0":                         compactionpoint.ClientNone,
		"my-bob-tool/2":                      compactionpoint.ClientNone,
		"my-claude-tool":                     compactionpoint.ClientNone,
		"xcodex/1":                           compactionpoint.ClientNone,
		"opencode/1":                         compactionpoint.ClientNone,
		"":                                   compactionpoint.ClientNone,
	} {
		cl := agent.IdentifyUserAgent(ua)
		if got := compactionClient(cl); got != want {
			t.Errorf("compactionClient(%q) = %q, want %q", ua, got, want)
		}
		if dashClaude := dash.AgentFor(ua) == "claude-code" || dash.AgentFor(ua) == "claude-cli"; dashClaude != (want == compactionpoint.ClientClaude) {
			t.Errorf("%q: dash.AgentFor = %q disagrees", ua, dash.AgentFor(ua))
		}
	}
}
