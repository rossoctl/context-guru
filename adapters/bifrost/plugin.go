// Package bifrost adapts context-guru's pipeline to bifrost's LLMPlugin
// interface: our components run as a pre-LLM-call hook (design D2). Registering
// this plugin via BifrostConfig.LLMPlugins is all it takes for an embedded
// bifrost proxy — or any bifrost deployment — to run context engineering.
//
// The same components package backs the AuthBridge in-process plugin (P3); only
// this thin adapter is bifrost-specific.
package bifrost

import (
	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/session"
	"github.com/rossoctl/context-guru/store"
)

// SessionContextKey is the BifrostContext value key the transport sets from the
// x-context-guru-session header (or Anthropic metadata.user_id). Absent -> the
// plugin falls back to a content hash.
const SessionContextKey bschemas.BifrostContextKey = "context-guru-session"

// Plugin runs the context-guru pipeline in PreRequestHook. It implements
// bifrost's schemas.LLMPlugin. It never aborts a request (fail-open): any
// component failure is already contained inside the pipeline.
type Plugin struct {
	pipe  *components.Pipeline
	store store.Store
	// CheapModel is the static "config"-source LLM for NeedsModel components. The
	// bifrost host has no usable "incoming" client (the agent's key is a
	// placeholder), so only the static source is offered here. Optional.
	CheapModel components.Model
}

// New builds the adapter from an already-constructed pipeline and store.
func New(pipe *components.Pipeline, st store.Store) *Plugin {
	return &Plugin{pipe: pipe, store: st}
}

func (*Plugin) GetName() string { return "context-guru" }
func (*Plugin) Cleanup() error  { return nil }

// PreRequestHook runs the pipeline over the chat request's messages. This is the
// canonical mutate-the-request phase: changes are committed and seen by the
// provider call and every fallback. Non-chat requests pass through untouched.
func (p *Plugin) PreRequestHook(ctx *bschemas.BifrostContext, req *bschemas.BifrostRequest) error {
	if req == nil || req.ChatRequest == nil {
		return nil
	}
	chat := req.ChatRequest
	c := &components.Ctx{
		Ctx:     ctx,
		Session: p.resolveSession(ctx, chat),
		Store:   p.store,
		Model:   components.ModelSpec{Static: p.CheapModel},
		Bypass:  bypassed(ctx),
	}
	p.pipe.Run(chat, c)
	return nil
}

// PreLLMHook and PostLLMHook are pass-throughs today; the expand continuation
// loop lives in the transport wrapper (it must re-invoke upstream, which a hook
// cannot). Kept here to satisfy the LLMPlugin interface.
func (p *Plugin) PreLLMHook(_ *bschemas.BifrostContext, req *bschemas.BifrostRequest) (*bschemas.BifrostRequest, *bschemas.LLMPluginShortCircuit, error) {
	return req, nil, nil
}

func (p *Plugin) PostLLMHook(_ *bschemas.BifrostContext, resp *bschemas.BifrostResponse, e *bschemas.BifrostError) (*bschemas.BifrostResponse, *bschemas.BifrostError, error) {
	return resp, e, nil
}

// resolveSession prefers an explicit id set on the context (from a header),
// falling back to a hash of the system prompt + first user message.
func (p *Plugin) resolveSession(ctx *bschemas.BifrostContext, chat *bschemas.BifrostChatRequest) string {
	explicit := ""
	if v := ctx.Value(SessionContextKey); v != nil {
		if s, ok := v.(string); ok {
			explicit = s
		}
	}
	sys, firstUser := schema.SessionHead(chat.Input)
	return session.Resolve(explicit, sys, firstUser)
}

// BypassContextKey lets a caller disable the pipeline for one request
// (x-context-guru-bypass header -> context value).
const BypassContextKey bschemas.BifrostContextKey = "context-guru-bypass"

func bypassed(ctx *bschemas.BifrostContext) bool {
	v := ctx.Value(BypassContextKey)
	b, _ := v.(bool)
	return b
}
