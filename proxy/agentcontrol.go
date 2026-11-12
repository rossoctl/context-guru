package proxy

import (
	"context"
	"fmt"

	"github.com/rossoctl/context-guru/agent"
	"github.com/rossoctl/context-guru/internal/logging"
)

// agentWantsReply is cp.WantsReply, fail open: a panic gives false, so the reply streams as on any
// other path.
func agentWantsReply(ctx context.Context, cp agent.ControlPlane, c agent.ControlCall) (wants bool) {
	defer func() {
		if rec := recover(); rec != nil {
			// The panic's type only: its message could hold a path or reply text.
			logging.From(ctx).Error("context-guru: recovered from panic in the agent's control-plane check; streaming the reply unchanged",
				"agent", fmt.Sprintf("%T", cp), "panic", fmt.Sprintf("%T", rec))
			wants = false
		}
	}()
	return cp.WantsReply(c)
}

// agentReply is cp.Reply, fail open: a panic gives the zero reply, so the host sends the original
// bytes and stores no windows.
func agentReply(ctx context.Context, cp agent.ControlPlane, c agent.ControlCall, body []byte) (out agent.ControlReply) {
	defer func() {
		if rec := recover(); rec != nil {
			// The panic's type only: its message could hold reply text.
			logging.From(ctx).Error("context-guru: recovered from panic in the agent's control-plane reply; sending the reply unchanged",
				"agent", fmt.Sprintf("%T", cp), "panic", fmt.Sprintf("%T", rec))
			out = agent.ControlReply{}
		}
	}()
	return cp.Reply(c, body)
}
