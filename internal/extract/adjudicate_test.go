package extract

import (
	"strings"
	"testing"
)

// The four safety invariants of the cold-sweep adjudicator, in the order the spec ranks them by
// danger. Each is written to FAIL when its subject is reverted; the reverted-output is quoted in the
// commit message.
//
// Every test asserts a PRECONDITION first — that the reply reached the check under test at all.
// Without it a parse regression turns each of these into a vacuous pass: Judge would return the zero
// Adjudication, Drop would be false, and "the refusal worked" and "the reply was never read" would be
// indistinguishable. That is the exact failure mode these tests exist to rule out.

const testTranscript = `user: find the flaky test and fix it
assistant: I will run the suite, then patch the failing case.
tool: 400 lines of test output
assistant: TestAuthExpiry is the flaky one. Next I will patch auth/session.go.`

// INVARIANT 1. A drop that names an outstanding obligation is refused, not performed. This is the one
// verification pointing the dangerous way: the model has just said the output is still needed, and
// then asked for it to be removed anyway.
func TestDropNamingAnObligationIsRefused(t *testing.T) {
	for _, nb := range []string{"a", "b", "c", "A", " b ", "in-progress-step"} {
		reply := `{"needed_by":"` + nb + `","quote":"Next I will patch auth/session.go.","verdict":"drop"}`
		a := Judge(reply, testTranscript)
		// PRECONDITION: the reply parsed and the verdict reached the drop branch. If this fails the
		// assertion below proves nothing — an unparsed reply is also not a drop.
		if !a.Parsed {
			t.Fatalf("needed_by=%q: reply did not parse, so the refusal was never exercised", nb)
		}
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
	a := Judge(`{"needed_by":"none","quote":"","verdict":"drop"}`, testTranscript)
	if !a.Parsed {
		t.Fatalf("reply did not parse")
	}
	if !a.Drop {
		t.Fatalf("a well-formed spent verdict was not performed; the component would never act")
	}
	if a.RefusedObligation || a.QuoteFabricated || a.CriterionMissing || a.VerdictUnusable {
		t.Errorf("clean verdict raised a failure counter: %+v", a)
	}
}

// INVARIANT 2. Unsure defaults to keep. A missing, malformed or unparseable verdict leaves the output
// verbatim.
func TestUnsureDefaultsToKeep(t *testing.T) {
	cases := []struct {
		name       string
		reply      string
		wantParsed bool
	}{
		{"empty reply", "", false},
		{"prose, no object", "I think this output is probably spent, you can remove it.", false},
		{"truncated object", `{"needed_by":"none","verdict":"dr`, false},
		{"not json inside braces", `{needed_by: none, verdict: drop}`, false},
		{"verdict absent", `{"needed_by":"none","quote":""}`, true},
		{"verdict empty", `{"needed_by":"none","verdict":""}`, true},
		{"verdict is trim", `{"needed_by":"none","verdict":"trim"}`, true},
		{"verdict is prose", `{"needed_by":"none","verdict":"probably drop it"}`, true},
	}
	for _, tc := range cases {
		a := Judge(tc.reply, testTranscript)
		if a.Drop {
			t.Errorf("%s: dropped on an unusable verdict; unsure must default to keep", tc.name)
		}
		// PRECONDITION, and it is what makes this test non-vacuous in BOTH directions: a reply that
		// carries a usable object must be reported as parsed, and one that does not must not. Without
		// this, a Judge that returned the zero value for everything would pass every Drop assertion
		// above while having stopped reading replies at all.
		if a.Parsed != tc.wantParsed {
			t.Errorf("%s: Parsed=%v, want %v", tc.name, a.Parsed, tc.wantParsed)
		}
		if tc.wantParsed && !a.VerdictUnusable {
			t.Errorf("%s: an unusable verdict was not counted", tc.name)
		}
	}
}

// INVARIANT 3. A fabricated obligation quote is counted. It argues for KEEPING so it is not
// dangerous — but it is the signal that the model is inventing, and on this design it is the only
// such signal left, since nothing else it returns is content.
func TestFabricatedObligationQuoteIsCounted(t *testing.T) {
	a := Judge(`{"needed_by":"c","quote":"Next I will rewrite the parser in Rust.","verdict":"keep"}`,
		testTranscript)
	if !a.Parsed {
		t.Fatalf("reply did not parse, so the quote check was never exercised")
	}
	if !a.QuoteFabricated {
		t.Fatalf("a quote absent from the transcript was not counted as fabricated")
	}
	// And a real quote must NOT be counted, or the signal is noise. Both directions, because a check
	// that fires on everything is as useless as one that fires on nothing.
	b := Judge(`{"needed_by":"c","quote":"Next I will patch auth/session.go.","verdict":"keep"}`,
		testTranscript)
	if b.QuoteFabricated {
		t.Errorf("a verbatim transcript quote was counted as fabricated")
	}
	// A re-wrapped quote is faithful copying, not invention. Counting it would train the operator to
	// ignore an alertable counter.
	c := Judge(`{"needed_by":"b","quote":"I will run the suite,\n  then patch the failing case.","verdict":"keep"}`,
		testTranscript)
	if c.QuoteFabricated {
		t.Errorf("a re-wrapped but faithful quote was counted as fabricated")
	}
}

// INVARIANT 4. An unanswered criterion field is tolerated and counted. Requiring it would collapse
// yield against a model that omits it; ignoring it would hide that the forcing function never ran.
func TestUnansweredCriterionIsToleratedAndCounted(t *testing.T) {
	a := Judge(`{"verdict":"drop"}`, testTranscript)
	if !a.Parsed {
		t.Fatalf("reply did not parse, so the criterion check was never exercised")
	}
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
	if b := Judge(`{"needed_by":"none","verdict":"drop"}`, testTranscript); b.CriterionMissing {
		t.Errorf("an answered criterion was counted as missing")
	}
}

// The contract itself must never invite the model to return content. This is the property the whole
// design rests on: no transporting verdict is offered, and no reply field can carry output text.
func TestContractOffersNoTransportingVerdict(t *testing.T) {
	item := AdjudicationItem{Index: 3, ID: "toolu_1", SizeTokens: 4200, Content: "line one\nline two\n"}
	p := BuildAdjudicationPrompt("fix the flaky test", item)
	if !strings.Contains(p, "keep|drop") {
		t.Fatalf("prompt does not carry the binary verdict contract")
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
	if !strings.Contains(p, "line one") {
		t.Errorf("prompt does not show the output it is asking about")
	}
}

// A large output is shown as a MARKED excerpt, so the model does not reason as though it had seen the
// end of something it has not.
func TestLargeOutputExcerptIsMarked(t *testing.T) {
	body := strings.Repeat("x", adjudicationSampleChars+500)
	p := BuildAdjudicationPrompt("goal", AdjudicationItem{Content: body})
	if strings.Contains(p, body) {
		t.Fatalf("the whole oversized body was shipped; the bound did not apply")
	}
	if !strings.Contains(p, "excerpt truncated") {
		t.Errorf("the cut is not marked, so the model cannot tell it is judging an excerpt")
	}
}
