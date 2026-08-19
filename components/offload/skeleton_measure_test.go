//go:build cg_skeleton

// Local measurement of the skeleton component against a capture of real agent
// traffic. Not an assertion and not part of CI — skeleton is evaluation-only (see
// docs/components/skeleton.md), and the point of this file is to answer "what would
// it actually remove, and from what" with numbers instead of intuition.
//
//	CG_SKEL_CAPTURE=/tmp/cgtune/capture.jsonl \
//	  CGO_ENABLED=1 go test -tags cg_skeleton ./components/offload -run SkeletonCapture -v
//
// It reports two things, because they differ by two orders of magnitude:
//   - AS SHIPPED: what the component does to this traffic today (fenced ```lang
//     blocks in tool outputs only).
//   - HEADROOM: what the skeletonizer would remove if it also handled the shape real
//     agents actually send — an unfenced, line-numbered file read. That path does not
//     exist; the figure is what an implementation of it would be worth.

package offload

import (
	"bufio"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/treesitter"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
)

// blankRunRe matches the newline padding skeleton leaves so line numbers stay valid.
var blankRunRe = regexp.MustCompile(`\n{2,}`)

// lineNumRe matches the "   123\t" prefix Claude Code puts on every line of a file read.
var lineNumRe = regexp.MustCompile(`^\s*\d+\t`)

// captureToolOutputs returns the deduplicated tool_result texts in a capture.
func captureToolOutputs(t *testing.T, file string) []string {
	t.Helper()
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	seen := map[string]bool{}
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		gjson.GetBytes(sc.Bytes(), "body.messages.#.content|@flatten").ForEach(func(_, blk gjson.Result) bool {
			if blk.Get("type").String() != "tool_result" {
				return true
			}
			txt := blk.Get("content").String()
			if c := blk.Get("content"); c.IsArray() {
				var b strings.Builder
				c.ForEach(func(_, p gjson.Result) bool { b.WriteString(p.Get("text").String()); return true })
				txt = b.String()
			}
			if txt != "" && !seen[txt] {
				seen[txt] = true
				out = append(out, txt)
			}
			return true
		})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// stripLineNumbers removes the "  N\t" gutter from a file read and reports whether the
// text looked like one (most lines carried a gutter).
func stripLineNumbers(s string) (string, bool) {
	lines := strings.Split(s, "\n")
	hits := 0
	for i, ln := range lines {
		if m := lineNumRe.FindString(ln); m != "" {
			hits++
			lines[i] = ln[len(m):]
		}
	}
	return strings.Join(lines, "\n"), hits*2 > len(lines)
}

// guessGrammar picks a grammar for a numbered file read: the path Claude Code echoes
// above the content when present, else a cheap content sniff for the two languages
// this box's traffic is made of.
func guessGrammar(raw, code string) string {
	for _, ln := range strings.Split(raw, "\n") {
		for _, tok := range strings.Fields(ln) {
			if lang := treesitter.LangForExt(strings.Trim(tok, "`'\"(),:")); lang != "" && path.Ext(tok) != "" {
				return lang
			}
		}
	}
	switch {
	case strings.Contains(code, "\nfunc ") && strings.Contains(code, "\npackage "):
		return "go"
	case strings.Contains(code, "\ndef ") || strings.Contains(code, "\nimport ") && strings.Contains(code, "self"):
		return "python"
	}
	return ""
}

func TestSkeletonCaptureMeasure(t *testing.T) {
	file := os.Getenv("CG_SKEL_CAPTURE")
	if file == "" {
		t.Skip("set CG_SKEL_CAPTURE=/path/capture.jsonl to measure")
	}
	outs := captureToolOutputs(t, file)
	s := skeletonFor(t, "min_tokens: 80\n")

	// --- as shipped -------------------------------------------------------
	var fenced, acted, tokBefore, tokAfter int
	for _, o := range outs {
		if strings.Contains(o, "```") {
			fenced++
		}
		req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{tool(o)}}
		rep := &components.Report{}
		if _, err := s.Offload(req, rep, &components.Ctx{Session: "m", Store: store.NewMemory(store.Options{})}); err != nil {
			t.Fatal(err)
		}
		if !rep.Skipped {
			acted++
			tokBefore += schema.TextTokens(o)
			tokAfter += schema.TextTokens(schema.MessageText(req.Input[0]))
		}
	}
	fmt.Printf("\ncapture: %s\n", file)
	fmt.Printf("AS SHIPPED  tool outputs=%d  with a fence=%d  acted on=%d  tokens %d->%d (removed %d)\n",
		len(outs), fenced, acted, tokBefore, tokAfter, tokBefore-tokAfter)

	// --- headroom: unfenced, line-numbered code reads ---------------------
	var (
		reads, elided     int
		codeTok, skelTok  int
		bodyBytes, padTok int
		allTok            int
		byLang            = map[string]int{}
	)
	for _, o := range outs {
		allTok += schema.TextTokens(o)
		code, numbered := stripLineNumbers(o)
		if !numbered {
			continue
		}
		grammar := guessGrammar(o, code)
		if grammar == "" {
			continue
		}
		reads++
		byLang[grammar]++
		skel, ok := skeletonize([]byte(code), grammar)
		if !ok {
			continue
		}
		elided++
		codeTok += schema.TextTokens(code)
		skelTok += schema.TextTokens(skel)
		// What line preservation costs: the same skeleton with its padding squeezed out.
		bodyBytes += len(code) - len(skel)
		padTok += schema.TextTokens(skel) - schema.TextTokens(blankRunRe.ReplaceAllString(skel, "\n"))
	}
	pct := func(a, b int) float64 {
		if b == 0 {
			return 0
		}
		return 100 * float64(a) / float64(b)
	}
	fmt.Printf("HEADROOM    numbered code reads=%d (%v)  skeletonizable=%d\n", reads, byLang, elided)
	fmt.Printf("HEADROOM    code tokens %d->%d  removed %d (%.1f%% of those reads, %.1f%% of ALL tool tokens in the capture)\n",
		codeTok, skelTok, codeTok-skelTok, pct(codeTok-skelTok, codeTok), pct(codeTok-skelTok, allTok))
	fmt.Printf("HEADROOM    removed bytes=%d, all of it function/method bodies; line-preserving padding costs %d tokens (%.1f%% of the saving)\n",
		bodyBytes, padTok, pct(padTok, codeTok-skelTok+padTok))
}
