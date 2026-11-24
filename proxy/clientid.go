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

// modelAgentOf is the agent label the prediction model reads from a stored row. The model's agent
// feature has "openai" in it as a provider proxy: it was the SDK token of a client's User-Agent. A
// Pi row now stores the harness label "pi", so the provider column gives the old token back. Every
// other label passes through.
func modelAgentOf(label, provider string) string {
	if label != agent.ClientPi {
		return label
	}
	if provider == "openai" {
		return "openai"
	}
	return "anthropic"
}
