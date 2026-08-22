package extract

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// A result that is a bare cap-sized slice of the input, with nothing saying content was
// dropped, is refused with its own rejection reason. In production this shape was accepted
// 25 times and supplied 53% of all reported savings.
func TestCapTruncationIsRejectedWithItsOwnReason(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&b, "-rw-r--r--@ 1 itayn staff %d Aug 21 09:%02d run-%04d.json\n", 1498+i, i%60, i)
	}
	body := b.String()
	cfg := DefaultCfg()
	cfg.Mode = "code"
	cfg.Rewrite = true
	cfg.AllowDeterministic = false // isolate the model leg: the fallback is tested separately
	// The model replies with a program that returns a bare prefix of the input — the same
	// damage the deterministic projection used to do silently.
	m := fixedModel{prog: `OUTPUT = INPUT[:4000]
SUMMARY = ""`}
	out, _, strat, why := RunExtractionDetail(context.Background(), body, "compare the runs", nil,
		tokensOf(body), cfg, m)
	if strat != "none" || out != "" {
		t.Fatalf("a bare cap slice must not be accepted: strategy=%q out=%d chars", strat, len(out))
	}
	if !strings.Contains(why, "truncated at the character cap") {
		t.Fatalf("want the cap rejection, got %q", why)
	}
}

// The same slice, once it says what it dropped, is a legitimate reduction.
func TestMarkedWindowIsAccepted(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&b, "-rw-r--r--@ 1 itayn staff %d Aug 21 09:%02d run-%04d.json\n", 1498+i, i%60, i)
	}
	body := b.String()
	cfg := DefaultCfg()
	cfg.Mode = "deterministic"
	cfg.Rewrite = true
	out, _, strat, why := RunExtractionDetail(context.Background(), body, "compare the runs", nil,
		tokensOf(body), cfg, nil)
	if strat != "deterministic" {
		t.Fatalf("the deterministic window should be accepted now that it is marked: %q (%s)", strat, why)
	}
	if !strings.Contains(out, "elided") {
		t.Fatalf("the window must name what it dropped:\n%s", out)
	}
	// Line-aligned: no truncated final record. Every non-marker line must be a whole
	// line of the input.
	for _, ln := range strings.Split(out, "\n") {
		if ln == "" || strings.Contains(ln, "elided") {
			continue
		}
		if !strings.Contains(body, ln+"\n") {
			t.Fatalf("window cut mid-line: %q", ln)
		}
	}
}

// Dropping interior lines splices non-adjacent lines into statements the input never made.
// The gap must be visible. This is req20042.seq1, reduced to its shape.
func TestLineSelectionMarksTheGap(t *testing.T) {
	body := strings.Join([]string{
		"#         POSITIONAL argument, not --run-dir. AND its arm list is HARDCODED at",
		"#            the top of the file, so a new arm is not picked up automatically.",
		"#            (vllm-structural-llm-lo, vllm-summarize-lo) and it iterates ORD rather",
		"#            than the manifest order, so the columns can disagree with the header.",
		"#            vllm-agentdiet-lo are SILENTLY OMITTED and the table still looks complete.",
	}, "\n")
	// A filter that keeps lines 1, 3 and 5 — fluent, and false.
	result := strings.Join([]string{
		"#         POSITIONAL argument, not --run-dir. AND its arm list is HARDCODED at",
		"#            (vllm-structural-llm-lo, vllm-summarize-lo) and it iterates ORD rather",
		"#            vllm-agentdiet-lo are SILENTLY OMITTED and the table still looks complete.",
	}, "\n")
	got := markElisions(result, body)
	if strings.Count(got, "elided") != 2 {
		t.Fatalf("both interior gaps must be marked:\n%s", got)
	}
	if !strings.Contains(got, "... 1 line elided ...") {
		t.Fatalf("single-line gaps are marked in the singular:\n%s", got)
	}
	// Every original line that survived is still verbatim.
	for _, ln := range strings.Split(result, "\n") {
		if !strings.Contains(got, ln) {
			t.Fatalf("marking must not alter kept lines: %q missing", ln)
		}
	}
}

// Trailing content dropped is the `ls -l` failure: the last kept line looks like the end of
// the output and is not.
func TestTrailingElisionIsMarked(t *testing.T) {
	body := "alpha\nbravo\ncharlie\ndelta"
	got := markElisions("alpha\nbravo", body)
	if !strings.HasSuffix(got, "... 2 lines elided ...") {
		t.Fatalf("a truncated tail must be marked:\n%s", got)
	}
}

// A reduction that already marks its own gaps is not annotated twice.
func TestExistingMarkersAreNotDuplicated(t *testing.T) {
	body := "alpha\nbravo\ncharlie\ndelta"
	in := "alpha\n... 2 lines elided ...\ndelta"
	if got := markElisions(in, body); got != in {
		t.Fatalf("a self-marked reduction must be left alone:\n%s", got)
	}
}

// A reworded result has no honest place for a marker, so it is left for the derivation
// check to judge.
func TestRewordedResultIsLeftAlone(t *testing.T) {
	body := "alpha\nbravo\ncharlie"
	in := "the run mentioned alpha and charlie"
	if got := markElisions(in, body); got != in {
		t.Fatalf("a rewritten result must not be marked:\n%s", got)
	}
}

// Markers are ours, not content, so a heavily marked reduction must still pass the
// derivation check rather than reading as fabricated.
func TestMarkedReductionPassesTheDerivationCheck(t *testing.T) {
	body := strings.Repeat("alpha\nbravo\ncharlie\ndelta\n", 40)
	marked := markElisions("alpha\ndelta", body)
	cfg := DefaultCfg()
	cfg.Rewrite = true
	if ok, why := validateExtraction(marked, body, nil, cfg); !ok {
		t.Fatalf("a marked reduction must pass validation: %s\n%s", why, marked)
	}
}

func tokensOf(s string) int { return len(s) / 4 }

// The window rule must hold for EVERY strategy, not only the deterministic projection: a model
// that replies with the first N whole lines of its input has truncated it, and an elision marker
// makes that honest without making it an extraction. The caller withholds the window by setting
// MaxChars to 0 for content whose class cannot support one.
func TestAContiguousWindowIsRefusedFromAnyStrategyWhenWithheld(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 160; i++ {
		fmt.Fprintf(&b, "./components/offload/extract_econ_test.go:%d:func TestSomething%d(t *testing.T) {\n", 100+i, i)
	}
	body := b.String()
	head := strings.Join(strings.Split(body, "\n")[:37], "\n")

	cfg := DefaultCfg()
	cfg.Mode, cfg.Rewrite, cfg.AllowDeterministic = "code", true, false
	// With the window allowed, a marked head IS accepted — that is the ls -l case.
	cfg.MaxChars = 4000
	if !isLineWindow(markElisions(head, body), body) {
		t.Fatal("a head of whole lines must be recognised as a contiguous window")
	}
	if ok, why := validateExtraction(markElisions(head, body), body, nil, cfg); !ok {
		t.Fatalf("a marked window must still pass validation when it is allowed: %s", why)
	}

	// With it withheld, the same result is refused, whatever produced it.
	cfg.MaxChars = 0
	out, _, strat, why := RunExtractionDetail(context.Background(), body, "find the tests", nil,
		tokensOf(body), cfg, fixedModel{prog: `lines = INPUT.split("\n")
OUTPUT = "\n".join(lines[:37])
SUMMARY = "first 37 matches"`})
	if strat != "none" || out != "" {
		t.Fatalf("a withheld window must be refused from the code leg too: strategy=%q", strat)
	}
	if !strings.Contains(why, "contiguous window is not a reduction") {
		t.Fatalf("want the window refusal, got %q", why)
	}

	// And a genuine selection with gaps is NOT a window, so it is unaffected.
	lines := strings.Split(body, "\n")
	var gapped []string
	for i := 0; i < 60; i += 3 {
		gapped = append(gapped, lines[i])
	}
	if isLineWindow(strings.Join(gapped, "\n"), body) {
		t.Fatal("a selection with gaps must not be read as a contiguous window")
	}
}
