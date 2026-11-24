package proxy

import (
	"net/http"

	"github.com/rossoctl/context-guru/agent"
)

// identify names the client of a request (agent.Identify). A nil request gives the generic result.
func identify(r *http.Request) agent.Client {
	if r == nil {
		return agent.Identify(nil)
	}
	return agent.Identify(r.Header)
}

// modelAgentOf is the agent label the prediction model reads from a stored row. Before the Pi label,
// a Pi row held the token of the provider SDK's User-Agent, and the model's agent feature one-hots
// "openai" (and "asyncopenai", "claude-cli"). A Pi row now holds "pi", so the provider column gives
// a token back: "openai" and "anthropic" are the SDK tokens Pi's two routed providers send. Any
// other provider has no known token: the provider string is returned, which the model reads as an
// unseen value, as it read the old unknown SDK token. Every other label passes through.
func modelAgentOf(label, provider string) string {
	if label != agent.ClientPi {
		return label
	}
	return provider
}
