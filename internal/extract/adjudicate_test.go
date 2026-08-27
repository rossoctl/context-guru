package extract

import (
	"strings"
	"testing"
)

// The four safety invariants of the sweep adjudicator, in the order the spec ranks them by danger.
// Each is written to FAIL when its subject is reverted; the reverted output is quoted in the commit
// message.
//
// They are orthogonal to how the question is DELIVERED. These rules applied when the prompt carried
// copied output content, they applied at batch 12, and they apply now that the question is a prefix ask
// over the cached transcript: they operate on one VERDICT, and a verdict has the same shape whatever
// the model was reading when it wrote one.
//
// Every test asserts a PRECONDITION first — that the reply parsed, and that the verdict reached the
// check under test. Without it a parse regression turns each of these into a vacuous pass: Judge would
// return the zero Adjudication, Drop would be false, and "the refusal worked" and "the reply was never
// read" would be indistinguishable.

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

// The other side of invariant 1, so it cannot be satisfied by refusing everything: needed_by "none" is
// the one answer a drop is allowed to carry, and it must go through.
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

// THE DISTINCTION 4ca1f13 ADDED. An EMPTY array is the model saying "keep everything", which the
// contract explicitly invites. A reply that did not parse is a model or prompt failure. Folding them
// together makes "the model declined to act" and "the model was never successfully asked" the same
// number.
func TestAnEmptyArrayIsADeliberateKeepAllNotAFailure(t *testing.T) {
	vs, ok := ParseVerdicts("[]")
	if !ok {
		t.Fatal("an empty array is a well-formed answer and must report as parsed")
	}
	if len(vs) != 0 {
		t.Fatalf("expected no verdicts, got %d", len(vs))
	}
	if _, ok := ParseVerdicts("no."); ok {
		t.Fatal("junk reported as parsed, so keep-all and failure are indistinguishable")
	}
}

// A TRUNCATED reply is not a malformed one, and they need opposite fixes — raise the output budget
// versus fix the prompt. 659e7a6 found 24 of 34 unparseable replies were truncation at a 2048-token
// budget, misread for three iterations because one name covered both. It matters MORE now: one reply
// carries a verdict for every candidate, not twelve.
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

// INVARIANT 3. A fabricated obligation quote is counted. It argues for KEEPING so it is not dangerous
// — but it is the signal that the model is inventing, and on this design it is the only such signal
// left, since nothing else it returns is content. It is also the measurement that decided WHICH model
// is asked: verbatim quoting degraded to 20.8% on the cheap model against 0 of 59 on the request one.
func TestFabricatedObligationQuoteIsCounted(t *testing.T) {
	a := Judge(one(t, `[{"i":1,"needed_by":"c","quote":"Next I will rewrite the parser in Rust.","verdict":"keep"}]`),
		testTranscript)
	if !a.QuoteFabricated {
		t.Fatalf("a quote absent from the transcript was not counted as fabricated")
	}
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
	if b := Judge(one(t, `[{"i":1,"needed_by":"none","verdict":"drop"}]`), testTranscript); b.CriterionMissing {
		t.Errorf("an answered criterion was counted as missing")
	}
}

// Mixed verdicts in one reply are each decided on their own merits, keyed by their own label. This is
// the shape 4ca1f13 found missing when a "bulk" arm was answering 1.02 verdicts per call.
func TestAReplyIsJudgedPerLabel(t *testing.T) {
	reply := `[
	  {"i":0,"needed_by":"none","quote":"","verdict":"drop"},
	  {"i":1,"needed_by":"c","quote":"Next I will patch auth/session.go.","verdict":"keep"},
	  {"i":2,"needed_by":"a","quote":"Next I will patch auth/session.go.","verdict":"drop"},
	  {"i":3,"needed_by":"none","verdict":"trim"}
	]`
	vs, ok := ParseVerdicts(reply)
	if !ok {
		t.Fatal("a well-formed multi-verdict reply did not parse")
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
			t.Errorf("verdict %d carries label %d, want %d — the label mapping is what keys a "+
				"decision to an output", i, vs[i].Label, w.label)
		}
		a := Judge(vs[i], testTranscript)
		if a.Drop != w.drop || a.RefusedObligation != w.refused || a.VerdictUnusable != w.unusabl {
			t.Errorf("label %d: got %+v, want drop=%v refused=%v unusable=%v",
				w.label, a, w.drop, w.refused, w.unusabl)
		}
	}
}

// THE INVENTORY SHIPS NO OUTPUT CONTENT. This is the property the whole prefix-ask design rests on:
// the model reads the outputs in full from its own prompt cache, and paying fresh to send truncated
// copies of them would both defeat the mechanism and show it an excerpt of something it could read
// whole.
func TestThePrefixAskShipsAnInventoryAndNotTheOutputs(t *testing.T) {
	body := "SECRET-PAYLOAD-ostrich line one\nSECRET-PAYLOAD-ostrich line two\n" +
		strings.Repeat("more payload that must not travel\n", 50)
	items := []AdjudicationItem{
		{Label: 0, ID: "toolu_abc123", SizeTokens: 4200, Head: HeadLine(body, AdjudicationHeadChars)},
		{Label: 1, ID: "toolu_def456", SizeTokens: 900, Head: HeadLine("second output starts here\nand goes on", AdjudicationHeadChars)},
	}
	ask := BuildPrefixAsk(items)

	// PRECONDITION: the ask is a real ask. If the contract or the labels were missing, the leak
	// assertions below would pass against an empty string.
	for _, want := range []string{"keep|drop", `"i": <label>`, "SPENT only if",
		"JUDGE THEM AGAINST EACH OTHER", "[0]", "[1]"} {
		if !strings.Contains(ask, want) {
			t.Fatalf("the ask does not carry %q", want)
		}
	}
	// The body must NOT be in it. Only the bounded head line may appear, which is there to locate the
	// output in the transcript above rather than to inform the judgement.
	if strings.Contains(ask, "more payload that must not travel") {
		t.Error("output content past the head line reached the ask; it is being paid for twice")
	}
	if n := strings.Count(ask, "SECRET-PAYLOAD-ostrich"); n != 1 {
		t.Errorf("the payload appears %d times in the ask; only the single head line may carry it", n)
	}
	if strings.Contains(ask, "line two") {
		t.Error("the head line spans more than one line of the output")
	}
	// The ask must be small: it is the only part paid fresh, so its size is the mechanism's whole cost.
	if len(ask) > len(adjudicationContract)+500 {
		t.Errorf("the inventory added %d chars over the contract; it should be one line per candidate",
			len(ask)-len(adjudicationContract))
	}
	// The opaque tool_use ids stay on OUR side: a random identifier in front of the model is a string
	// it may echo instead of the integer label, and the labels are integers for exactly that reason.
	for _, it := range items {
		if strings.Contains(ask, it.ID) {
			t.Errorf("tool_use id %q reached the ask", it.ID)
		}
	}
	// And it must tell the model to read from the conversation rather than from the ask.
	if !strings.Contains(ask, "conversation above is your own") {
		t.Error("the ask does not point the model at the cached transcript")
	}
}

// The contract must never invite the model to transport text, and must never tell it that a mistake is
// cheap. Measured: reassuring the model that removals stay recoverable produced 91% removal at 6%
// live-kept.
func TestTheContractOffersNoTransportAndPromisesNoSafetyNet(t *testing.T) {
	ask := BuildPrefixAsk([]AdjudicationItem{{Label: 0, SizeTokens: 10, Head: "x"}})
	low := strings.ToLower(ask)
	for _, banned := range []string{"trim", "rewrite", "summarize", "shorten"} {
		if strings.Contains(low, banned) {
			t.Errorf("the contract invites the model to transport text: mentions %q", banned)
		}
	}
	for _, banned := range []string{"recoverab", "restore", "you can get it back"} {
		if strings.Contains(low, banned) {
			t.Errorf("the contract tells the model its mistakes are cheap: mentions %q", banned)
		}
	}
}

// The head line is bounded and single-line, whatever the output looks like.
func TestHeadLineIsBoundedAndSingleLine(t *testing.T) {
	for _, tc := range []string{
		"short\n",
		strings.Repeat("x", 500),
		"\n\n\nleading blank lines then content\nand more\n",
		"a\rb\rc",
	} {
		got := HeadLine(tc, AdjudicationHeadChars)
		if strings.ContainsAny(got, "\r\n") {
			t.Errorf("head line contains a newline: %q", got)
		}
		// The ellipsis is multi-byte, so bound on runes rather than bytes.
		if n := len([]rune(got)); n > AdjudicationHeadChars+1 {
			t.Errorf("head line is %d runes, over the %d bound: %q", n, AdjudicationHeadChars, got)
		}
	}
}
