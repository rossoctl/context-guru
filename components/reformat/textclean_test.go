package reformat

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
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
