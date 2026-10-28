package apply_test

import (
	"context"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/store"
)

// Two tool_results in ONE wire message (index 2) become two req.Input entries (2 and 3); both
// operations must land on wire index 2, and neither on the req.Input index.
func TestBodyOptsTraceOpsCarryWireIndex(t *testing.T) {
	long := strings.Repeat("a long original tool output line that the component will rewrite\\n", 8)
	body := []byte(`{"model":"claude-x","messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"R","input":{}},{"type":"tool_use","id":"b","name":"R","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"` + long + `"},{"type":"tool_result","tool_use_id":"b","content":"` + long + `x"}]}
	]}`)
	for _, tc := range []struct {
		mode   components.Mode
		dryRun bool
	}{{components.ModeSync, false}, {components.ModeObserve, true}} {
		p, err := pipe(t, "pipeline: [attr2]\n").Build(nil)
		if err != nil {
			t.Fatal(err)
		}
		res := apply.BodyOpts(context.Background(), p, store.NewMemory(store.Options{}), apply.Opts{
			Provider: bschemas.Anthropic, Body: body, Mode: tc.mode,
		})
		if len(res.Trace.Ops) == 0 {
			t.Fatalf("%s: Trace.Ops empty (dropped %d)", tc.mode, res.Trace.OpsDropped)
		}
		for _, o := range res.Trace.Ops {
			if o.At.Index != 2 || o.DryRun != tc.dryRun || o.Operator != "context-guru.attr2" {
				t.Fatalf("%s: op = %+v", tc.mode, o)
			}
		}
	}
}
