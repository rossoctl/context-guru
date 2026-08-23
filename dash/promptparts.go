package dash

// Splitting the system prompt into the parts it was assembled from.
//
// The Inventory page could show the system prompt's total weight and, once the text was stored,
// the whole thing as one wall of text. Neither answers the question a reader actually has, which
// is "WHICH of the things in there is costing me this". A 12,000-token prompt is not one
// decision: it is the agent's own preamble, plus the harness rules, plus the environment block,
// plus whatever the reader's own CLAUDE.md contributed — and only some of those are theirs to
// change. Undivided, the page could show the number and not the answer.
//
// THE SPLIT IS ON HEADINGS, AND THAT IS A DELIBERATE SECOND CHOICE. The real structural seam is
// the wire one: `system` arrives as an ARRAY of blocks, each separately cacheable. Those
// boundaries are not recoverable here — systemTextOf joins the blocks with a blank line before
// anything is stored, so every row already in the database has lost them, and a reader cannot be
// shown a decomposition that only works for prompts captured after today. Markdown headings are
// present in the text itself, are what the prompt is actually organised by, and work on every row
// ever captured. The UI says which of the two it is showing, because "part" could mean either and
// a reader comparing this against a cache breakpoint count would otherwise be misled.
//
// Everything before the first heading is a part too, named for what it is rather than dropped:
// on Claude Code that leading chunk is the identity preamble, and it is not small.

import (
	"strings"

	"github.com/rossoctl/context-guru/internal/tokens"
)

// PromptPart is one section of the system prompt, identified and measured.
type PromptPart struct {
	// Title is the heading text, or "Preamble" for the text ahead of the first heading. Level
	// is the heading depth (1 for `#`, 2 for `##`, 0 for the preamble) so a UI can indent
	// rather than present a flat list of forty equal-looking sections.
	Title string `json:"title"`
	Level int    `json:"level"`
	// Tokens is this part's own measured BPE weight and Share its percentage of the parts'
	// SUM — not of the region's stored total. The two differ, by a token or two per boundary:
	// BPE is not additive across a split, so measuring the pieces and measuring the whole are
	// different measurements of the same bytes. PromptRegion.PartsTokens carries the sum so a
	// UI can show both figures instead of implying they must agree.
	Tokens int     `json:"tokens"`
	Share  float64 `json:"share"`
	Text   string  `json:"text"`
}

// splitSystemPrompt divides a system prompt at its top-level markdown headings.
//
// `#` and `##` only. Deeper levels are left inside their parent: a prompt with forty `###`
// subsections split at every one of them is the same wall of text with more furniture, and the
// question this answers ("which region owns my prompt") is answered at the top level.
//
// A heading is recognised only at the start of a line and only with a space after the hashes,
// so a `#` inside a fenced code block or a shell comment does not open a section. Fences are
// tracked for exactly that reason — Claude Code's own prompt contains ``` blocks with comments
// in them, and without the fence check its environment block split into fragments named after
// somebody's example command.
func splitSystemPrompt(text string) []PromptPart {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	type seam struct {
		at    int // byte offset of the heading line
		title string
		level int
	}
	var seams []seam
	inFence := false
	for off := 0; off < len(text); {
		end := strings.IndexByte(text[off:], '\n')
		line := text[off:]
		next := len(text)
		if end >= 0 {
			line = text[off : off+end]
			next = off + end + 1
		}
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "```"), strings.HasPrefix(trimmed, "~~~"):
			inFence = !inFence
		case inFence:
		case strings.HasPrefix(line, "## ") && len(strings.TrimSpace(line[3:])) > 0:
			seams = append(seams, seam{off, strings.TrimSpace(line[3:]), 2})
		case strings.HasPrefix(line, "# ") && len(strings.TrimSpace(line[2:])) > 0:
			seams = append(seams, seam{off, strings.TrimSpace(line[2:]), 1})
		}
		off = next
	}
	// No headings at all: one part, which is still worth returning. It tells the UI "this prompt
	// has no sections" rather than "the splitter found nothing", and those read differently.
	if len(seams) == 0 {
		return []PromptPart{{Title: wholeTitle, Tokens: tokens.Count(text), Text: text}}
	}
	out := make([]PromptPart, 0, len(seams)+1)
	if head := text[:seams[0].at]; strings.TrimSpace(head) != "" {
		out = append(out, PromptPart{Title: preambleTitle, Tokens: tokens.Count(head), Text: head})
	}
	for i, s := range seams {
		end := len(text)
		if i+1 < len(seams) {
			end = seams[i+1].at
		}
		body := text[s.at:end]
		out = append(out, PromptPart{
			Title: s.title, Level: s.level, Tokens: tokens.Count(body), Text: body,
		})
	}
	var sum int
	for _, p := range out {
		sum += p.Tokens
	}
	if sum > 0 {
		for i := range out {
			out[i].Share = 100 * float64(out[i].Tokens) / float64(sum)
		}
	}
	return out
}

// The two synthesised titles. Named constants because the UI does not special-case them and a
// reader must be able to tell a section the prompt actually names from one this code named.
const (
	preambleTitle = "Preamble — everything before the first heading"
	wholeTitle    = "The whole prompt — it has no headings to split on"
)
