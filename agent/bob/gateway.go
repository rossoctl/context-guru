package bob

import "github.com/rossoctl/context-guru/agent"

// Bob's client calls the host at its root: the model route, and its control-plane calls on any
// other path, which the host forwards through its catch-all.
var (
	_ agent.Gateway      = Bob{}
	_ agent.ControlPlane = Bob{}
)

// Gateway: Bob's model route is OpenAI chat, and Bob owns the host's catch-all.
func (Bob) Gateway() agent.GatewaySpec {
	return agent.GatewaySpec{Dialect: "openai", CatchAll: true}
}

// WantsReply is true for the profile GET, and for the model/info GET when not hosted. The
// model-info half is local-only: Bob is not deployed on hosted for now (owner, 2026-11-12), so the
// rule was not designed for hosted Bob. See docs/reference/hosted-vs-local.md.
func (Bob) WantsReply(c agent.ControlCall) bool {
	return IsProfile(c.Method, c.Path) || (!c.Hosted && IsModelInfo(c.Method, c.Path))
}

// Reply changes or reads the two replies WantsReply asks for.
//
// The profile goes out with the per-instance region fields removed. Bob's client does not keep the
// base URL it was configured with. Having fetched the profile, `Pc.resolveBaseUrl` (bobshell 1.0.6)
// replaces the HOSTNAME of that URL with `api.<region_domain>` from the profile, and keeps the
// scheme and the PORT. Pointed at a host on 127.0.0.1:4111, its next call goes to
// http://api.example.com:4111, where nothing listens: seen live as "Request failed after 6
// attempts: fetch failed", with no model request reaching the host. The rewrite is skipped when
// the instance has no region, so dropping those two fields keeps the client on the configured
// endpoint. A body that is not a profile gives the zero reply: the host sends it unchanged.
//
// The model/info reply goes out unchanged. Reply gives the window Bob compacts against for each
// model (#963), as ModelWindows reads it.
func (Bob) Reply(c agent.ControlCall, body []byte) agent.ControlReply {
	switch {
	case IsProfile(c.Method, c.Path):
		if out, ok := StripProfileRegion(body); ok {
			return agent.ControlReply{Body: out}
		}
	case !c.Hosted && IsModelInfo(c.Method, c.Path):
		return agent.ControlReply{Windows: ModelWindows(body)}
	}
	return agent.ControlReply{}
}
