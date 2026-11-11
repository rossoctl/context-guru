package apply

import (
	"context"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/components"
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

// A successful insertion is the last operation of the request: the pipeline's own operations keep
// their positions, and the copy is reported as a message splice with its wire size (#481). Dropping
// them all made the accountant book no saving on every request that carried a restored copy.
func TestRestoreInsertionIsReportedAsTheLastOperation(t *testing.T) {
	res := restoreRun(t, true)
	if res.Restored != 1 {
		t.Fatalf("precondition: a copy must be inserted, Restored=%d", res.Restored)
	}
	if res.Trace.OpsDropped != 0 || len(res.Trace.Ops) != 2 {
		t.Fatalf("ops=%+v dropped=%d, want the tool rewrite and the restore", res.Trace.Ops, res.Trace.OpsDropped)
	}
	if tool := res.Trace.Ops[0]; tool.Operator != "context-guru.toolcut" || tool.At.Index != 1 || tool.MessageSplice != nil {
		t.Fatalf("the pipeline's operation must be kept as it was, got %+v", tool)
	}
	msgs := gjson.GetBytes(res.Body, "messages").Array()
	op := res.Trace.Ops[1]
	if op.Operator != "context-guru.expand_restore" || op.ChangeID != "cg:restore:H1" || op.DryRun {
		t.Fatalf("restore op = %+v", op)
	}
	if sp := op.MessageSplice; sp == nil || sp.At != 2 || sp.Removed != 0 || sp.Inserted != 1 || op.At.Index != 2 {
		t.Fatalf("restore op must insert one message at 2, got %+v at %+v", op.MessageSplice, op.At)
	}
	if op.SizeBefore.Bytes != 0 || op.SizeAfter.Bytes != len(msgs[2].Raw) || !strings.Contains(msgs[2].Raw, "ORIGINAL OUTPUT") {
		t.Fatalf("restore op sizes %d -> %d, inserted message %q", op.SizeBefore.Bytes, op.SizeAfter.Bytes, msgs[2].Raw)
	}
}

// The tokens a copy adds are on the wire, so the run's TokensAfter counts them, and a saving read
// off the run does not credit the compaction of an output that is back in full.
func TestRestoreInsertionCountsInTokensAfter(t *testing.T) {
	with, without := restoreRun(t, true), restoreRun(t, false)
	if with.Trace.Run == nil || without.Trace.Run == nil || with.RestoredTokens <= 0 {
		t.Fatalf("precondition: both runs report, and a copy adds tokens (%d)", with.RestoredTokens)
	}
	if got, want := with.Trace.Run.TokensAfter, without.Trace.Run.TokensAfter+with.RestoredTokens; got != want {
		t.Fatalf("TokensAfter = %d, want the run's %d plus the copy's %d", got, without.Trace.Run.TokensAfter, with.RestoredTokens)
	}
}

// Copies sort by position and each lands past those before it, whatever order the plan lists them.
func TestRestoreOpsRankByPosition(t *testing.T) {
	msg := func(s string) gjson.Result { return gjson.Parse(`{"role":"user","content":"` + s + `"}`) }
	nb := []gjson.Result{msg("a"), msg("COPY-B"), msg("b"), msg("COPY-A1"), msg("COPY-A2")}
	plan := restorePlan{{anchor: expand.Anchor{ID: "B"}}, {anchor: expand.Anchor{ID: "A1"}}, {anchor: expand.Anchor{ID: "A2"}}}
	ops, ok := plan.ops([]int{1, 2, 2}, nb)
	if !ok || len(ops) != 3 {
		t.Fatalf("ok=%v ops=%+v", ok, ops)
	}
	for k, want := range []struct {
		id  string
		pos int
	}{{"cg:restore:B", 1}, {"cg:restore:A1", 3}, {"cg:restore:A2", 4}} {
		if ops[k].ChangeID != want.id || ops[k].At.Index != want.pos || ops[k].MessageSplice.At != want.pos {
			t.Fatalf("op %d = %s at %d, want %s at %d", k, ops[k].ChangeID, ops[k].At.Index, want.id, want.pos)
		}
	}
	if _, ok := plan.ops([]int{1, 2, 9}, nb); ok {
		t.Fatal("a copy that is not in the body must make the report fail, not guess")
	}
}

// A failed insertion forwards the pre-pipeline body: no op describes it.
func TestOpsDroppedWhenRestoreInsertFallsBack(t *testing.T) {
	orig := []byte(restoreBody)
	res := Result{Body: []byte(`{"model":"claude-x"}`)}
	res.Trace.Ops = []accountant.Operation{{Kind: accountant.KindRewrite, At: accountant.Position{Section: accountant.SectionMessages, Index: 1}}}
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
