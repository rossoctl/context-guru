package dash

import (
	"strings"
	"testing"
)

// ccPrompt is the shape of a real Claude Code system prompt: an identity preamble with no
// heading, then top-level sections, a fenced code block that contains a `#` COMMENT, and the
// user's own CLAUDE.md pasted in with its own headings.
const ccPrompt = `You are Claude Code, Anthropic's official CLI for Claude.

# Harness

Text you output outside of tool use is displayed to the user.

## Git

Use the gh CLI. Example:

` + "```" + `bash
# this is a shell comment, not a heading
git status
` + "```" + `

# Environment

Working directory: /home/user/project

# Memory

You have a persistent file-based memory.
`

func partTitles(ps []PromptPart) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Title)
	}
	return out
}

func TestSplitSystemPromptNamesEverySection(t *testing.T) {
	ps := splitSystemPrompt(ccPrompt)
	got := partTitles(ps)
	want := []string{preambleTitle, "Harness", "Git", "Environment", "Memory"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("parts = %v, want %v", got, want)
	}
	// The fenced `# this is a shell comment` did NOT open a section. Without the fence check the
	// Git section splits in two and one of them is named after somebody's example command.
	for _, p := range ps {
		if strings.HasPrefix(p.Title, "this is a shell comment") {
			t.Error("a heading inside a fenced code block opened a section")
		}
	}
	// The code block stays with the section it belongs to, whole.
	var git string
	for _, p := range ps {
		if p.Title == "Git" {
			git = p.Text
		}
	}
	if !strings.Contains(git, "git status") || !strings.Contains(git, "shell comment") {
		t.Errorf("the Git section lost its code block: %q", git)
	}
	// Levels, so a UI can nest rather than print a flat list of equals.
	byTitle := map[string]int{}
	for _, p := range ps {
		byTitle[p.Title] = p.Level
	}
	if byTitle["Harness"] != 1 || byTitle["Git"] != 2 || byTitle[preambleTitle] != 0 {
		t.Errorf("levels = %v, want Harness 1 / Git 2 / preamble 0", byTitle)
	}
}

// The parts must reassemble into the original prompt, exactly. This is the assertion that makes
// the decomposition trustworthy: a reader is being shown pieces and told they are the whole, so
// nothing may be dropped, duplicated or reordered by the split.
func TestSplitSystemPromptLosesNothing(t *testing.T) {
	var b strings.Builder
	for _, p := range splitSystemPrompt(ccPrompt) {
		b.WriteString(p.Text)
	}
	if b.String() != ccPrompt {
		t.Errorf("the parts do not reassemble into the prompt:\n--- got ---\n%q\n--- want ---\n%q",
			b.String(), ccPrompt)
	}
}

func TestSplitSystemPromptSharesSumTo100(t *testing.T) {
	var sum, tok float64
	ps := splitSystemPrompt(ccPrompt)
	for _, p := range ps {
		sum += p.Share
		tok += float64(p.Tokens)
		if p.Tokens <= 0 {
			t.Errorf("part %q measured %d tokens", p.Title, p.Tokens)
		}
	}
	if sum < 99.9 || sum > 100.1 {
		t.Errorf("shares sum to %.3f, want 100", sum)
	}
	if tok <= 0 {
		t.Fatal("the parts measured nothing")
	}
}

// A prompt with no headings is ONE part that says so, not zero parts. "This prompt has no
// sections" and "the splitter found nothing" read differently to a reader, and only one of them
// is true.
func TestSplitSystemPromptWithNoHeadings(t *testing.T) {
	ps := splitSystemPrompt("just some instructions, no headings at all")
	if len(ps) != 1 || ps[0].Title != wholeTitle {
		t.Fatalf("parts = %v, want one part titled %q", partTitles(ps), wholeTitle)
	}
	if ps[0].Tokens <= 0 {
		t.Error("the single part measured nothing")
	}
}

func TestSplitSystemPromptOnEmptyText(t *testing.T) {
	for _, s := range []string{"", "   ", "\n\n\t\n"} {
		if ps := splitSystemPrompt(s); ps != nil {
			t.Errorf("splitSystemPrompt(%q) = %v, want nil", s, partTitles(ps))
		}
	}
}

// A prompt that OPENS on a heading has no preamble, and must not get an empty one.
func TestSplitSystemPromptWithNoPreamble(t *testing.T) {
	ps := splitSystemPrompt("# First\n\nbody\n")
	if len(ps) != 1 || ps[0].Title != "First" {
		t.Fatalf("parts = %v, want just First", partTitles(ps))
	}
}
