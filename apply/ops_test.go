package apply

import (
	"testing"

	"github.com/rossoctl/context-guru/components"
	cf "github.com/rossoctl/context-guru/counterfactual"
)

func TestOpsFromRun_MapsToWirePositionAndSkipsRevertedAndSkipped(t *testing.T) {
	rr := components.RunReport{Components: []components.Report{
		{Component: "mask", Changes: []components.Change{{Idx: 1, BytesBefore: 4000, BytesAfter: 100, Hash: "aa11"}}},
		{Component: "dedup", Reverted: true, Changes: []components.Change{{Idx: 0, BytesBefore: 9, BytesAfter: 1, Hash: "bb22"}}},
		{Component: "toon", Skipped: true},
	}}
	ops, dropped := OpsFromRun(rr, []int{0, 3}, true)
	if dropped != 0 || len(ops) != 1 {
		t.Fatalf("ops = %+v dropped = %d", ops, dropped)
	}
	o := ops[0]
	if o.Kind != cf.KindRewrite || o.Operator != "context-guru.mask" || o.ChangeID != "cg:mask:aa11" {
		t.Fatalf("op = %+v", o)
	}
	if o.At != (cf.Position{Section: cf.SectionMessages, Index: 3}) {
		t.Fatalf("At = %+v, want messages[3] (wire index, not req.Input index)", o.At)
	}
	if o.SizeBefore.Bytes != 4000 || o.SizeAfter.Bytes != 100 || o.DryRun {
		t.Fatalf("sizes/dryrun = %+v", o)
	}
}

func TestOpsFromRun_DryRunWhenNotApplied(t *testing.T) {
	rr := components.RunReport{Components: []components.Report{
		{Component: "mask", Changes: []components.Change{{Idx: 0, BytesBefore: 10, BytesAfter: 1, Hash: "cc"}}},
	}}
	ops, _ := OpsFromRun(rr, []int{0}, false)
	if len(ops) != 1 || !ops[0].DryRun {
		t.Fatalf("a run that was not applied must be DryRun: %+v", ops)
	}
}

func TestOpsFromRun_ObserveReportIsDryRunEvenWhenApplied(t *testing.T) {
	rr := components.RunReport{Components: []components.Report{
		{Component: "mask", Mode: components.ModeObserve, Changes: []components.Change{{Idx: 0, BytesBefore: 10, BytesAfter: 1, Hash: "cc"}}},
	}}
	ops, _ := OpsFromRun(rr, []int{0}, true)
	if len(ops) != 1 || !ops[0].DryRun {
		t.Fatalf("an observe-mode report must be DryRun: %+v", ops)
	}
}

func TestOpsFromRun_NilWireDropsEverything(t *testing.T) {
	rr := components.RunReport{Components: []components.Report{
		{Component: "mask", Changes: []components.Change{{Idx: 0, BytesBefore: 10, BytesAfter: 1, Hash: "cc"}}},
	}}
	ops, dropped := OpsFromRun(rr, nil, true)
	if len(ops) != 0 || dropped != 1 {
		t.Fatalf("ops = %+v dropped = %d", ops, dropped)
	}
}
