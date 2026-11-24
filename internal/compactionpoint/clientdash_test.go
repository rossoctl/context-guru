package compactionpoint_test

import (
	"testing"

	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/compactionpoint"
)

// ClientOf repeats dash.AgentFor's Claude rule (dash imports this package). Pin them together.
func TestClientOfClaudeMatchesDashAgentFor(t *testing.T) {
	for _, ua := range []string{"claude-cli/2.1.319 (external, cli)", "Claude-Code/1", " claude-code/1", "x claude-cli", "curl/8", "", "my-claude-tool", "cursor claude-code"} {
		dashSaysClaude := dash.AgentFor(ua) == "claude-code" || dash.AgentFor(ua) == "claude-cli"
		got := compactionpoint.ClientOf(ua) == compactionpoint.ClientClaude
		if got != dashSaysClaude {
			t.Errorf("UA %q: ClientOf claude = %v, dash.AgentFor = %q", ua, got, dash.AgentFor(ua))
		}
	}
}
