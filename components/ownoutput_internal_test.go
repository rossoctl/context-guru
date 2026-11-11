package components

import (
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/schema"
)

// ownOutputs runs before any component, outside runOne's recover, and some hosts (the bifrost
// adapter) call Pipeline.Run with no recover of their own: a panic in it must exempt nothing,
// never fail the request.
func TestOwnOutputsFailsOpen(t *testing.T) {
	orig := toolCallsOf
	defer func() { toolCallsOf = orig }()
	toolCallsOf = func(*schemas.BifrostChatRequest) map[int]schema.ToolCall { panic("boom") }
	if got := ownOutputs(&schemas.BifrostChatRequest{}); got != nil {
		t.Fatalf("got %v", got)
	}
}
