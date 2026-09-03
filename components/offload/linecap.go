package offload

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/components/dsl"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
)

func init() { components.Register("linecap", newLinecap) }

// Linecap applies the two COMMAND-AGNOSTIC output rules, in one pass over each tool
// message: scattered duplicate-line collapse, then a per-line character cap.
//
// Why generic rather than more cmdfilter filters. cmdfilter ships 939 lines of
// per-command filters and production has matched exactly two of them (`ssh`, `uv-sync`;
// `no_filter_match` 164,865). Sixteen further rtk command signatures — pytest, apt, npm,
// pip, go test, cargo, tsc, eslint, mypy, ruff, docker, kubectl, make, gcc, git log, ps —
// were replayed against 9,763 real messages and every one matched ZERO. These two rules
// need no command signature, which is exactly why they fire where the signatures do not:
// on the same corpus they remove 1.75M tokens, 20.3% of everything shipped.
//
// It is an Offload, not a Reformat: both rules drop bytes. Under the default marker_mode
// the original is stashed and a <<cg:HASH>> marker names it, so the agent can call the
// expand tool and get the whole output back.
//
// Cache safety needs no depth gate. The rewrite is a pure function of the message's own
// text, so a message re-sent next turn rewrites to the same bytes and no cached position
// is re-anchored — the same argument format, textclean, searchfold and cmdfilter rely on.
type Linecap struct {
	maxLineChars int
	dups         bool
	minSize      int
	mode         markerMode
}

type linecapConfig struct {
	// MaxLineChars caps a single output line. 0 disables the cap.
	MaxLineChars *int `yaml:"max_line_chars"`
	// CollapseDuplicateLines folds non-adjacent repeated lines to their first occurrence
	// with an (xN) count. Distinct from extract's collapseObviousNoise, which handles
	// ADJACENT repeats — measured at 63 tokens of remaining value on real traffic, while
	// the non-adjacent case is worth 649,330.
	CollapseDuplicateLines *bool `yaml:"collapse_duplicate_lines"`
	// MinSize is the byte floor below which no rewrite is attempted (a marker would often
	// cost more than the saving). Shares cmdfilter's measured default.
	MinSize    *int   `yaml:"min_size"`
	MarkerMode string `yaml:"marker_mode"`
}

// defaultMaxLineChars is 500, chosen off the measured sweep rather than rtk's 120-200.
// Against 9,763 real messages (tokens removed / messages touched):
//
//	@200  2,031,381 tok / 2,742 msgs   @300  1,606,241 / 2,520
//	@500  1,105,387 tok / 1,337 msgs   @1000   745,350 /   862
//
// 500 takes 54% of @200's tokens while touching HALF as many messages, and every message
// it does not touch is a message whose bytes stay byte-identical for the provider's cache.
// A cap below 500 buys diminishing tokens for a widening blast radius.
const defaultMaxLineChars = 500

// minDupLineChars is the floor below which a repeated line is not worth collapsing.
// `}`, `];`, `---`, `  ` repeat constantly in structured output and each collapse costs an
// `(xN)` annotation, so short lines lose tokens as often as they save them.
const minDupLineChars = 8

// dupEdgeLines protects the first and last few lines from the duplicate collapse: a
// banner and a summary are where a model looks first, and a summary line that legitimately
// repeats a banner line is not noise.
const dupEdgeLines = 3

func newLinecap(raw []byte) (components.Component, error) {
	var cfg linecapConfig
	if err := components.Decode(raw, &cfg); err != nil {
		return nil, err
	}
	lc := &Linecap{maxLineChars: defaultMaxLineChars, dups: true,
		minSize: defaultMinSize, mode: parseMarkerMode(cfg.MarkerMode)}
	if cfg.MaxLineChars != nil {
		lc.maxLineChars = *cfg.MaxLineChars
	}
	if cfg.CollapseDuplicateLines != nil {
		lc.dups = *cfg.CollapseDuplicateLines
	}
	if cfg.MinSize != nil {
		lc.minSize = *cfg.MinSize
	}
	return lc, nil
}

func (Linecap) Name() string { return "linecap" }

func (lc *Linecap) Enabled(*components.Ctx) bool { return lc.maxLineChars > 0 || lc.dups }

func (lc *Linecap) Offload(req *schemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) ([]string, error) {
	var keys []string
	changed := 0
	for i := range req.Input {
		m := &req.Input[i]
		if m.Role != schemas.ChatMessageRoleTool {
			continue
		}
		if !schema.Rewritable(*m) {
			rep.Gate("non_text_blocks") // would be dropped by a text rewrite
			continue
		}
		content := schema.MessageText(*m)
		if content == "" {
			continue
		}
		if len(content) < lc.minSize {
			rep.Gate("below_min_size")
			continue
		}
		if skipReduce(c, content) {
			// marker-bearing (a cap could cut the marker line and orphan the stash) or
			// expanded by the agent — leave it verbatim.
			rep.Gate("marker_or_kept_verbatim")
			continue
		}
		out, folds := lc.rewrite(content)
		if folds == 0 {
			rep.Gate("no_long_or_repeated_lines")
			continue
		}
		// The marker costs tokens too, so the comparison includes it: a cap that barely
		// wins can still make the message larger (the shared marker-inclusive never-worse
		// guard; the pipeline's own guard is per REQUEST, not per message).
		newText, key, eff, ok := tryMark(c, lc.mode, content,
			" [full output: call "+expand.ToolName+"]",
			func(tok string) string {
				if tok == "" {
					return out
				}
				return out + "\n" + tok
			})
		if !ok {
			rep.Gate("marker_no_win")
			continue
		}
		if !commitMark(c, rep, eff, key, content) {
			continue // the store cannot back the marker; leave this message verbatim
		}
		schema.SetMessageText(m, newText)
		if key != "" {
			keys = append(keys, key)
		}
		changed++
	}
	if changed == 0 {
		rep.Skipped = true
	}
	return keys, nil
}

// rewrite applies both rules and reports how many lines it altered. Duplicate collapse
// runs FIRST so a dropped duplicate is not also charged as a capped line — the two rules
// overlap on the same corpus and measuring them in the other order double-counts.
//
// The (xN) annotations are rendered LAST, after the cap. Appending them during the
// collapse put them at the end of a line the cap then truncated, which ate the very
// annotation that makes the elision visible.
func (lc *Linecap) rewrite(content string) (string, int) {
	lines := strings.Split(content, "\n")
	var counts []int // parallel to lines; >1 means "annotate this one"
	folds := 0
	if lc.dups {
		lines, counts, folds = collapseScatteredDups(lines)
	}
	if lc.maxLineChars > 0 {
		for i, l := range lines {
			// len is a cheap upper bound on the rune count, so the byte test comes first.
			if len(l) <= lc.maxLineChars || neverTruncate(l) {
				continue
			}
			if t := clipMiddle(l, lc.maxLineChars); t != l {
				lines[i] = t
				folds++
			}
		}
	}
	if folds == 0 {
		return content, 0
	}
	for i, n := range counts {
		if n > 1 {
			lines[i] += "  (x" + strconv.Itoa(n) + ")"
		}
	}
	return strings.Join(lines, "\n"), folds
}

// clipMiddle caps a line by cutting its MIDDLE, keeping both ends.
//
// A head-only cut (dsl.TruncateRunes, which the DSL filters use) destroys the one token the
// agent needs whenever the reference sits at the END of the line — and in real diagnostics it
// usually does: `... error TS2345: Argument of type ... is not assignable ... Widget.tsx:42:11`,
// `Cannot find module '...' imported from '...'`, a JSON log line whose "file"/"line" keys
// come after the message. Measured on three such lines at 709, 631 and 666 chars: head-only
// dropped the path from all three.
//
// Keeping both ends removes the allow-list from the critical path. neverTruncate below still
// runs, but it is now defence in depth rather than the thing correctness rests on — which
// matters because an allow-list's completeness cannot be verified, and every diagnostic format
// that does not match one of its patterns was silently unprotected.
//
// The budget is honoured exactly: head + marker + tail never exceeds n runes.
//
// ponytail: a token in the MIDDLE third is still lost (head-only lost it too, so this is a
// ceiling and not a regression). Widen the tail share, or make the cut skip the run containing
// the last path-like token, if middle-of-line references ever show up in real output.
func clipMiddle(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	const marker = "...[cut]..."
	if n < 3 {
		return string(r[:n]) // no room even for an ellipsis; the budget still binds
	}
	if n <= len(marker) {
		return dsl.TruncateRunes(s, n) // room for one end and an ellipsis, not for two ends
	}
	budget := n - len([]rune(marker))
	// Two thirds to the head (the message), one third to the tail (the reference).
	head := budget * 2 / 3
	tail := budget - head
	return string(r[:head]) + marker + string(r[len(r)-tail:])
}

// neverTruncate is the allow-list: a line matching it survives any cap intact. Without it
// the cap is not safe to run generically — a 600-character stack frame, a compiler
// diagnostic with the offending source inlined, or a long import path in a traceback are
// exactly the lines a model needs whole, and they are long for the same reason the noise
// is. Each pattern names a class of thing the agent must be able to act on: a file
// reference, a source location, an error, a test outcome, an exit status, a diff line, a
// hunk header, a URL.
// ponytail: `^\S+:\d+` matches any SPACE-FREE line containing `:<digit>` anywhere, so a
// JSON log line is exempt by accident rather than by intent. Harmless — the exemptions are
// now defence in depth on top of clipMiddle, not the thing correctness rests on — but do not
// read this list as precise.
var neverTruncateRe = regexp.MustCompile(
	`^\S+:\d+` + // path:line and path:line:col references
		`|^\s*(File "|at )` + // Python and JVM/JS stack frames
		`|(?i)(error|exception|traceback|assertionerror|panic:|fatal)` +
		`|^(FAILED|ERROR|PASS|FAIL|ok) ` + // test-runner verdict lines
		`|(?i)exit (code|status)` +
		`|^[+-]{1,3}[^+-]` + // diff added/removed lines and ---/+++ headers
		`|^@@ ` + // diff hunk header
		`|https?://`)

func neverTruncate(line string) bool { return neverTruncateRe.MatchString(line) }

// diffShaped matches a blob whose head says the repeated lines in it are POSITIONAL. In a
// unified diff, two identical `+    return nil` lines are two distinct edits in two
// distinct hunks; collapsing them to one with `(x2)` produces something that cannot be
// read as a diff at all.
var diffShaped = regexp.MustCompile(`(?m)^(diff --git |@@ |Index: |--- |\+\+\+ )`)

// collapseScatteredDups keeps the FIRST occurrence of each distinct line in its original
// position, annotates it with the total count, and drops the rest. It is the non-adjacent
// case: extract's collapseObviousNoise already folds adjacent repeats, measured at 63
// tokens of remaining value against 649,330 for this one.
//
// Lossy — a repeat's position is not recoverable from the annotation — so the caller
// stashes the original and the marker names the expand tool. The (xN) is what keeps the
// elision VISIBLE: a silently deduplicated log reads as a complete log.
func collapseScatteredDups(lines []string) ([]string, []int, int) {
	if len(lines) <= 2*dupEdgeLines {
		return lines, nil, 0
	}
	if diffShaped.MatchString(strings.Join(lines, "\n")) {
		return lines, nil, 0
	}
	// Count first, then rewrite: a count is only trustworthy once the whole blob has been
	// seen, and the annotation has to state the true total on the first occurrence.
	lo, hi := dupEdgeLines, len(lines)-dupEdgeLines
	count := map[string]int{}
	for _, l := range lines[lo:hi] {
		if k, ok := dupKey(l); ok {
			count[k]++
		}
	}
	out := make([]string, 0, len(lines))
	counts := make([]int, 0, len(lines))
	keep := func(l string, n int) {
		out = append(out, l)
		counts = append(counts, n)
	}
	for _, l := range lines[:lo] {
		keep(l, 0)
	}
	seen := map[string]bool{}
	folds := 0
	for _, l := range lines[lo:hi] {
		k, ok := dupKey(l)
		switch {
		case !ok || count[k] < 2:
			keep(l, 0)
		case seen[k]:
			folds++ // a later repeat: drop it, the first occurrence carries the count
		default:
			seen[k] = true
			keep(l, count[k])
		}
	}
	for _, l := range lines[hi:] {
		keep(l, 0)
	}
	if folds == 0 {
		return lines, nil, 0
	}
	return out, counts, folds
}

// dupKey is the identity a duplicate is judged by, or ok=false for a line that must never
// be collapsed: blank, or too short to be worth an annotation.
func dupKey(line string) (string, bool) {
	t := strings.TrimSpace(line)
	if len(t) < minDupLineChars {
		return "", false
	}
	// Two lines differing only in a line number are DIFFERENT lines (see lineNum), so the
	// numbers stay in the key rather than being normalized out of it. Trimming leading
	// whitespace is the only normalization: indentation is layout, not identity.
	return t, true
}

func init() {
	components.RegisterFields("linecap", linecapConfig{}, []components.Field{
		{Key: "max_line_chars", Type: components.FieldInt, Default: defaultMaxLineChars,
			Hint: "Cap any single output line at this many characters, with an ellipsis marking the cut. Lines carrying a file path, source location, error, test verdict, exit status, diff marker or URL are never cut. 0 disables the cap."},
		{Key: "collapse_duplicate_lines", Type: components.FieldBool, Default: true,
			Hint: "Fold NON-adjacent repeated lines to their first occurrence with an (xN) count. Skipped entirely on diff-shaped output, where repeated lines are positional."},
		{Key: "min_size", Type: components.FieldInt, Default: defaultMinSize,
			Hint: "Only rewrite an output at least this many bytes long — below it a marker usually costs more than the saving."},
		markerModeField(),
	})
}
