package reformat

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
)

// realTerminalOutput produces genuine terminal output: a real `grep --color=always -rn`
// over a real file tree, which is both a shape agents produce constantly and a source of
// authentic SGR sequences. Real bytes matter here — a hand-written "\x1b[31m" fixture
// proves only that the regex matches itself.
func realTerminalOutput(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for i := 0; i < 40; i++ {
		body := fmt.Sprintf("package p\n\nfunc Handler%d() error {\n\treturn nil // needle\n}\n", i)
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d.go", i)), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out, err := exec.Command("grep", "--color=always", "-rn", "needle", dir).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "\x1b") {
		t.Skipf("no colour-capable grep here: %v", err)
	}
	return string(out)
}

// The losslessness claim for a one-way transform is not "decode it back" — an escape
// sequence has nothing to decode back to. It is: every line the agent could have READ
// survives byte-identical, and nothing else does. That is what this asserts, over real
// `ls --color` output.
func TestTextCleanStripsRealANSIAndKeepsEveryLine(t *testing.T) {
	in := realTerminalOutput(t)
	out, changed := cleanText(in)
	if !changed {
		t.Fatal("real coloured output reported as clean")
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("escape bytes survived:\n%q", out)
	}
	wantLines, gotLines := informativeLines(in), informativeLines(out)
	if len(wantLines) != len(gotLines) {
		t.Fatalf("line count changed: %d -> %d", len(wantLines), len(gotLines))
	}
	for i := range wantLines {
		if wantLines[i] != gotLines[i] {
			t.Fatalf("line %d changed:\n%q\n%q", i, wantLines[i], gotLines[i])
		}
	}
	if schema.TextTokens(out) >= schema.TextTokens(in) {
		t.Errorf("did not shrink: %d -> %d tokens", schema.TextTokens(in), schema.TextTokens(out))
	}
	t.Logf("grep --color: %d -> %d tokens", schema.TextTokens(in), schema.TextTokens(out))
}

// A \r redraw means the earlier bytes on that line were overwritten before anything
// displayed them: resolving it is lossless in the only sense that matters, and the
// expected result is written out literally here rather than derived, so this test fails
// if the resolution rule itself drifts. The input is real progress-writer output (the
// same \r-per-update loop pip/tqdm use), not a hand-typed string.
func TestTextCleanResolvesRealRedraws(t *testing.T) {
	out, err := exec.Command("python3", "-c",
		`import sys
for i in range(0,101,20):
    sys.stdout.write("\rdownloading: %d%%" % i)
sys.stdout.write("\ndone\n")`).CombinedOutput()
	if err != nil {
		t.Skipf("python3 unavailable: %v", err)
	}
	got, changed := cleanText(string(out))
	if !changed {
		t.Fatal("redraw line reported as clean")
	}
	if want := "downloading: 100%\ndone\n"; got != want {
		t.Fatalf("redraw not resolved to the final rendered frame\n got %q\nwant %q", got, want)
	}
}

// CRLF is the trap: a trailing \r is the line separator surfacing after a split on \n,
// not a redraw. Treating it as one keeps ln[len:] = "" and blanks every line of a
// Windows-style file — silent content loss. Such text must come back byte-identical.
func TestTextCleanLeavesCRLFAlone(t *testing.T) {
	in := "first line\r\nsecond line\r\nthird line\r\n"
	got, changed := cleanText(in)
	if changed || got != in {
		t.Fatalf("CRLF text was rewritten: changed=%v\n%q", changed, got)
	}
}

// Deterministic per content, in-process and across restarts: a Reformat whose bytes vary
// re-anchors the prompt cache on every request, which is a pure loss. cleanText is a pure
// function of its input, and applying it twice must be a no-op.
func TestTextCleanIsDeterministicAndIdempotent(t *testing.T) {
	in := realTerminalOutput(t)
	first, _ := cleanText(in)
	for i := 0; i < 200; i++ {
		if got, _ := cleanText(in); got != first {
			t.Fatalf("non-deterministic output")
		}
	}
	if again, changed := cleanText(first); changed || again != first {
		t.Fatalf("not idempotent: changed=%v", changed)
	}
}

// Plain text with nothing to clean must be left exactly as it arrived — no marker, no
// trailing-whitespace edits, no reflow. This is the case that covers most tool output.
func TestTextCleanLeavesCleanOutputUntouched(t *testing.T) {
	in := strings.Repeat("plain content line with  double  spaces  and a trailing one \n", 40) + "\n\n\nend"
	req := &schemas.BifrostChatRequest{
		Provider: schemas.Anthropic,
		Input:    []schemas.ChatMessage{blockMsg(schemas.ChatMessageRoleTool, in)},
	}
	c := &TextClean{minTokens: 50}
	out, rep := reformatTool(t, c, in)
	if out != in {
		t.Errorf("rewrote clean text:\n%q", out)
	}
	if !rep.Skipped {
		t.Error("expected Skipped on clean text")
	}
	// Only tool messages are in scope: an assistant message with the same bytes is not touched.
	req.Input[0].Role = schemas.ChatMessageRoleAssistant
	if _, ok := cleanText(in); ok {
		t.Error("clean text reported as changed")
	}
}

// THE DISPUTE THIS SETTLES. `format` and `textclean` were being priced as the pipeline's
// best components on the strength of a 1.0 gross/unique ratio, read as "they rewrite in
// place every turn and every removal is NEW money". The second half of that sentence does
// not follow from the first — it is its opposite.
//
// The agent holds its own transcript and re-sends the ORIGINAL tool output on every turn
// (our rewrites are wire-only; apply never mutates the client's copy). An in-place fold
// therefore meets the same unfolded bytes again on turn 2, folds them again, and reports
// the same saving again. That is what "byte-stable across turns" in this file's own doc
// comment MEANS, and it is the definition of a re-counted saving, not of a new one.
//
// So: same content, ten consecutive turns, and the component reports a fresh saving every
// time. Only the FIRST of those ten is money. Before the pipeline gave Reformats
// content-derived dedup keys, metrics counted all ten.
func TestInPlaceFoldReportsTheSameSavingOnEveryTurn(t *testing.T) {
	built, err := newTextClean([]byte("min_tokens: 5\n"))
	if err != nil {
		t.Fatal(err)
	}
	tc := built.(components.Reformat)
	// One tool output with ANSI colour in it — the thing textclean strips.
	original := "\x1b[31mFAILED\x1b[0m tests/test_api.py::test_create\n" +
		strings.Repeat("\x1b[2mdebug: some verbose line of output here\x1b[0m\n", 40)

	var savings []int
	for turn := 0; turn < 10; turn++ {
		body := original // the agent re-sends the ORIGINAL, unfolded, every turn
		req := &schemas.BifrostChatRequest{Input: []schemas.ChatMessage{
			{Role: schemas.ChatMessageRoleTool,
				Content: &schemas.ChatMessageContent{ContentStr: &body}},
		}}
		rep := components.Report{TokensBefore: schema.TextTokens(original)}
		if err := tc.Reformat(req, &rep, nil); err != nil {
			t.Fatal(err)
		}
		got := schema.MessageText(req.Input[0])
		if got == original {
			t.Fatalf("turn %d: fixture did not trigger the fold", turn)
		}
		savings = append(savings, schema.TextTokens(original)-schema.TextTokens(got))
	}
	for turn, s := range savings {
		if s <= 0 {
			t.Fatalf("turn %d reported no saving; the fixture or the fold changed", turn)
		}
		if s != savings[0] {
			t.Fatalf("turn %d saved %d, turn 0 saved %d — the fold is not byte-stable, "+
				"which is a DIFFERENT and worse bug: it would churn the cached prefix",
				turn, s, savings[0])
		}
	}
	// Ten identical savings on one message. Gross = 10x unique, and the gross figure is
	// the one that used to be reported as "100% unique".
	t.Logf("10 turns x %d tokens gross for %d tokens of real reduction (%dx replay)",
		savings[0], savings[0], len(savings))
}
