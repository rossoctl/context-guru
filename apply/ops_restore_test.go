package apply

import (
	"context"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/components"
	cf "github.com/rossoctl/context-guru/counterfactual"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

type toolCut struct{}

func (toolCut) Name() string                 { return "toolcut" }
func (toolCut) Enabled(*components.Ctx) bool { return true }
func (toolCut) Reformat(req *bschemas.BifrostChatRequest, _ *components.Report, _ *components.Ctx) error {
	for i := range req.Input {
		if req.Input[i].Role == bschemas.ChatMessageRoleTool {
			schema.SetMessageText(&req.Input[i], "cut")
		}
	}
	return nil
}

func restoreRun(t *testing.T, withAnchor bool) Result {
	t.Helper()
	pipe := components.NewPipeline([]components.Component{toolCut{}}, nil)
	pipe.SetFixedRestore(true)
	st := store.NewMemory(store.Options{})
	if withAnchor {
		msgs := gjson.Get(restoreBody, "messages").Array()
		sess := SessionIDFor("", "", bschemas.Anthropic, []byte(restoreBody))
		expand.RecordAnchor(st, sess, expand.Anchor{ID: "H1", At: 2, Fp: expand.Fingerprint(msgs[1])})
		st.Put("H1", []byte("ORIGINAL OUTPUT"))
	}
	return BodyOpts(context.Background(), pipe, st, Opts{Provider: bschemas.Anthropic, Body: []byte(restoreBody)})
}

// Control: with an empty plan the op for the rewritten tool_result (wire message 1) is kept.
func TestOpsKeptWhenRestorePlanEmpty(t *testing.T) {
	res := restoreRun(t, false)
	if res.Restored != 0 || len(res.Trace.Ops) != 1 || res.Trace.Ops[0].At.Index != 1 || res.Trace.OpsDropped != 0 {
		t.Fatalf("restored=%d ops=%+v dropped=%d", res.Restored, res.Trace.Ops, res.Trace.OpsDropped)
	}
}

// A successful insertion shifts later wire indices, so every op is dropped, not misplaced.
func TestOpsDroppedWhenRestoreInserts(t *testing.T) {
	res := restoreRun(t, true)
	if res.Restored != 1 {
		t.Fatalf("precondition: a copy must be inserted, Restored=%d", res.Restored)
	}
	if len(res.Trace.Ops) != 0 || res.Trace.OpsDropped != 1 {
		t.Fatalf("ops=%+v dropped=%d, want none and 1", res.Trace.Ops, res.Trace.OpsDropped)
	}
}

// A failed insertion forwards the pre-pipeline body: no op describes it.
func TestOpsDroppedWhenRestoreInsertFallsBack(t *testing.T) {
	orig := []byte(restoreBody)
	res := Result{Body: []byte(`{"model":"claude-x"}`)}
	res.Trace.Ops = []cf.Operation{{Kind: cf.KindRewrite, At: cf.Position{Section: cf.SectionMessages, Index: 1}}}
	res.Trace.OpsDropped = 2
	plan := restorePlan{{anchor: expand.Anchor{ID: "H1", At: 2}, original: "ORIGINAL OUTPUT"}}
	got := finishRestore(res, plan, "anthropic", orig, gjson.GetBytes(orig, "messages").Array())
	if string(got.Body) != restoreBody || got.Restored != 0 {
		t.Fatalf("want the original body and no restore, got %s restored=%d", got.Body, got.Restored)
	}
	if len(got.Trace.Ops) != 0 || got.Trace.OpsDropped != 3 {
		t.Fatalf("ops=%+v dropped=%d, want none and 3", got.Trace.Ops, got.Trace.OpsDropped)
	}
}
