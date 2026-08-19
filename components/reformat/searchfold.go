package reformat

import (
	"regexp"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
)

func init() { components.Register("searchfold", newSearchfold) }

// Searchfold folds the repeated path prefix out of search output — `rg`/`grep -rn`
// hit lists, `find`/`ls -1`/`rg -l` path lists — by emitting each path (or parent
// directory) once as a heading and the rows beneath it:
//
//	pkg/a.go:12:foo          pkg/a.go
//	pkg/a.go:31:foo    =>    12:foo
//	pkg/b.go:7:foo           31:foo
//	                         pkg/b.go
//	                         7:foo
//
// It is a Reformat, so it must be lossless — and it is lossless BY CONSTRUCTION
// rather than by argument: every fold has an exact inverse, and a fold is adopted
// only if applying that inverse reproduces the input BYTE FOR BYTE and the result is
// strictly smaller (see adopt). Anything else returns the input unchanged. No path,
// line number or line is ever dropped, so there is nothing to stash and no marker.
//
// Cache safety: the fold is a pure function of the message's own text, so a message
// that reappears on the next turn folds to the same bytes. The already-cached prefix
// is byte-stable and no cached position is re-anchored, which is the same argument
// the other in-place components (format, toon, cmdfilter) rely on — hence no TailOnly
// gate, which would only make the FIRST turn's fold differ from later turns'.
//
// Routing is by COMMAND, not by shape: schema.ToolCalls says which call produced each
// tool result, so `rg`/`grep`/`find` output is folded and `cat`/build/test output is
// not looked at. When the request carries no pairing (an unmatched id, or a dialect
// with no call in scope) the fold is attempted anyway — it is a no-op on output that
// has no repeated path prefix, and the round-trip check makes attempting it safe.
type Searchfold struct{ minTokens int }

type searchfoldConfig struct {
	MinTokens int `yaml:"min_tokens"`
}

func newSearchfold(raw []byte) (components.Component, error) {
	cfg := searchfoldConfig{MinTokens: 50}
	if err := components.Decode(raw, &cfg); err != nil {
		return nil, err
	}
	return &Searchfold{minTokens: cfg.MinTokens}, nil
}

func (Searchfold) Name() string                 { return "searchfold" }
func (Searchfold) Enabled(*components.Ctx) bool { return true }

func (f *Searchfold) Reformat(req *schemas.BifrostChatRequest, rep *components.Report, _ *components.Ctx) error {
	pairs := schema.ToolCalls(req)
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
		if tc, ok := pairs[i]; ok && !isSearchCommand(tc.Command()) {
			rep.Gate("not_a_search_command")
			continue
		}
		content := schema.MessageText(*m)
		if schema.TextTokens(content) < f.minTokens {
			rep.Gate("below_min_tokens")
			continue
		}
		out := FoldSearchOutput(content)
		if out == content {
			rep.Gate("no_repeated_prefix")
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

// searchProgram matches the first word of a command (after any `cd x &&` / env
// prefixes are ignored by scanning every word) that produces path-prefixed output.
// `ls` is included for `ls -1`-style listings; a plain `ls` produces no paths and so
// folds to nothing anyway.
var searchProgram = regexp.MustCompile(`(^|[|;&(]\s*|\s)(rg|ag|ack|fd|find|ls|grep|egrep|fgrep|zgrep|git\s+grep|git\s+ls-files)\b`)

// isSearchCommand reports whether a command line runs a search/list program anywhere
// in its pipeline. Anywhere, not just at the head, because real agent traffic pipes
// and chains constantly (`cd /x && grep -rn foo . | head -50`), and the tail of a
// pipeline is what shapes the output.
func isSearchCommand(cmd string) bool {
	switch cmd {
	case "", "Grep", "Glob":
		return true // a bare tool name with no args, or the search tools themselves
	}
	if strings.HasPrefix(cmd, "Grep ") || strings.HasPrefix(cmd, "Glob ") {
		return true
	}
	return searchProgram.MatchString(cmd)
}

// FoldSearchOutput returns the smallest of the candidate folds whose inverse
// reproduces s exactly, or s itself when none of them wins. Exported so a measurement
// harness can run the transform over captured output without a request around it.
func FoldSearchOutput(s string) string {
	best := s
	for _, c := range []struct {
		fold    func(string) string
		inverse func(string) string
	}{
		{foldHitPath, unfoldHitPath},    // rg/grep -rn: several hits per file
		{foldHitDir, unfoldPrefixDir},   // one hit per file: factor the parent dir
		{foldPathList, unfoldPrefixDir}, // find/ls -1/rg -l: bare paths
	} {
		if out, ok := adopt(s, c.fold, c.inverse); ok && len(out) < len(best) {
			best = out
		}
	}
	return best
}

// adopt is the verify-then-adopt harness: run a fold, then require that its inverse
// reproduces the input byte for byte AND that the fold shrank it. It is what makes
// the transform lossless by construction — a fold whose heading rule collides with
// the payload (a content line that reads as a path, output already grouped by file,
// a basename ending in `/`) is silently declined instead of corrupting the output,
// and no case analysis has to be right for that to hold.
func adopt(in string, fold, inverse func(string) string) (string, bool) {
	out := fold(in)
	if len(out) >= len(in) {
		return in, false
	}
	if inverse(out) != in {
		return in, false
	}
	return out, true
}

// splitHit splits a `path<sep>NNN<sep>…` search hit into its path, where sep is `:`
// for a match line and `-` for a `grep -C` context line. It is hand-rolled because
// the shape needs a BACKREFERENCE (the two separators must be the same byte) and RE2
// has none. The first such field wins, and the path may not itself be all digits —
// otherwise single-file `grep -n -C` output (`12-foo`) would parse as path `1`.
func splitHit(l string) (string, bool) {
	for i := 0; i < len(l); i++ {
		c := l[i]
		if c != ':' && c != '-' {
			continue
		}
		j := i + 1
		for j < len(l) && l[j] >= '0' && l[j] <= '9' {
			j++
		}
		if j == i+1 || j >= len(l) || l[j] != c || i == 0 || allDigits(l[:i]) {
			continue
		}
		return l[:i], true
	}
	return "", false
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// digitRow matches the row shape foldHitPath emits: a line number, then its own
// separator, which is how the inverse recovers the exact byte that joined the path
// to the line number.
var digitRow = regexp.MustCompile(`^\d+([:-])`)

// foldHitPath emits each distinct path once as its own line and strips it from the
// rows beneath. A run of length one is folded too, which costs one newline and keeps
// the inverse total: every row starts with a digit, every heading does not.
func foldHitPath(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines)+8)
	cur, folded := "", false
	for _, l := range lines {
		p, ok := splitHit(l)
		if !ok {
			cur = ""
			out = append(out, l)
			continue
		}
		if p != cur {
			cur = p
			out = append(out, p)
		} else {
			folded = true // a second row under the same heading: this is where it pays
		}
		out = append(out, l[len(cur)+1:])
	}
	if !folded {
		return s
	}
	return strings.Join(out, "\n")
}

// unfoldHitPath is foldHitPath's exact inverse: a digit-leading row is re-joined to
// the heading above it with the row's own separator byte; anything else IS a heading
// (consumed) unless no row follows it, in which case it is a plain line.
func unfoldHitPath(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	cur, have := "", false
	for i, l := range lines {
		if m := digitRow.FindStringSubmatch(l); m != nil && have {
			out = append(out, cur+m[1]+l)
			continue
		} else if m != nil {
			out = append(out, l)
			continue
		}
		if i+1 < len(lines) && digitRow.MatchString(lines[i+1]) {
			cur, have = l, true
			continue
		}
		cur, have = "", false
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// foldHitDir factors the shared PARENT DIRECTORY out of hit rows instead of the whole
// path — the fold that pays when each file has only one hit, where foldHitPath's
// heading per row wins nothing.
func foldHitDir(s string) string {
	return foldPrefixDir(s, func(l string) (string, bool) {
		p, ok := splitHit(l)
		if !ok {
			return "", false
		}
		return parentDir(p)
	})
}

// foldPathList folds a bare path list (`find`, `ls -1`, `rg -l`) to its parent
// directory plus basenames. A line with a space or a `:` is not treated as a path: it
// is prose or a hit row, and folding it would only make the round-trip fail.
func foldPathList(s string) string {
	return foldPrefixDir(s, func(l string) (string, bool) {
		if strings.ContainsAny(l, ": \t") {
			return "", false
		}
		return parentDir(l)
	})
}

func parentDir(path string) (string, bool) {
	i := strings.LastIndexByte(path, '/')
	if i <= 0 {
		return "", false // no directory part, or a bare "/x": nothing to factor
	}
	return path[:i], true
}

// foldPrefixDir emits each distinct directory once, as a line ending in `/`, and
// strips it (with its slash) from the lines beneath, which are INDENTED with a tab.
//
// The tab costs one byte per row and buys a total inverse. Without it, the rows under
// a heading run to the next heading, so any ordinary line after a folded group — the
// empty last line every real output ends with — was swallowed into the group, the
// round-trip failed and the fold was declined. That is a systematic decline, not an
// edge case: it hit every captured `find` listing.
func foldPrefixDir(s string, dir func(string) (string, bool)) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines)+8)
	cur, folded := "", false
	for _, l := range lines {
		d, ok := dir(l)
		if !ok {
			cur = ""
			out = append(out, l)
			continue
		}
		if d != cur {
			cur = d
			out = append(out, d+"/")
		} else {
			folded = true
		}
		out = append(out, "\t"+l[len(cur)+1:])
	}
	if !folded {
		return s
	}
	return strings.Join(out, "\n")
}

// unfoldPrefixDir is foldPrefixDir's exact inverse: a tab-indented line is re-joined
// to the directory heading above it; a line ending in `/` that a tab-indented line
// follows IS that heading (and is consumed); everything else passes through.
func unfoldPrefixDir(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	cur, have := "", false
	for i, l := range lines {
		if strings.HasPrefix(l, "\t") && have {
			out = append(out, cur+"/"+l[1:])
			continue
		}
		if strings.HasSuffix(l, "/") && i+1 < len(lines) && strings.HasPrefix(lines[i+1], "\t") {
			cur, have = strings.TrimSuffix(l, "/"), true
			continue
		}
		have = false
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

func init() {
	components.RegisterFields("searchfold", searchfoldConfig{}, []components.Field{
		{Key: "min_tokens", Type: components.FieldInt, Default: 50, Min: 1,
			Hint: "Only fold search output estimated above this many tokens. Below it the fold cannot pay for the work."},
	})
}
