package reformat

import (
	"regexp"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
)

func init() { components.Register("textclean", newTextClean) }

// TextClean is the lossless Reformat for PLAIN TEXT — the shape most agent tool output
// actually has (measured: 1,724 of 1,748 distinct tool outputs across every capture on
// this box are not JSON at all, so `format` and `toon` never see them). It removes three
// kinds of pure display noise and nothing else:
//
//   - ANSI/VT100 escape sequences — terminal colour and cursor control, never content;
//   - carriage-return redraws — a line rewritten in place by \r never displayed its
//     earlier bytes, so keeping only the final rendered segment loses nothing the agent
//     ever saw (a trailing \r is a CRLF separator, NOT a redraw, and is preserved).
//
// Both transforms already existed in `extract` (an Offload), which charged
// them a <<cg:HASH>> marker, a stash and a 300–400 token floor: most output paid an
// offload price for what is a pure lossless reformat, and anything under the floor paid
// nothing because it was declined. As a Reformat this needs no marker, no stash and no
// floor, and it is a pure function of the content, so it is byte-stable across turns and
// process restarts — the same cache posture as `format`.
//
// What it deliberately does NOT do. Dropping progress-bar lines and collapsing repeated
// lines or blocks remove real lines, so they need extract's stash to be recoverable.
// Trimming trailing whitespace changes meaning in a unified diff (a context line whose
// only content is the leading space) and in markdown (the two-space line break).
// Collapsing runs of blank lines is safe but MEASURED WORTHLESS: over 1,748 real tool
// outputs it saved 21 tokens in total, against 16,721 for the ANSI strip and 498 for the
// redraws (TestCorpusMeasureTextCleanBreakdown keeps that measurement runnable), so it
// is not worth the line of code or the argument about whether padding is content.
type TextClean struct{ minTokens int }

type textCleanConfig struct {
	MinTokens int `yaml:"min_tokens"`
}

func newTextClean(raw []byte) (components.Component, error) {
	cfg := textCleanConfig{MinTokens: 50}
	if err := components.Decode(raw, &cfg); err != nil {
		return nil, err
	}
	return &TextClean{minTokens: cfg.MinTokens}, nil
}

func (TextClean) Name() string                 { return "textclean" }
func (TextClean) Enabled(*components.Ctx) bool { return true }

func (t *TextClean) Reformat(req *schemas.BifrostChatRequest, rep *components.Report, _ *components.Ctx) error {
	acted := false
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
		if schema.TextTokens(content) < t.minTokens {
			rep.Gate("below_min_tokens")
			continue
		}
		out, changed := cleanText(content)
		if !changed {
			rep.Gate("no_terminal_noise")
			continue
		}
		// Verify-then-adopt: keep the candidate only if every informative line survived
		// it intact AND it is strictly smaller. Anything else leaves the message verbatim.
		if !sameInformativeLines(content, out) {
			rep.Gate("content_would_change") // a bug guard, not an expected path
			continue
		}
		if schema.TextTokens(out) >= schema.TextTokens(content) {
			rep.Gate("already_clean")
			continue
		}
		schema.SetMessageText(m, out)
		acted = true
	}
	if !acted {
		rep.Skipped = true
	}
	return nil
}

// ansiRe matches ANSI/VT100 escape sequences (colours, cursor moves, OSC strings).
// Same pattern extract uses; stripping them is universally safe — pure display control.
var ansiRe = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)")

// cleanText strips ANSI escapes and resolves carriage-return redraws. Deterministic and dependency-free: the same input always yields
// byte-identical output, which is what keeps the prompt cache anchored.
func cleanText(s string) (string, bool) {
	changed := false
	if ansiRe.MatchString(s) {
		s = ansiRe.ReplaceAllString(s, "")
		changed = true
	}
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		// A trailing "\r" is the CRLF separator surfacing after the split on "\n", NOT a
		// redraw: treating it as one would blank every CRLF line. Only an interior CR is
		// an in-place redraw; there the final segment is what was left on screen, and the
		// original trailing CR is re-attached so line endings stay byte-identical.
		if !strings.Contains(ln, "\r") {
			continue // no CR at all: pure content
		}
		core := strings.TrimSuffix(ln, "\r")
		j := strings.LastIndexByte(core, '\r')
		if j < 0 {
			continue // CRLF only, not a redraw
		}
		seg := core[j+1:]
		if strings.HasSuffix(ln, "\r") {
			seg += "\r"
		}
		lines[i] = seg
		changed = true
	}
	if !changed {
		return s, false
	}
	return strings.Join(lines, "\n"), true
}

// sameInformativeLines reports whether two texts carry the same informative lines in the
// same order — every line that is not blank once the display control is resolved. It is
// the runtime half of the losslessness claim: the transforms may only ever remove
// terminal control and bytes a redraw overwrote, so if this disagrees, the candidate is
// dropped rather than sent.
func sameInformativeLines(before, after string) bool {
	a, b := informativeLines(before), informativeLines(after)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// informativeLines renders a text the way a terminal would (ANSI stripped, \r redraws
// resolved) and returns its non-blank lines.
func informativeLines(s string) []string {
	s = ansiRe.ReplaceAllString(s, "")
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		core := strings.TrimSuffix(ln, "\r")
		if j := strings.LastIndexByte(core, '\r'); j >= 0 {
			core = core[j+1:]
		}
		if strings.TrimSpace(core) == "" {
			continue
		}
		out = append(out, core)
	}
	return out
}

func init() {
	components.RegisterFields("textclean", textCleanConfig{}, []components.Field{
		{Key: "min_tokens", Type: components.FieldInt, Default: 50, Min: 1,
			Hint: "Only clean terminal control out of outputs above this many tokens; below it the saving is noise."},
	})
}
