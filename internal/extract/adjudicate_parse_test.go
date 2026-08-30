package extract

import "testing"

// A REPLY WITH PROSE AROUND THE JSON MUST STILL PARSE.
//
// Reported from live verification against the real gateway on aws/claude-sonnet-5: three firings,
// three failures to produce a usable verdict, one of them a 7,191-completion-token reply at 71.2s
// that was NOT truncated. The parser took the first `[` to the last `]`, so any bracket anywhere in
// the model's reasoning made the span unparseable — and a model asked to justify twelve verdicts
// writes reasoning. Net effect: zero effective compactions on real traffic, reported as
// `sweep_unparseable`, which reads as "the prompt is wrong".
func TestParseVerdictsSurvivesProseAroundTheArray(t *testing.T) {
	const arr = `[{"i":0,"needed_by":"none","quote":"","verdict":"drop"},` +
		`{"i":1,"needed_by":"b","quote":"still needed","verdict":"keep"}]`

	for _, tc := range []struct{ name, reply string }{
		{"bare array", arr},
		{"prose before", "Let me work through each one.\n\n" + arr},
		{"prose after", arr + "\n\nThat covers all twelve."},
		{"prose both sides", "Reasoning:\n" + arr + "\nDone."},
		// The shape that actually broke it: a bracket in the prose BEFORE the array, so the old
		// first-to-last span started inside reasoning text.
		{"bracketed citation before", "Per criterion [a] the first is spent.\n" + arr},
		{"bracketed list before", "Candidates: [0, 1] considered.\n\n" + arr},
		{"markdown fence", "```json\n" + arr + "\n```"},
		{"fence plus prose", "Here is my answer:\n```json\n" + arr + "\n```\nHope that helps."},
		// A trailing bracket after the array, which the old LastIndex would have grabbed.
		{"bracket after", arr + "\n\nNote: see criterion [b] above."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, ok := ParseVerdicts(tc.reply)
			if !ok {
				t.Fatalf("did not parse; a real reply of this shape produced zero compactions live")
			}
			if len(out) != 2 {
				t.Fatalf("expected 2 verdicts, got %d: %+v", len(out), out)
			}
			if out[0].Verdict != "drop" || out[1].Verdict != "keep" || out[1].NeededBy != "b" {
				t.Errorf("verdicts decoded wrong: %+v", out)
			}
		})
	}
}

// And the converse: a decoded array is not automatically THE array. `[{}]` decodes into []Verdict
// cleanly and would give a phantom verdict for label 0 — which is a REAL candidate, so acting on it
// would remove the wrong output. Requiring an actual verdict field is what separates the two.
func TestParseVerdictsRejectsArraysThatAreNotVerdicts(t *testing.T) {
	for _, tc := range []struct{ name, reply string }{
		{"empty objects", `[{}]`},
		{"unrelated objects", `[{"note":"thinking"},{"note":"more"}]`},
		{"numbers", `[1, 2, 3]`},
		{"strings", `["keep","drop"]`},
		{"no array at all", "I could not decide."},
		{"prose with brackets only", "criterion [a] and [b] apply here"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if out, ok := ParseVerdicts(tc.reply); ok {
				t.Errorf("accepted a non-verdict array as verdicts: %+v — a phantom verdict for "+
					"label 0 would act on a real candidate", out)
			}
		})
	}
}

// A REAL VERDICT MISSING ITS `verdict` FIELD MUST STILL PARSE, so the caller can classify it.
//
// This is the boundary the phantom-rejection guard has to respect, and a stricter version of it broke
// TestUnsureDefaultsToKeep: a model that answered badly and a reply that could not be read are
// different failures with different remedies, and they are separately counted downstream for exactly
// that reason. Refusing to parse this would file the first as the second.
func TestParseVerdictsAcceptsAMalformedButRealVerdict(t *testing.T) {
	for _, tc := range []struct{ name, reply string }{
		{"verdict field omitted", `[{"i":1,"needed_by":"none","quote":""}]`},
		{"only a criterion", `[{"needed_by":"b"}]`},
		{"only a quote", `[{"quote":"still needed later"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := ParseVerdicts(tc.reply); !ok {
				t.Errorf("a real verdict object missing a field must PARSE so the caller can count "+
					"it as unusable and default to keep: %s", tc.reply)
			}
		})
	}
}

// An EMPTY array stays a legitimate answer: the contract invites keep-everything, and folding that
// into "unparseable" is what made "the model declined to act" and "the model was never successfully
// asked" the same number for three iterations (4ca1f13).
func TestParseVerdictsKeepsEmptyArrayDistinctFromJunk(t *testing.T) {
	out, ok := ParseVerdicts("Everything is still needed.\n[]")
	if !ok {
		t.Fatal("a well-formed empty array is a deliberate keep-all, not a parse failure")
	}
	if len(out) != 0 {
		t.Errorf("expected no verdicts, got %+v", out)
	}
	// And it must not be mistaken for truncation, whose remedy is the opposite (raise the budget).
	if ReplyWasTruncated("[]") {
		t.Error("a closed empty array is not truncated")
	}
}
