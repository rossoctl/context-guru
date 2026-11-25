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
