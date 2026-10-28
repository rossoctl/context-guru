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
	if o.Kind != cf.KindRewrite || o.Operator != "context-guru.mask" || o.ChangeID != "cg:mask:1:aa11" {
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

func TestOpsFromRun_CountChangedRule(t *testing.T) {
	ch := func(h string) []components.Change {
		return []components.Change{{Idx: 0, BytesBefore: 10, BytesAfter: 1, Hash: h}}
	}
	sum := func() components.Report {
		return components.Report{Component: "sum", CountChanged: true, Changes: []components.Change{
			{Idx: 1, BytesBefore: 8, BytesAfter: 1, Hash: "ss", Splice: &components.Splice{At: 1, Removed: 2, Inserted: 1}}}}
	}
	mid := components.RunReport{Components: []components.Report{
		{Component: "a", Changes: ch("1")}, sum(), {Component: "b", Changes: ch("2")},
	}}
	ops, dropped := OpsFromRun(mid, []int{0, 1, 2, 3}, true)
	if len(ops) != 2 || ops[0].ChangeID != "cg:a:0:1" || ops[1].MessageSplice == nil || dropped != 1 {
		t.Fatalf("CountChanged in the middle: ops = %+v dropped = %d", ops, dropped)
	}
	last := components.RunReport{Components: []components.Report{{Component: "a", Changes: ch("1")}, sum()}}
	ops, dropped = OpsFromRun(last, []int{0, 1, 2, 3}, true)
	if len(ops) != 2 || dropped != 0 {
		t.Fatalf("CountChanged last: ops = %+v dropped = %d", ops, dropped)
	}
}

func spliceReport(at, removed, inserted int) components.RunReport {
	return components.RunReport{Components: []components.Report{{Component: "sum", CountChanged: true,
		Changes: []components.Change{{Idx: at, BytesBefore: 8, BytesAfter: 2, Hash: "ss",
			Splice: &components.Splice{At: at, Removed: removed, Inserted: inserted}}}}}}
}

func TestOpsFromRun_SpliceMapsToWire(t *testing.T) {
	// slots 1 and 2 are one wire message (1); slot 0 is wire 0, slot 3 is wire 2. Dense.
	wire := []int{0, 1, 1, 2}
	ops, dropped := OpsFromRun(spliceReport(1, 2, 1), wire, true)
	if dropped != 0 || len(ops) != 1 {
		t.Fatalf("ops = %+v dropped = %d", ops, dropped)
	}
	o := ops[0]
	if *o.MessageSplice != (cf.MessageSplice{At: 1, Removed: 1, Inserted: 1}) || o.At.Index != 1 ||
		o.SizeBefore.Bytes != 8 || o.SizeAfter.Bytes != 2 || o.ChangeID != "cg:sum:1:ss" {
		t.Fatalf("op = %+v splice %+v", o, o.MessageSplice)
	}
}

func TestOpsFromRun_SpliceAtZero(t *testing.T) {
	ops, dropped := OpsFromRun(spliceReport(0, 1, 0), []int{0, 1, 2}, true)
	if dropped != 0 || len(ops) != 1 || *ops[0].MessageSplice != (cf.MessageSplice{At: 0, Removed: 1, Inserted: 0}) {
		t.Fatalf("ops = %+v dropped = %d", ops, dropped)
	}
}

func TestOpsFromRun_SpliceAppendedAtTheEnd(t *testing.T) {
	// the last wire message (5) spans two slots: one past it is 6, not len(wire) = 3.
	ops, dropped := OpsFromRun(spliceReport(3, 0, 1), []int{0, 5, 5}[:3], true)
	if dropped != 0 || len(ops) != 1 || *ops[0].MessageSplice != (cf.MessageSplice{At: 6, Removed: 0, Inserted: 1}) {
		t.Fatalf("ops = %+v dropped = %d", ops, dropped)
	}
	if ops[0].At.Index != 6 {
		t.Fatalf("At = %+v", ops[0].At)
	}
}

func TestOpsFromRun_SplicePartOfASharedWireMessageIsDropped(t *testing.T) {
	wire := []int{0, 1, 1, 2}
	for _, c := range []struct{ at, rm, in int }{{1, 1, 1}, {2, 1, 0}, {2, 0, 1}, {0, 2, 1}} {
		ops, dropped := OpsFromRun(spliceReport(c.at, c.rm, c.in), wire, true)
		if len(ops) != 0 || dropped != 1 {
			t.Fatalf("splice %+v: ops = %+v dropped = %d, want dropped", c, ops, dropped)
		}
	}
}

func TestOpsFromRun_ChangeAfterASpliceStaysDropped(t *testing.T) {
	rr := spliceReport(1, 1, 1)
	rr.Components = append(rr.Components, components.Report{Component: "mask",
		Changes: []components.Change{{Idx: 0, BytesBefore: 5, BytesAfter: 1, Hash: "m"}}})
	ops, dropped := OpsFromRun(rr, []int{0, 1, 2}, true)
	if len(ops) != 1 || ops[0].MessageSplice == nil || dropped != 1 {
		t.Fatalf("ops = %+v dropped = %d", ops, dropped)
	}
}

func TestOpsFromRun_SecondSpliceIsDropped(t *testing.T) {
	rr := spliceReport(1, 1, 1)
	rr.Components = append(rr.Components, spliceReport(0, 1, 0).Components...)
	ops, dropped := OpsFromRun(rr, []int{0, 1, 2}, true)
	if len(ops) != 1 || dropped != 1 {
		t.Fatalf("ops = %+v dropped = %d", ops, dropped)
	}
}

// wire {0,3,3,4} means wire messages 1 and 2 were unparseable and have no slot.
func TestOpsFromRun_SpliceAcrossAWireHoleIsDropped(t *testing.T) {
	wire := []int{0, 3, 3, 4}
	for _, c := range []struct{ at, rm, in int }{{1, 2, 1}, {0, 1, 1}, {0, 1, 0}, {1, 0, 1}} {
		ops, dropped := OpsFromRun(spliceReport(c.at, c.rm, c.in), wire, true)
		if len(ops) != 0 || dropped != 1 {
			t.Fatalf("splice %+v: ops = %+v dropped = %d, want dropped", c, ops, dropped)
		}
	}
}

func TestOpsFromRun_SpliceBeforeAWireHoleIsKept(t *testing.T) {
	wire := []int{0, 1, 2, 5, 6}
	ops, dropped := OpsFromRun(spliceReport(0, 2, 1), wire, true)
	// end=2: pair (wire[1], wire[2]) = (1,2) is dense, so it is kept.
	if dropped != 0 || len(ops) != 1 || *ops[0].MessageSplice != (cf.MessageSplice{At: 0, Removed: 2, Inserted: 1}) {
		t.Fatalf("ops = %+v dropped = %d", ops, dropped)
	}
	// ending at slot 3 touches the pair (2,5): dropped.
	ops, dropped = OpsFromRun(spliceReport(1, 2, 1), wire, true)
	if len(ops) != 0 || dropped != 1 {
		t.Fatalf("ops = %+v dropped = %d, want dropped", ops, dropped)
	}
}
