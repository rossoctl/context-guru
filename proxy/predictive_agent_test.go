package proxy

import (
	"net/http"
	"testing"

	"github.com/rossoctl/context-guru/agent"
	"github.com/rossoctl/context-guru/predictive"
)

// The live agent value must equal the value training reads for the same request (#1349).
// Training reads the stored label through agent.ModelAgentOf (campaign/replay.go). The dashboard
// row stores capture.agent.Label (capture.finish), so the stored label here is that field.
func TestLiveAgentEqualsTrainingAgent(t *testing.T) {
	cases := []struct {
		name, ua, pi, provider, want string
	}{
		{"claude code cli", "claude-cli/2.1.0 (external, cli)", "", "anthropic", "claude-cli"},
		{"claude agent sdk", "claude-code/2.1.0", "", "anthropic", "claude-code"},
		{"pi on anthropic", "Anthropic/JS 0.60.0", "pi", "anthropic", "anthropic"},
		{"pi on openai", "OpenAI/JS 5.0.0", "pi", "openai", "openai"},
		{"codex", "codex_cli_rs/0.50.0", "", "openai", "codex"},
		{"opencode", "opencode/1.2.3", "", "anthropic", "opencode"},
		{"empty user agent", "", "", "anthropic", "unknown"},
		{"unknown user agent", "mytool/9 extra", "", "anthropic", "mytool"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := http.NewRequest("POST", "http://x/v1/messages", nil)
			if tc.ua != "" {
				r.Header.Set("User-Agent", tc.ua)
			}
			if tc.pi != "" {
				r.Header.Set("x-context-guru-client", tc.pi)
			}
			cl := identify(r)
			c := &capture{agent: cl, provider: tc.provider}
			live := c.liveFacts(0).Agent
			training := agent.ModelAgentOf(cl.Label, tc.provider) // the stored row's label is cl.Label
			if live != training {
				t.Errorf("live %q != training %q", live, training)
			}
			if live != tc.want {
				t.Errorf("live agent = %q, want %q", live, tc.want)
			}
		})
	}
}

// A live claude-cli request sets the claude-cli one-hot of the model input.
func TestLiveClaudeCLISetsTheOneHot(t *testing.T) {
	r, _ := http.NewRequest("POST", "http://x/v1/messages", nil)
	r.Header.Set("User-Agent", "claude-cli/2.1.0 (external, cli)")
	c := &capture{agent: identify(r), provider: "anthropic"}
	tr := predictive.NewTracker()
	rq := predictive.Request{Tenant: "t", Session: "s", Model: "claude-opus-4", TsMs: 1000}
	arr := tr.Arrive(rq)
	resp := &predictive.Response{Request: rq, EndMs: 2000, DecMs: 2000, Agent: c.liveFacts(0).Agent, StopReason: "end_turn"}
	row := predictive.NewRow()
	tr.Features(arr, resp, row)
	idx := -1
	for i, n := range predictive.FeatureNames {
		if n == "ag_claude-cli" {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatal("no ag_claude-cli feature")
	}
	if row.F[idx] != 1 {
		t.Errorf("ag_claude-cli = %v, want 1", row.F[idx])
	}
}
