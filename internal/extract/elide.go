package extract

import (
	"fmt"
	"strings"
)

// Elision marking: make a reduction SAY what it dropped.
//
// Two real production failures motivate this file, both from the same cause — a reduction
// that removes content and leaves nothing behind to signal the gap:
//
//   - A line-selecting filter spliced three fragments of three different original lines
//     into one fluent paragraph that said something the input did not. The operational
//     warnings in a `.env` ("POSITIONAL argument, not --run-dir", "SILENTLY OMITTED") were
//     recombined into a new, plausible, false statement. Worse than truncation, because
//     nothing marks it as incomplete.
//   - A deterministic projection returned the first 4,000 characters of four concatenated
//     `ls -l` listings, ending mid-line, with an empty summary. Two of the four directories
//     were simply gone and the reader had no way to know.
//
// The fix is not to reduce less; it is to make every gap visible. A marked reduction is an
// honest one: the agent can see that something was cut and recover it with the expand tool.
// The markers are excluded from derivationRatio (isElisionMarker), so marking a reduction
// never makes it look fabricated.

// elisionLine renders the note that stands in for dropped lines. It must satisfy
// isElisionMarker so the derivation check treats it as ours rather than as invented content.
func elisionLine(n int) string {
	if n == 1 {
		return "... 1 line elided ..."
	}
	return fmt.Sprintf("... %d lines elided ...", n)
}

func elisionChars(n int) string {
	return fmt.Sprintf("... %d characters elided ...", n)
}

// markElisions inserts an "N lines elided" note at every gap where result dropped whole
// lines of body, and returns result unchanged when it cannot attribute the gaps.
//
// It only acts when the result's content lines are an in-order subsequence of the body's,
// which is what a line-selecting filter produces and is the case the splicing failure comes
// from. A result that reworded, reordered or reflowed lines is left alone: there is no
// honest place to put a marker, and the derivation check is what governs there.
//
// Lines the reduction already marked are preserved and suppress a marker of our own at that
// point, so a well-behaved filter is not double-annotated.
//
// ponytail: greedy first-match line matching, linear because the body cursor only advances. On
// a body with repeated identical lines (common in logs) the greedy match can attribute a gap to
// the earlier duplicate and so UNDER-count it. That is the harmless direction — the marker's job
// is "content is missing here", and the count is indicative — but do not read the number as
// exact. Use an LCS if a case is ever found where the count misleads.
func markElisions(result, body string) string {
	if result == "" || body == "" || !strings.Contains(result, "\n") {
		return result
	}
	rl := strings.Split(result, "\n")
	bl := strings.Split(body, "\n")
	if len(rl) > len(bl) {
		return result // more lines out than in: not a selection
	}
	out := make([]string, 0, len(rl)+8)
	j := 0      // cursor into body lines
	marked := 0 // body lines skipped since the last emitted content line
	alreadyMarked := false
	for _, ln := range rl {
		if isElisionMarker(ln) {
			// The reduction's own note (or a blank line). Keep it, and let it stand for
			// whatever gap precedes it.
			out = append(out, ln)
			marked, alreadyMarked = 0, true
			continue
		}
		k := j
		for k < len(bl) && bl[k] != ln {
			k++
		}
		if k == len(bl) {
			return result // this line is not a body line: not a pure selection
		}
		if gap := k - j; gap > 0 && !alreadyMarked {
			marked += gap
		}
		if marked > 0 {
			out = append(out, elisionLine(marked))
		}
		out = append(out, ln)
		j, marked, alreadyMarked = k+1, 0, false
	}
	if tail := len(bl) - j; tail > 0 && !alreadyMarked {
		// Trailing content dropped. This is the `ls -l` failure: the last kept line looked
		// like the end of the output and was not.
		out = append(out, elisionLine(tail))
	}
	if len(out) == len(rl) {
		return result // nothing was dropped
	}
	return strings.Join(out, "\n")
}

// capTruncated reports whether text is a bare cap-sized window of body carrying no note
// that anything was dropped — i.e. a truncation dressed as an extraction.
//
// The test is deliberately about the SHAPE of the result rather than about which strategy
// produced it: a model that replies with the first N characters of its input has done the
// same damage as a projection that cut a window, and both must be refused for the same
// reason. A contiguous slice of the body means no selection happened at all.
func capTruncated(text, body string, maxChars int) bool {
	if text == "" || maxChars <= 0 || len([]rune(text)) < maxChars {
		return false
	}
	if strings.Contains(text, "elided") || strings.Contains(text, expandToolName) {
		return false // it says what it dropped; that is the whole requirement
	}
	return strings.Contains(body, text)
}

// isLineWindow reports whether text is a CONTIGUOUS run of body's lines — a window, marked or
// not — rather than a selection with gaps.
//
// It is the general form of capTruncated: `head -n` and `sed -n '40,80p'` are truncations whatever
// their character count, and adding an elision marker makes such a result honest without making it
// an extraction. Whether a window is an acceptable reduction depends on the content: for a
// directory listing it is (the elided rows are more of the same), for a `grep -n` result it is not
// (every line is a distinct fact). The caller signals which by whether it allows a window at all
// (Cfg.MaxChars), because the caller is the layer that knows the content class.
//
// Marker lines and blanks are ignored, so a result the model annotated is judged on its content.
func isLineWindow(text, body string) bool {
	var content []string
	for _, ln := range strings.Split(text, "\n") {
		if !isElisionMarker(ln) {
			content = append(content, ln)
		}
	}
	if len(content) < 2 {
		return false // one line is not evidence of anything
	}
	bl := strings.Split(body, "\n")
	// Find where the first content line sits, then require every following one to be the very
	// next body line.
	for start := 0; start < len(bl); start++ {
		if bl[start] != content[0] {
			continue
		}
		if start+len(content) > len(bl) {
			return false
		}
		ok := true
		for i, ln := range content {
			if bl[start+i] != ln {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// windowLines cuts a line-aligned window of at most maxChars characters around the first
// matching term and names what it dropped on either side.
//
// Line-aligned because a mid-line cut is the specific failure mode: `-rw-r--r--@ 1 alice
// staff 1498 Aug ` is not a shorter truth, it is a broken record. The markers are what make
// the remainder readable as a fragment rather than as the whole output.
func windowLines(text string, terms []string, maxChars int) string {
	if maxChars <= 0 || len([]rune(text)) <= maxChars {
		return text
	}
	lines := strings.Split(text, "\n")
	// Anchor on the first line containing any term, so a keep-id decides the window.
	anchor := 0
	lowered := make([]string, len(lines))
	for i, ln := range lines {
		lowered[i] = strings.ToLower(ln)
	}
	for _, t := range terms {
		t = strings.TrimSpace(strings.ToLower(t))
		if t == "" {
			continue
		}
		found := false
		for i, ln := range lowered {
			if strings.Contains(ln, t) {
				anchor, found = i, true
				break
			}
		}
		if found {
			break
		}
	}
	// Grow outward from the anchor, backwards a quarter of the budget then forwards.
	budget := maxChars
	lo, hi := anchor, anchor
	if n := len(lines[anchor]) + 1; n <= budget {
		budget -= n
	} else {
		budget = 0
	}
	back := maxChars / 4
	for lo > 0 {
		n := len(lines[lo-1]) + 1
		if n > budget || n > back {
			break
		}
		lo--
		budget -= n
		back -= n
	}
	for hi < len(lines)-1 {
		n := len(lines[hi+1]) + 1
		if n > budget {
			break
		}
		hi++
		budget -= n
	}
	out := make([]string, 0, hi-lo+3)
	if dropped := charsIn(lines[:lo]); dropped > 0 {
		out = append(out, elisionChars(dropped))
	}
	out = append(out, lines[lo:hi+1]...)
	if dropped := charsIn(lines[hi+1:]); dropped > 0 {
		out = append(out, elisionChars(dropped))
	}
	return strings.Join(out, "\n")
}

func charsIn(lines []string) int {
	n := 0
	for _, ln := range lines {
		n += len(ln) + 1
	}
	return n
}
