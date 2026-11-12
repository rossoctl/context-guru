package agent

// Gateway is an optional capability: the agent's client calls the host on the agent's own paths,
// at the host's root. The host mounts each path in Paths() at the same path. A Gateway must have a
// Name: the host keys its upstream by it. Register refuses a Gateway with an empty Name.
type Gateway interface {
	Agent
	Gateway() GatewaySpec
}

// GatewaySpec is how the host mounts a Gateway.
type GatewaySpec struct {
	// Dialect is the wire dialect of the agent's model paths: "openai" today. The host refuses, at
	// start-up, a dialect it does not serve.
	Dialect string
	// CatchAll forwards every request no other route matched to this agent's upstream. At most one
	// agent has it: Register refuses a second one.
	CatchAll bool
}

// ControlPlane is an optional capability of a CatchAll Gateway: it reads or changes the reply to a
// control-plane call (a call that is not a model call). Like ModelTarget, it sees no header: the
// host keeps auth, the size limit and metering.
type ControlPlane interface {
	// WantsReply reports whether the agent reads the reply to c. True: the host asks the upstream
	// for an uncompressed reply, and calls Reply on a readable 200.
	WantsReply(c ControlCall) bool
	// Reply reads or changes the 200 reply body of c. body is at most the host's size limit. A
	// body the agent cannot read gives the zero value: the host sends the body unchanged.
	Reply(c ControlCall, body []byte) ControlReply
}

// ControlCall is the control-plane call the client made, as plain strings.
type ControlCall struct {
	Method string
	Path   string // without the query
	Hosted bool   // the host serves many tenants
}

// ControlReply is what the agent makes of a reply.
type ControlReply struct {
	Body    []byte         // nil: send the upstream body unchanged
	Windows map[string]int // model id -> context window from the agent's catalog; nil: none
}
