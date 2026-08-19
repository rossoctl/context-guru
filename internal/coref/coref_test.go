package coref

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// The fixture below is the Go twin of deploy/harbor/coref_fixture.py: four tool outputs
// whose correct classification is fixed by construction. It is duplicated rather than
// loaded so this package's test has no Python dependency, and so a change to the index
// that silently disagrees with the offline measurement fails HERE — the two must share
// one definition of a reference or the thresholds the measurement produces are
// calibrated for a different algorithm.
//
//	#1 src/auth.py read     -> closed       (novel TOKEN_GRACE_SECONDS lifted out once, early)
//	#2 src/config.py read   -> unreferenced (only overlap is the echoed path == the argument)
//	#3 ls -R listing        -> unreferenced (nothing ever comes back to it)
//	#4 pytest failure       -> open         (novel error id reused 3x, most recently 1 turn ago)

// filler builds per-output distinct lines. Shared filler would be dropped as session
// boilerplate, which would mask whether the novel-token logic works at all.
func filler(tag string, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "%4d\t%s_line_%d = compute_%s_%d(arg_%d)\n", i, tag, i, tag, i, i)
	}
	return b.String()
}

// fixture returns the flattened transcript, ending on the last MODEL turn. Ending there
// (rather than on the trailing tool result) is what the offline fixture captures, and it
// is the state in which the last turn's references are visible — recency is measured from
// the head, so the two must agree on where the head is.
func fixture() []Message {
	toolUse := func(name string, args map[string]any) string {
		b, _ := json.Marshal(args)
		return name + " " + string(b)
	}
	text := func(t string) Message { return Message{Texts: []string{t}} }
	asst := func(t, name string, args map[string]any) Message {
		return Message{Texts: []string{t, toolUse(name, args)}}
	}
	res := func(id, t string) Message { return Message{Results: []Result{{ID: id, Text: t}}} }

	msgs := []Message{
		text("Fix the failing test test_auth_expiry in src/auth.py"),

		// #1 closed
		asst("Reading the auth module.", "Read", map[string]any{"path": "src/auth.py"}),
		res("t1", "src/auth.py\n"+filler("auth", 240)+"\nTOKEN_GRACE_SECONDS = 0  # novel\n"),
		asst("The bug is TOKEN_GRACE_SECONDS is 0; it must be 300.", "Edit",
			map[string]any{"path": "src/auth.py", "old": "TOKEN_GRACE_SECONDS = 0", "new": "TOKEN_GRACE_SECONDS = 300"}),
		res("t2", "ok"),

		// #2 unreferenced — the echo confound
		asst("Checking config.", "Read", map[string]any{"path": "src/config.py"}),
		res("t3", "src/config.py\n"+filler("config", 240)),
		asst("Config is fine, adjusting anyway.", "Edit", map[string]any{"path": "src/config.py", "old": "a", "new": "b"}),
		res("t4", "ok"),

		// #3 unreferenced
		asst("Surveying the tree.", "Bash", map[string]any{"cmd": "ls -R"}),
		res("t5", filler("tree", 240)),

		// #4 open
		asst("Running the suite.", "Bash", map[string]any{"cmd": "pytest -q"}),
		res("t6", "1 failed, 42 passed\n"+filler("pytest", 240)+"\nE   AssertionError: XPIRE_DRIFT_7f3a\n"),
	}
	for k := 0; k < 3; k++ {
		msgs = append(msgs,
			asst(fmt.Sprintf("XPIRE_DRIFT_7f3a again; attempt %d.", k), "Bash", map[string]any{"cmd": "pytest -q"}),
		)
		if k < 2 { // the transcript ends on the model turn, so the last result is not sent
			msgs = append(msgs, res(fmt.Sprintf("r%d", k), "1 failed\nE   AssertionError: XPIRE_DRIFT_7f3a\n"))
		}
	}
	return msgs
}

// The offline pass's defaults, so the Go index is checked at the same operating point.
const (
	testClosedDist = 12
	testOpenReps   = 3
	testMinOutput  = 300
	// The fixture's outputs sit near the tail of a short transcript, so the opportunity
	// floor is disabled for the ground-truth cases; it has its own test below.
	testMinLater = 0
)

func classifyFixture(t *testing.T, guard bool) map[string]Class {
	t.Helper()
	recs := index(fixture(), testMinOutput, nil, guard)
	got := map[string]Class{}
	for _, r := range recs {
		got[r.ID] = Classify(r, testClosedDist, testOpenReps, testMinLater)
	}
	return got
}

func TestFixtureClassification(t *testing.T) {
	got := classifyFixture(t, true)
	want := map[string]Class{"t1": Closed, "t3": Unreferenced, "t5": Unreferenced, "t6": Open}
	if len(got) != len(want) {
		t.Fatalf("recorded outputs = %v, want exactly the four above min_output", got)
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("output %s classified %q, want %q", id, got[id], w)
		}
	}
}

// TestEchoGuardIsLoadBearing is the negative control. Without the prior-vocabulary
// exclusion, the src/config.py read is scored as REFERENCED — its only later overlap is
// the path that arrived as the tool-call argument, so the output introduced nothing that
// was carried forward. An index that gets this wrong reports nearly all mass as
// load-bearing, which reads as "there is nothing to cut" rather than as a bug.
func TestEchoGuardIsLoadBearing(t *testing.T) {
	if got := classifyFixture(t, true)["t3"]; got != Unreferenced {
		t.Fatalf("with the guard, t3 = %q, want %q", got, Unreferenced)
	}
	if got := classifyFixture(t, false)["t3"]; got == Unreferenced {
		t.Fatal("without the guard, t3 stayed unreferenced: the control proves nothing, " +
			"so the guard is no longer what produces the result")
	}
}

// Cuttable mass (unreferenced + closed) must be materially larger with the guard on.
// This is the measurement-level statement of the control: the guard's effect is not a
// reclassified edge case, it is most of the answer.
func TestEchoGuardChangesCuttableMass(t *testing.T) {
	mass := func(guard bool) (cuttable, total int) {
		for _, r := range index(fixture(), testMinOutput, nil, guard) {
			total += r.SizeTokens
			if c := Classify(r, testClosedDist, testOpenReps, testMinLater); c != Open {
				cuttable += r.SizeTokens
			}
		}
		return cuttable, total
	}
	onCut, onTotal := mass(true)
	offCut, offTotal := mass(false)
	if onTotal != offTotal || onTotal == 0 {
		t.Fatalf("total mass differs between arms (%d vs %d): the arms are not comparable", onTotal, offTotal)
	}
	if onCut <= offCut {
		t.Errorf("cuttable mass with guard = %d/%d, without = %d/%d; the guard must INCREASE it",
			onCut, onTotal, offCut, offTotal)
	}
}

func TestRecencyIsMeasuredFromTheHead(t *testing.T) {
	recs := index(fixture(), testMinOutput, nil, true)
	n := len(fixture())
	byID := map[string]Record{}
	for _, r := range recs {
		byID[r.ID] = r
	}
	// #1 is referenced once, by the turn immediately after it. Its RefAge must be the
	// distance from the HEAD (large — the reference is ancient), while its ConsumeLag is
	// small (the value was taken immediately). Swapping the two is the modelling error
	// this assertion exists to catch: it makes every early output look freshly used.
	r := byID["t1"]
	if r.Refs != 1 {
		t.Fatalf("t1 refs = %d, want 1", r.Refs)
	}
	if r.RefAge != n-3 {
		t.Errorf("t1 RefAge = %d, want %d (messages ago, from the head)", r.RefAge, n-3)
	}
	if r.ConsumeLag != 1 {
		t.Errorf("t1 ConsumeLag = %d, want 1 (the very next turn took the value)", r.ConsumeLag)
	}
	if r.RefAge <= r.ConsumeLag {
		t.Error("t1 RefAge must exceed ConsumeLag here; the two axes have been conflated")
	}
	// An unreferenced output reports both as absent rather than as zero — zero would read
	// as "referenced by the current turn", the opposite of the truth.
	if u := byID["t5"]; u.RefAge != -1 || u.ConsumeLag != -1 {
		t.Errorf("t5 (unreferenced) RefAge/ConsumeLag = %d/%d, want -1/-1", u.RefAge, u.ConsumeLag)
	}
}

func TestUsedFracShowsPartialConsumption(t *testing.T) {
	// #1 introduced ~240 lines' worth of identifiers and the model carried exactly one
	// value forward. "Took a value, does not need the rest" should therefore be visible
	// as a small UsedFrac rather than assumed.
	for _, r := range index(fixture(), testMinOutput, nil, true) {
		if r.ID != "t1" {
			continue
		}
		if r.Novel < 100 {
			t.Fatalf("t1 novel tokens = %d, want the filler identifiers to count", r.Novel)
		}
		if r.UsedFrac <= 0 || r.UsedFrac > 0.05 {
			t.Errorf("t1 UsedFrac = %.4f, want a small positive fraction", r.UsedFrac)
		}
	}
}

func TestClassifyBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  Record
		want Class
	}{
		{"no trackable identifiers is opaque, not unreferenced", Record{Novel: 0, Refs: 0, RefAge: -1}, Opaque},
		{"never referenced", Record{Novel: 20, Refs: 0, RefAge: -1, LaterTurns: 99}, Unreferenced},
		{"referenced exactly at the recency floor is closed", Record{Novel: 20, Refs: 1, RefAge: 12, LaterTurns: 99}, Closed},
		{"one message newer than the floor is open", Record{Novel: 20, Refs: 1, RefAge: 11, LaterTurns: 99}, Open},
		{"repetition keeps it open however old", Record{Novel: 20, Refs: 3, RefAge: 9999, LaterTurns: 99}, Open},
		{"just under the repetition ceiling, and old", Record{Novel: 20, Refs: 2, RefAge: 9999, LaterTurns: 99}, Closed},
	} {
		if got := Classify(tc.rec, testClosedDist, testOpenReps, testMinLater); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestDistinctiveRejectsProse(t *testing.T) {
	// Precision matters more than recall: a prose word scored as an identifier mints a
	// spurious reference, and a spurious reference silently suppresses a cut.
	//
	// Every entry in the second group below is a REGRESSION CASE — each was among the top
	// reference-producing "identifiers" on real agent traffic before the rules in
	// distinctive were tightened, and together they inflated referenced mass from 51% to 71%.
	for _, w := range []string{"the", "have", "should", "config", "failing", "module"} {
		if distinctive(w) {
			t.Errorf("distinctive(%q) = true, want false (prose)", w)
		}
	}
	for _, w := range []string{
		"description", "transparency", "integration", "efficiency", "conditions",
		"persistent", "orientation", "effectiveness", "environment", "conversation",
		"e.g.", "try:", "None:", "memory.", "2026", "447",
	} {
		if distinctive(w) {
			t.Errorf("distinctive(%q) = true, want false (measured false positive)", w)
		}
	}
	for _, w := range []string{
		"src/auth.py", "TOKEN_GRACE_SECONDS", "XPIRE_DRIFT_7f3a", "camelCaseName", "12345",
		"v1/messages", "session_id", "GraphStore", "config.py", "claude-sonnet-5", "1.2.3",
	} {
		if !distinctive(w) {
			t.Errorf("distinctive(%q) = false, want true (identifier)", w)
		}
	}
}

// A token's surrounding punctuation must not split it in two: `memory.` at the end of a
// sentence and `memory` in a tool argument have to match, or a real reference is missed.
func TestIdentsTrimEdgePunctuation(t *testing.T) {
	got := Idents("wrote src/auth.py. then read src/auth.py")
	if _, ok := got["src/auth.py"]; !ok {
		t.Errorf("Idents lost the trimmed form: %v", got)
	}
	if _, ok := got["src/auth.py."]; ok {
		t.Errorf("Idents kept an untrimmed duplicate: %v", got)
	}
}

func TestSiblingResultsDoNotCreditEachOther(t *testing.T) {
	// A batched turn carries several results at once. If one sibling's identifiers count
	// as a reference to another's, a parallel tool call makes both look load-bearing.
	body := filler("batch", 240)
	msgs := []Message{
		{Texts: []string{"go"}},
		{Results: []Result{{ID: "a", Text: body}, {ID: "b", Text: body}}},
		{Texts: []string{"done"}},
	}
	for _, r := range index(msgs, testMinOutput, nil, true) {
		if r.Refs != 0 {
			t.Errorf("output %s refs = %d, want 0 (its only overlap is its sibling)", r.ID, r.Refs)
		}
	}
}

func TestBoilerplateIsNotAReference(t *testing.T) {
	// A banner repeated by many outputs is furniture. Counting it makes the FIRST output
	// that emitted it look referenced by every later turn that echoes it.
	const banner = "=== build_harness_v2.1 /opt/ci/run.sh ==="
	var msgs []Message
	msgs = append(msgs, Message{Texts: []string{"start"}})
	for i := 0; i < 12; i++ {
		msgs = append(msgs,
			Message{Results: []Result{{ID: fmt.Sprintf("o%d", i), Text: banner + "\n" + filler(fmt.Sprintf("run%d", i), 240)}}},
			Message{Texts: []string{banner + " again"}},
		)
	}
	for _, r := range index(msgs, testMinOutput, nil, true) {
		if r.Refs != 0 {
			t.Errorf("output %s refs = %d, want 0 (its only later overlap is the banner)", r.ID, r.Refs)
		}
	}
}

func TestBelowFloorOutputsStillExclude(t *testing.T) {
	// A small output gets no Record, but the identifiers it introduced must still be in
	// the exclusion set: otherwise a large output re-emitting them looks like it
	// introduced them, and a later mention of them looks like a reference to it.
	const novel = "GRACE_WINDOW_88fa"
	msgs := []Message{
		{Texts: []string{"start"}},
		{Results: []Result{{ID: "small", Text: novel}}},
		{Results: []Result{{ID: "big", Text: novel + "\n" + filler("big", 240)}}},
		{Texts: []string{"using " + novel}},
	}
	for _, r := range index(msgs, testMinOutput, nil, true) {
		if r.ID != "big" {
			t.Fatalf("unexpected record for %q: the small output is below the floor", r.ID)
		}
		if r.Refs != 0 {
			t.Errorf("big refs = %d, want 0: %s was introduced by the earlier small output", r.Refs, novel)
		}
	}
}

func TestToolResultsAreNotReferenceBearing(t *testing.T) {
	// The environment repeating a token is not the model using it. Otherwise a flaky
	// command that prints the same error every turn keeps its own first output alive.
	msgs := []Message{
		{Texts: []string{"start"}},
		{Results: []Result{{ID: "first", Text: "E   AssertionError: DRIFT_9c2b\n" + filler("first", 240)}}},
		{Results: []Result{{ID: "second", Text: "E   AssertionError: DRIFT_9c2b\n" + filler("second", 240)}}},
	}
	for _, r := range index(msgs, testMinOutput, nil, true) {
		if r.ID == "first" && r.Refs != 0 {
			t.Errorf("first refs = %d, want 0 (only a later tool_result echoes it)", r.Refs)
		}
	}
}

func TestIndexHandlesEmptyAndNil(t *testing.T) {
	if recs := Index(nil, testMinOutput, nil); len(recs) != 0 {
		t.Errorf("Index(nil) = %v, want none", recs)
	}
	if recs := Index([]Message{{}, {Results: []Result{{ID: "x", Text: ""}}}}, 0, nil); len(recs) != 1 {
		t.Errorf("an empty output should still record (at the zero floor), got %v", recs)
	}
}

// An output whose values the tokenizer cannot see must come back `opaque`, never
// `unreferenced`. Both have refs == 0 and they mean opposite things: "introduced 200
// identifiers, nobody touched one" is evidence of deadness, "introduced nothing I can see"
// is absence of evidence. Collapsing them made the DEFAULT config cut a record set the
// agent had explicitly said it still needed.
//
// Raised in review on PR #80 with exactly this shape: the agent references an ANCHOR
// (`david`, `123`) in order to point at a payload (`foobarbaz`) it never copied.
func TestRecordsOfPlainValuesAreOpaqueNotUnreferenced(t *testing.T) {
	people := strings.Repeat(
		`[{"name":"david","id":123,"address":"foobarbaz"},{"name":"osher","id":235,"address":"banana"}]`, 60)
	msgs := []Message{
		{Texts: []string{"look up the people directory"}},
		{Texts: []string{"Querying.", "query_people {}"}},
		{Results: []Result{{ID: "t1", Text: people}}},
		{Texts: []string{"I need to remember david 123 address."}},
	}
	for i := 0; i < 20; i++ {
		msgs = append(msgs, Message{Texts: []string{fmt.Sprintf("unrelated step %d", i)}})
	}
	recs := index(msgs, testMinOutput, nil, true)
	if len(recs) != 1 {
		t.Fatalf("expected one record, got %d", len(recs))
	}
	if recs[0].Novel != 0 {
		t.Fatalf("fixture assumption broken: Novel = %d, expected the tokenizer to see nothing "+
			"in short lowercase words and 3-digit numbers", recs[0].Novel)
	}
	if got := Classify(recs[0], testClosedDist, testOpenReps, testMinLater); got != Opaque {
		t.Errorf("classified %q, want %q — an output the index cannot see into must not be "+
			"a silent vote to delete", got, Opaque)
	}
}

// An output near the tail has had no chance to be referenced, so scoring it as unused would
// make a batched pass preferentially cut the most RECENT context. This is why mask carries
// keep_recent, and coref needs the same idea expressed in turns.
func TestOpportunityFloorProtectsRecentOutputs(t *testing.T) {
	body := filler("recent", 240)
	msgs := []Message{
		{Texts: []string{"start"}},
		{Texts: []string{"Reading.", "Read {\"path\":\"x\"}"}},
		{Results: []Result{{ID: "fresh", Text: body}}},
		{Texts: []string{"ok"}}, // exactly one later model turn
	}
	recs := index(msgs, testMinOutput, nil, true)
	if len(recs) != 1 || recs[0].Novel == 0 {
		t.Fatalf("fixture assumption broken: %+v", recs)
	}
	if recs[0].LaterTurns != 1 {
		t.Errorf("LaterTurns = %d, want 1", recs[0].LaterTurns)
	}
	if got := Classify(recs[0], testClosedDist, testOpenReps, 0); got != Unreferenced {
		t.Errorf("with the floor disabled: got %q, want %q", got, Unreferenced)
	}
	if got := Classify(recs[0], testClosedDist, testOpenReps, 8); got != Open {
		t.Errorf("with the floor at 8: got %q, want %q — one later turn is not an "+
			"opportunity to be referenced", got, Open)
	}
}
