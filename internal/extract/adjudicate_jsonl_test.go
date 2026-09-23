package extract

import "testing"

// THE REPLY THIS EXISTS FOR, captured verbatim from aws/claude-sonnet-5 on a real prefix ask whose four
// verdicts were individually well-formed, correctly reasoned and complete for the batch. It carries no
// bracket, so the array scan discarded the entire ask and the money spent on it.
const jsonlReply = `{"i": 0, "needed_by": "none", "quote": "", "verdict": "drop"}
{"i": 1, "needed_by": "none", "quote": "", "verdict": "drop"}
{"i": 2, "needed_by": "none", "quote": "", "verdict": "drop"}
{"i": 3, "needed_by": "a", "quote": "not_submitted.json", "verdict": "keep"}`

func TestNewlineDelimitedVerdictsParse(t *testing.T) {
	vs, ok := ParseVerdicts(jsonlReply)
	if !ok {
		t.Fatal("a complete newline-delimited reply must parse; discarding it wastes the whole ask")
	}
	if len(vs) != 4 {
		t.Fatalf("verdicts = %d, want 4", len(vs))
	}
	if vs[3].Label != 3 || vs[3].NeededBy != "a" || vs[3].Verdict != "keep" {
		t.Errorf("last verdict decoded wrong: %+v", vs[3])
	}
	// It must NOT be reported as truncated: the remedy for truncation is a bigger budget, and this
	// reply was complete.
	if ReplyWasTruncated(jsonlReply) {
		t.Error("a complete newline-delimited reply must not read as truncated")
	}
}

// The fallback must not become a hole in the checks the array path applies.
func TestNewlineFallbackKeepsTheArrayPathsGuards(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply string
	}{
		{"empty objects are phantom verdicts, not verdicts", "{}\n{}"},
		{"objects with no verdict field carry no information", `{"note":"x"}` + "\n" + `{"note":"y"}`},
		{"prose is not a verdict list", "I will not answer that."},
		{"a half-written object is not a verdict list", `{"i":0,"needed_by":"non`},
		// The pre-existing guard this fallback deliberately does not weaken: one bare object is not a
		// chosen format, it is a reply that may have stopped. See parseVerdictLines' two-line note.
		{"a lone bare object stays refused", `{"i":1,"needed_by":"none","verdict":"drop"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if vs, ok := ParseVerdicts(tc.reply); ok {
				t.Errorf("parsed %d verdicts from %q; must refuse", len(vs), tc.reply)
			}
		})
	}
}

// PARTIAL ACCEPTANCE IS THE QUIET FAILURE. A reply cut off mid-object must be refused outright, not
// accepted for the lines that happened to be whole — a partial batch reported as a complete judgement
// leaves the omitted candidates looking like deliberate keeps.
func TestATruncatedNewlineReplyIsRefusedAndLabelledTruncated(t *testing.T) {
	cut := `{"i": 0, "needed_by": "none", "quote": "", "verdict": "drop"}
{"i": 1, "needed_by": "none", "quote": "", "verdict": "drop"}
{"i": 2, "needed_by": "a", "quote": "I will run the su`
	if vs, ok := ParseVerdicts(cut); ok {
		t.Errorf("parsed %d verdicts from a truncated reply; must refuse", len(vs))
	}
	if !ReplyWasTruncated(cut) {
		t.Error("a newline reply whose earlier lines parsed and whose last is a fragment ran out of " +
			"budget; calling it a format failure points at the wrong remedy")
	}
}

// The array path must be untouched by the fallback: it is tried first and still wins.
func TestArrayRepliesStillParseAndEmptyArrayStillMeansKeepAll(t *testing.T) {
	if vs, ok := ParseVerdicts(`[{"i":0,"needed_by":"none","verdict":"drop"}]`); !ok || len(vs) != 1 {
		t.Fatalf("array reply: ok=%v n=%d", ok, len(vs))
	}
	// A closed empty array is a deliberate keep-everything and must stay distinguishable from junk.
	if vs, ok := ParseVerdicts(`[]`); !ok || len(vs) != 0 {
		t.Fatalf("empty array: ok=%v n=%d, want ok with 0 verdicts", ok, len(vs))
	}
	if ReplyWasTruncated(`[]`) {
		t.Error("a closed empty array is not truncated")
	}
}
