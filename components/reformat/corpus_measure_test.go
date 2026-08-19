// Measurement of the reformatters against captures of REAL agent traffic, with the
// repo's own tokenizer (internal/tokens via schema.TextTokens — never bytes/4). It is a
// measurement, not an assertion: captures are never committed (they hold real traffic),
// so it skips unless pointed at one.
//
//	CG_CORPUS='/home/vpcuser/cg-research/bench/*.jsonl,/tmp/cg-runs/*.jsonl' \
//	  go test ./components/reformat -run CorpusMeasure -v
//
// It reports, per component, how often it fires across the distinct tool outputs in the
// corpus and the tokens it removes — the two numbers that say whether a change is worth
// its complexity. Run it on the same corpus at two commits to attribute a delta to one
// change; the per-shape counters below say WHICH shape paid.
package reformat

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
	"github.com/tidwall/gjson"
)

// corpusOutputs returns the distinct tool-result texts across every capture matched by
// CG_CORPUS (comma-separated globs). Distinct, because the same output is re-sent on
// every later turn of a session and counting it once per turn would inflate every
// number by the transcript's length.
func corpusOutputs(t *testing.T) []string {
	t.Helper()
	spec := os.Getenv("CG_CORPUS")
	if spec == "" {
		t.Skip("set CG_CORPUS to captured-traffic jsonl globs")
	}
	seen := map[string]bool{}
	var out []string
	for _, g := range strings.Split(spec, ",") {
		files, _ := filepath.Glob(strings.TrimSpace(g))
		for _, file := range files {
			f, err := os.Open(file)
			if err != nil {
				continue
			}
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 0, 1<<20), 128<<20)
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
			f.Close()
		}
	}
	if len(out) == 0 {
		t.Skipf("CG_CORPUS %q matched no tool outputs", spec)
	}
	return out
}

func TestCorpusMeasureReformatters(t *testing.T) {
	outputs := corpusOutputs(t)
	total := 0
	for _, o := range outputs {
		total += schema.TextTokens(o)
	}
	t.Logf("corpus: %d distinct tool outputs, %d tokens", len(outputs), total)

	for _, c := range []components.Reformat{&Format{minTokens: 50}, &Toon{minTokens: 50}, &TextClean{minTokens: 50}} {
		fired, before, after := 0, 0, 0
		gates := map[string]int{}
		for _, o := range outputs {
			req := &schemas.BifrostChatRequest{
				Provider: schemas.Anthropic,
				Input:    []schemas.ChatMessage{blockMsg(schemas.ChatMessageRoleTool, o)},
			}
			rep := &components.Report{}
			if err := c.Reformat(req, rep, ctx()); err != nil {
				t.Fatalf("%s: %v", c.Name(), err)
			}
			for g, n := range rep.Gates {
				gates[g] += n
			}
			got := schema.MessageText(req.Input[0])
			if got == o {
				continue
			}
			fired++
			before += schema.TextTokens(o)
			after += schema.TextTokens(got)
		}
		t.Logf("%-10s fired on %d/%d outputs (%.1f%%): %d -> %d tokens, saved %d (%.2f%% of corpus); gates=%v",
			c.Name(), fired, len(outputs), 100*float64(fired)/float64(len(outputs)),
			before, after, before-after, 100*float64(before-after)/float64(total), gates)
	}
}

// TestCorpusMeasureTextCleanBreakdown attributes textclean's saving to each of its three
// transforms, and splits it by extract's token floor — the outputs BELOW that floor are
// savings no component takes today, because `extract` (the only other code path to these
// transforms) declines everything under it. Above the floor the saving is not new money,
// it is the same money without a marker, a stash and an expand round-trip.
func TestCorpusMeasureTextCleanBreakdown(t *testing.T) {
	outputs := corpusOutputs(t)
	const extractFloor = 400 // extract's min_tokens under the codesmart preset

	type acc struct{ n, saved int }
	var ansi, cr, blank, belowFloor, aboveFloor acc
	for _, o := range outputs {
		tok := schema.TextTokens(o)
		if tok < 50 {
			continue // textclean's own floor
		}
		add := func(a *acc, out string) {
			if out == o {
				return
			}
			a.n++
			a.saved += tok - schema.TextTokens(out)
		}
		add(&ansi, ansiRe.ReplaceAllString(o, ""))
		add(&cr, resolveRedraws(o))
		add(&blank, collapseBlankRuns(o))
		if out, changed := cleanText(o); changed && schema.TextTokens(out) < tok {
			if tok < extractFloor {
				belowFloor.n++
				belowFloor.saved += tok - schema.TextTokens(out)
			} else {
				aboveFloor.n++
				aboveFloor.saved += tok - schema.TextTokens(out)
			}
		}
	}
	t.Logf("ansi:      %d outputs, %d tokens", ansi.n, ansi.saved)
	t.Logf("cr redraw: %d outputs, %d tokens", cr.n, cr.saved)
	t.Logf("blank run: %d outputs, %d tokens", blank.n, blank.saved)
	t.Logf("below extract's %d-token floor (NEW saving): %d outputs, %d tokens", extractFloor, belowFloor.n, belowFloor.saved)
	t.Logf("above it (same saving, no marker/stash):     %d outputs, %d tokens", aboveFloor.n, aboveFloor.saved)
}

// resolveRedraws and collapseBlankRuns are cleanText's two non-ANSI halves, isolated so
// the measurement above can attribute the saving. Test-only: cleanText does both in one
// pass because they share the line split.
func resolveRedraws(s string) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		core := strings.TrimSuffix(ln, "\r")
		if j := strings.LastIndexByte(core, '\r'); j >= 0 {
			seg := core[j+1:]
			if strings.HasSuffix(ln, "\r") {
				seg += "\r"
			}
			lines[i] = seg
		}
	}
	return strings.Join(lines, "\n")
}

func collapseBlankRuns(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" && len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
			continue
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}
