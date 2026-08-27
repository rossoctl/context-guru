package extract

import (
	"strconv"
	"strings"
	"testing"
)

// The four safety invariants of the cold-sweep adjudicator, in the order the spec ranks them by
// danger. Each is written to FAIL when its subject is reverted; the reverted-output is quoted in the
// commit message.
//
// They are orthogonal to batch size: the rules apply per VERDICT, whatever the batch was. What the
// move to batched adjudication changed is only where a reply becomes verdicts — ParseVerdicts — so
// invariant 2 is now checked on both halves, the array parser and the per-verdict judgement.
//
// Every test asserts a PRECONDITION first — that the reply parsed, and that the verdict reached the
// check under test. Without it a parse regression turns each of these into a vacuous pass: Judge
// would return the zero Adjudication, Drop would be false, and "the refusal worked" and "the reply
// was never read" would be indistinguishable.

const testTranscript = `user: find the flaky test and fix it
assistant: I will run the suite, then patch the failing case.
tool: 400 lines of test output
assistant: TestAuthExpiry is the flaky one. Next I will patch auth/session.go.`

// one parses a single-verdict reply and fails loudly if the array did not parse, so every test below
// asserts against a verdict that genuinely came off the wire.
func one(t *testing.T, reply string) Verdict {
	t.Helper()
	vs, ok := ParseVerdicts(reply)
	if !ok {
		t.Fatalf("reply did not parse, so nothing under test was exercised: %q", reply)
	}
	if len(vs) != 1 {
		t.Fatalf("expected exactly one verdict, got %d from %q", len(vs), reply)
	}
	return vs[0]
}

// INVARIANT 1. A drop that names an outstanding obligation is refused, not performed. This is the one
// verification pointing the dangerous way: the model has just said the output is still needed, and
// then asked for it to be removed anyway.
func TestDropNamingAnObligationIsRefused(t *testing.T) {
	for _, nb := range []string{"a", "b", "c", "A", " b ", "in-progress-step"} {
		v := one(t, `[{"i":3,"needed_by":"`+nb+`","quote":"Next I will patch auth/session.go.","verdict":"drop"}]`)
		a := Judge(v, testTranscript)
		// PRECONDITION: the verdict reached the drop branch. If this fails the assertion below
		// proves nothing — an unread verdict is also not a drop.
		if a.VerdictUnusable {
			t.Fatalf("needed_by=%q: verdict was not read as a drop, so the refusal was never exercised", nb)
		}
		if a.Drop {
			t.Errorf("needed_by=%q: dropped an output the model itself said is still needed", nb)
		}
		if !a.RefusedObligation {
			t.Errorf("needed_by=%q: refusal not counted; the alertable signal would be silent", nb)
		}
	}
}

// The other side of invariant 1, so it cannot be satisfied by refusing everything: needed_by "none"
// is the one answer a drop is allowed to carry, and it must go through.
func TestDropWithNoObligationIsPerformed(t *testing.T) {
	a := Judge(one(t, `[{"i":0,"needed_by":"none","quote":"","verdict":"drop"}]`), testTranscript)
	if !a.Drop {
		t.Fatalf("a well-formed spent verdict was not performed; the component would never act")
	}
	if a.RefusedObligation || a.QuoteFabricated || a.CriterionMissing || a.VerdictUnusable {
		t.Errorf("clean verdict raised a failure counter: %+v", a)
	}
}

// INVARIANT 2, first half: a reply that is not a usable JSON ARRAY yields no verdicts and is reported
// as a parse failure, so the caller keeps everything.
func TestAnUnparseableReplyYieldsNoVerdicts(t *testing.T) {
	for _, tc := range []struct{ name, reply string }{
		{"empty reply", ""},
		{"prose, no array", "I think outputs 2 and 5 are probably spent, you can remove them."},
		{"truncated array", `[{"i":1,"needed_by":"none","verdict":"dr`},
		{"not json inside brackets", `[{i: 1, needed_by: none, verdict: drop}]`},
		{"a bare object, not an array", `{"i":1,"needed_by":"none","verdict":"drop"}`},
	} {
		vs, ok := ParseVerdicts(tc.reply)
		if ok {
			t.Errorf("%s: reported as parsed, so junk would be acted on", tc.name)
		}
		if len(vs) != 0 {
			t.Errorf("%s: returned %d verdicts from an unparseable reply", tc.name, len(vs))
		}
	}
}

// INVARIANT 2, second half: a verdict that parsed but is missing, empty or unactionable leaves the
// output verbatim and is counted.
func TestUnsureDefaultsToKeep(t *testing.T) {
	for _, tc := range []struct{ name, reply string }{
		{"verdict absent", `[{"i":1,"needed_by":"none","quote":""}]`},
		{"verdict empty", `[{"i":1,"needed_by":"none","verdict":""}]`},
		{"verdict is trim", `[{"i":1,"needed_by":"none","verdict":"trim"}]`},
		{"verdict is prose", `[{"i":1,"needed_by":"none","verdict":"probably drop it"}]`},
	} {
		a := Judge(one(t, tc.reply), testTranscript)
		if a.Drop {
			t.Errorf("%s: dropped on an unusable verdict; unsure must default to keep", tc.name)
		}
		if !a.VerdictUnusable {
			t.Errorf("%s: an unusable verdict was not counted", tc.name)
		}
	}
}

// THE DISTINCTION `4ca1f13` ADDED, and it is the one the whole batch shape's diagnosis rests on. An
// EMPTY array is the model saying "keep everything", which the contract explicitly invites. A reply
// that did not parse is a model or prompt failure. Folding them together makes "the model declined to
// act" and "the model was never successfully asked" the same number.
func TestAnEmptyArrayIsADeliberateKeepAllNotAFailure(t *testing.T) {
	vs, ok := ParseVerdicts("[]")
	if !ok {
		t.Fatal("an empty array is a well-formed answer and must report as parsed")
	}
	if len(vs) != 0 {
		t.Fatalf("expected no verdicts, got %d", len(vs))
	}
	// And the two must not be conflatable: an unparseable reply reports the opposite.
	if _, ok := ParseVerdicts("no."); ok {
		t.Fatal("junk reported as parsed, so keep-all and failure are indistinguishable")
	}
}

// A TRUNCATED reply is not a malformed one, and they need opposite fixes — raise the output budget
// versus fix the prompt. `659e7a6` found 24 of 34 unparseable replies were truncation at a 2048-token
// budget, misread for three iterations because one name covered both.
func TestTruncationIsDistinguishedFromAFormatFailure(t *testing.T) {
	truncated := `[{"i":1,"needed_by":"none","verdict":"drop"},{"i":2,"needed_by":"a","quote":"I will run the su`
	if _, ok := ParseVerdicts(truncated); ok {
		t.Fatal("a truncated array must not parse")
	}
	if !ReplyWasTruncated(truncated) {
		t.Error("a reply that opened the array and never closed it must read as truncated")
	}
	if ReplyWasTruncated("I decline to answer.") {
		t.Error("a reply that never opened an array is a format failure, not truncation")
	}
}

// INVARIANT 3. A fabricated obligation quote is counted. It argues for KEEPING so it is not
// dangerous — but it is the signal that the model is inventing, and on this design it is the only
// such signal left, since nothing else it returns is content.
func TestFabricatedObligationQuoteIsCounted(t *testing.T) {
	a := Judge(one(t, `[{"i":1,"needed_by":"c","quote":"Next I will rewrite the parser in Rust.","verdict":"keep"}]`),
		testTranscript)
	if !a.QuoteFabricated {
		t.Fatalf("a quote absent from the transcript was not counted as fabricated")
	}
	// And a real quote must NOT be counted, or the signal is noise. Both directions, because a check
	// that fires on everything is as useless as one that fires on nothing.
	b := Judge(one(t, `[{"i":1,"needed_by":"c","quote":"Next I will patch auth/session.go.","verdict":"keep"}]`),
		testTranscript)
	if b.QuoteFabricated {
		t.Errorf("a verbatim transcript quote was counted as fabricated")
	}
	// A re-wrapped quote is faithful copying, not invention. Counting it would train the operator to
	// ignore an alertable counter.
	c := Judge(one(t, `[{"i":1,"needed_by":"b","quote":"I will run the suite,\n  then patch the failing case.","verdict":"keep"}]`),
		testTranscript)
	if c.QuoteFabricated {
		t.Errorf("a re-wrapped but faithful quote was counted as fabricated")
	}
}

// INVARIANT 4. An unanswered criterion field is tolerated and counted. Requiring it would collapse
// yield against a model that omits it; ignoring it would hide that the forcing function never ran.
func TestUnansweredCriterionIsToleratedAndCounted(t *testing.T) {
	a := Judge(one(t, `[{"i":1,"verdict":"drop"}]`), testTranscript)
	if a.VerdictUnusable {
		t.Fatalf("the verdict was not read, so this exercises invariant 2 rather than invariant 4")
	}
	if !a.Drop {
		t.Errorf("an unanswered criterion refused the drop; yield would collapse against a model that omits it")
	}
	if !a.CriterionMissing {
		t.Errorf("an unanswered criterion was not counted; the forcing function's absence would be invisible")
	}
	// Answered, so not counted — otherwise the counter says nothing.
	if b := Judge(one(t, `[{"i":1,"needed_by":"none","verdict":"drop"}]`), testTranscript); b.CriterionMissing {
		t.Errorf("an answered criterion was counted as missing")
	}
}

// A BATCH IS JUDGED AS A BATCH: every verdict is returned, keyed by its own label, and mixed verdicts
// in one reply are each decided on their own merits. This is the shape `4ca1f13` found missing when a
// "bulk" arm was answering 1.02 verdicts per call.
func TestABatchReplyIsParsedPerLabel(t *testing.T) {
	reply := `[
	  {"i":0,"needed_by":"none","quote":"","verdict":"drop"},
	  {"i":1,"needed_by":"c","quote":"Next I will patch auth/session.go.","verdict":"keep"},
	  {"i":2,"needed_by":"a","quote":"Next I will patch auth/session.go.","verdict":"drop"},
	  {"i":3,"needed_by":"none","verdict":"trim"}
	]`
	vs, ok := ParseVerdicts(reply)
	if !ok {
		t.Fatal("a well-formed batch reply did not parse")
	}
	if len(vs) != 4 {
		t.Fatalf("expected 4 verdicts, got %d", len(vs))
	}
	want := []struct {
		label   int
		drop    bool
		refused bool
		unusabl bool
	}{
		{0, true, false, false},  // spent
		{1, false, false, false}, // still needed
		{2, false, true, false},  // a drop contradicting its own obligation — refused
		{3, false, false, true},  // trim, which we do not perform — degraded to keep, counted
	}
	for i, w := range want {
		if vs[i].Label != w.label {
			t.Errorf("verdict %d carries label %d, want %d — the label mapping is what keys "+
				"a decision to an output", i, vs[i].Label, w.label)
		}
		a := Judge(vs[i], testTranscript)
		if a.Drop != w.drop || a.RefusedObligation != w.refused || a.VerdictUnusable != w.unusabl {
			t.Errorf("label %d: got %+v, want drop=%v refused=%v unusable=%v",
				w.label, a, w.drop, w.refused, w.unusabl)
		}
	}
}

// The contract must invite COMPARATIVE judgement, which is the measured difference between 6% and 58%
// live-kept, and must never invite the model to return content.
func TestContractAsksForComparativeJudgementAndNoTransport(t *testing.T) {
	items := []AdjudicationItem{
		{Label: 0, ID: "toolu_1", SizeTokens: 4200, Content: "alpha line\nbeta line\n"},
		{Label: 1, ID: "toolu_2", SizeTokens: 900, Content: "gamma line\n"},
	}
	p := BuildAdjudicationPrompt("fix the flaky test", items)
	for _, want := range []string{
		"keep|drop",
		"JUDGE THEM AGAINST EACH OTHER", // absent, this is the refuted per-output shape
		`"i": <label>`,                  // the array contract, one object per output
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt does not carry %q", want)
		}
	}
	for _, banned := range []string{"trim", "rewrite", "summarize", "shorten"} {
		if strings.Contains(strings.ToLower(p), banned) {
			t.Errorf("prompt invites the model to transport text: mentions %q", banned)
		}
	}
	// It must also NOT mention recoverability. Measured: reassuring the model that removals stay
	// recoverable produced 91% removal at 6% live-kept; the cost-honest framing is worth ~26 points
	// of live-kept on its own.
	for _, banned := range []string{"recoverab", "restore", "you can get it back"} {
		if strings.Contains(strings.ToLower(p), banned) {
			t.Errorf("prompt tells the model its mistakes are cheap: mentions %q", banned)
		}
	}
	// Every offered output must be SHOWN and LABELLED, or the model cannot answer per label.
	for _, it := range items {
		if !strings.Contains(p, "=== OUTPUT "+strconv.Itoa(it.Label)) {
			t.Errorf("output %d is not labelled in the prompt", it.Label)
		}
	}
	if !strings.Contains(p, "alpha line") || !strings.Contains(p, "gamma line") {
		t.Error("the prompt does not show every output it is asking about")
	}
	// The opaque tool-call id must NOT reach the model: asked to echo those back it regularised
	// them, because reproducing a random identifier is a copying task rather than a judgement.
	if strings.Contains(p, "toolu_1") || strings.Contains(p, "toolu_2") {
		t.Error("a tool_use id reached the prompt; labels are integers precisely so it cannot")
	}
}

// A large output is shown as a MARKED excerpt, so the model does not reason as though it had seen the
// end of something it has not.
func TestLargeOutputExcerptIsMarked(t *testing.T) {
	body := strings.Repeat("x", AdjudicationSampleChars+500)
	p := BuildAdjudicationPrompt("goal", []AdjudicationItem{{Label: 0, Content: body}})
	if strings.Contains(p, body) {
		t.Fatalf("the whole oversized body was shipped; the bound did not apply")
	}
	if !strings.Contains(p, "excerpt truncated") {
		t.Errorf("the cut is not marked, so the model cannot tell it is judging an excerpt")
	}
}

// The batch cap is a MEASURED ceiling, not a round number: 4 of 37 quotes came back non-verbatim at
// batch 16 against 0 of 16 at batch 10, so the transport limit sits between them. Pinned so a later
// "let's offer more" cannot quietly cross it.
func TestBatchCapStaysBelowTheMeasuredTransportCeiling(t *testing.T) {
	if MaxAdjudicationItems > 12 {
		t.Errorf("MaxAdjudicationItems = %d, above the measured quote-fidelity ceiling of 12",
			MaxAdjudicationItems)
	}
	if MaxAdjudicationItems < 10 {
		t.Errorf("MaxAdjudicationItems = %d, below the batch size at which the model was measured "+
			"willing to act at all (10); small batches make it unwilling, not wrong",
			MaxAdjudicationItems)
	}
}
