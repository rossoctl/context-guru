package proxy

import (
	"context"
	"fmt"

	"github.com/rossoctl/context-guru/agent"
	"github.com/rossoctl/context-guru/internal/logging"
)

// readAgentUsage is a.ReadUsage on the token counts of base, fail open: a panic in the agent gives
// base unchanged. The agent sees and returns only the token counts and the backend label. Every other field of base
// (the searches, the live facts, the stop reason, the tool names) stays as the shared parser read
// it.
func readAgentUsage(ctx context.Context, a agent.Agent, contentType string, reply []byte, base Usage) (out Usage) {
	defer func() {
		if rec := recover(); rec != nil {
			// The panic's type only: its message could hold reply text.
			logging.From(ctx).Error("context-guru: recovered from panic in the agent's usage reading; using the shared reading",
				"agent", fmt.Sprintf("%T", a), "panic", fmt.Sprintf("%T", rec))
			out = base
		}
	}()
	got := a.ReadUsage(contentType, reply, agent.Usage{
		FreshInput: base.FreshInput, CacheRead: base.CacheRead, CacheWrite: base.CacheWrite,
		CacheWrite1h: base.CacheWrite1h, Output: base.Output, Backend: base.Backend,
	})
	out = base
	out.FreshInput, out.CacheRead, out.CacheWrite = got.FreshInput, got.CacheRead, got.CacheWrite
	out.CacheWrite1h, out.Output = got.CacheWrite1h, got.Output
	out.Backend = got.Backend
	return out
}
