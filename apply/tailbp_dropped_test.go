package apply_test

import (
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"

	"github.com/rossoctl/accountant"
)

// accountant#70, case C. A request whose pipeline operations were dropped (here: the change is on a
// read-only sibling text slot, so it is never written back) still gets the tail mark (#795). The mark is
// on the wire, so the accountant must see it as an operation. The report was skipped when OpsDropped was
// above 0, so the request went to the accountant with 0 operations and a last message 37 bytes longer
// (`,"cache_control":{"type":"ephemeral"}`), and the sum check failed.
//
// The drop here comes from opsFromRun (the read-only slot). giveUp and finishRestore also clear
// Trace.Ops and raise OpsDropped, and the deferred hook only looks at the body, so it treats them the
// same. They are not driven through BodyOpts here: their failure conditions (a splice the body's bytes
// do not confirm, a restore copy that cannot be placed) are fail-open guards that a well-formed
// request does not reach. finishRestore's own side is covered by TestOpsDroppedWhenRestoreInsertFallsBack.
func TestTheTailMarkIsReportedWhenThePipelineOpsWereDropped(t *testing.T) {
	long := strings.Repeat("a long sibling text that the component rewrites to nothing\\n", 8)
	res := traceOf(t, "usershrink", bschemas.Anthropic, `{"model":"claude-x","messages":[
		{"role":"user","content":[{"type":"text","text":"`+long+`"},{"type":"tool_result","tool_use_id":"a","content":"out","cache_control":{"type":"ephemeral"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"b","content":"`+strings.Repeat("e", 5000)+`"}]}
	]}`)
	if res.Trace.OpsDropped == 0 {
		t.Fatal("precondition: the pipeline's change must be dropped")
	}
	if !res.Changed || !gjson.GetBytes(res.Body, "messages.1.content.0.cache_control").IsObject() {
		t.Fatalf("precondition: the tail mark must be on the last block:\n%s", res.Body)
	}
	if len(res.Trace.Ops) != 1 {
		t.Fatalf("ops = %+v, want the tail mark as the one operation", res.Trace.Ops)
	}
	op := res.Trace.Ops[0]
	if op.Kind != accountant.KindRewrite || op.Operator != "context-guru.tailbreakpoint" || op.At.Index != 1 ||
		op.SizeAfter.Bytes-op.SizeBefore.Bytes != len(`,"cache_control":{"type":"ephemeral"}`) {
		t.Fatalf("op = %+v, want the tail mark on messages[1], 37 bytes long", op)
	}
}
