package proxy

import (
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/internal/waitcause"
	"github.com/rossoctl/context-guru/keepalive"
)

// #1225: the keeper plans before the dashboard writer observes the request body. The keeper's
// ObserveTasks hook reads the body itself, so a turn that started a background task counts it.
func TestKeeperObserveTasksSeesTheBackgroundTaskOfThisBody(t *testing.T) {
	h := &Handler{waits: waitcause.New()}
	body := []byte(`{"model":"claude-sonnet-5-5","messages":[
	{"role":"user","content":"build"},
	{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"Bash","input":{"command":"make","run_in_background":true}}]},
	{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"Command running in background with ID: bash1."}]}]}`)
	key := keepalive.Key{Tenant: "t", Session: "s"}
	if bg, _ := h.waits.OpenCounts("t", "s", ""); bg != 0 {
		t.Fatalf("stored background count before the body = %d, want 0", bg)
	}
	seen := observeTasksFor(h)
	if bg, _ := seen(key, bschemas.Anthropic, body); bg != 1 {
		t.Fatalf("ObserveTasks background = %d, want 1", bg)
	}
}
