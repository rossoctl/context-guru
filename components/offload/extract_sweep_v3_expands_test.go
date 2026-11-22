package offload

import (
	"maps"
	"testing"

	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
)

// #1191 D5 input (Phase 7, M3): the expand rate after a sweep cut, per rule and per judge, read per
// route from the dashboard after a restart. The dashboard keeps each component's events per request,
// with the route on the request, so the denominator is an event: every v3 drop names what chose it.
func TestAV3CutRaisesOneDroppedEventPerRuleAndJudge(t *testing.T) {
	r := newV3Rig(t, "v3-dropped-ev", v3Hybrid)
	r.request(0)
	r.request(175_000)
	r.request(175_000)
	r.forward()

	_, rep := r.request(175_000)

	want := map[string]int{
		"sweep_v3_dropped_" + sourceJudge:    4,
		"sweep_v3_dropped_" + ruleFileReread: 1,
		"sweep_v3_dropped_" + ruleBuildRerun: 1,
	}
	for name, n := range want {
		if rep.Events[name] != n {
			t.Fatalf("%s = %d, want %d (events %v)", name, rep.Events[name], n, rep.Events)
		}
	}
}

// The numerator: the host learns, from the expand note, what chose the drop it gave back.
func TestTheExpandNoteNamesWhatChoseTheDrop(t *testing.T) {
	r := newV3Rig(t, "v3-expand-rule", v3Hybrid)
	r.request(0)
	r.request(175_000)
	r.request(175_000)
	r.forward()
	req, _ := r.request(175_000)

	got := map[string]int{}
	for _, i := range toolIndices(req) {
		ids := expand.ParseMarkers(schema.MessageText(req.Input[i]))
		if len(ids) == 0 {
			continue
		}
		note := NoteSweepExpand(r.st, ids[0], 100, r.key())
		if !note.First {
			t.Fatalf("expand of %s: not a first expand: %+v", ids[0], note)
		}
		for rule, n := range note.Rules {
			got[rule] += n
		}
	}
	if got[sourceJudge] != 4 || got[ruleFileReread] != 1 || got[ruleBuildRerun] != 1 {
		t.Fatalf("expand notes by rule = %v, want judge 4, file_reread 1, build_rerun 1", got)
	}
}

// #1203 review: two threads that dropped the same bytes share one marker, and each raised its own
// dropped event. The repair path expands with no thread, so both owners get a first expand. The note
// must count both, per rule, or the dashboard shows dropped 2 and expanded 1 for one real expand.
func TestAnExpandWithNoThreadCountsEveryOwnersFirstExpandPerRule(t *testing.T) {
	resetSweepLedgerForTest()
	t.Cleanup(resetSweepLedgerForTest)
	st := newMemStore()
	original := "the bytes both threads dropped"
	SeedSweepDropRuleForTest(st, "parent", original, "HASH", sourceJudge)
	SeedSweepDropRuleForTest(st, "child", original, "HASH", ruleFileReread)

	note := NoteSweepExpand(st, "HASH", 100, "")

	want := map[string]int{sourceJudge: 1, ruleFileReread: 1}
	if !maps.Equal(note.Rules, want) {
		t.Fatalf("note.Rules = %v, want %v", note.Rules, want)
	}
	if again := NoteSweepExpand(st, "HASH", 100, ""); len(again.Rules) != 0 {
		t.Fatalf("a second expand counted again: %v", again.Rules)
	}
}
