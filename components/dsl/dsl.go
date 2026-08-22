// Package dsl is a declarative, user-extensible text-filter engine, adapted
// from rtk's TOML filter DSL (design D11). It lets command/log/tool-output
// shrinking be authored in YAML — no recompile — with the same 8-stage pipeline
// and Lossiness typing rtk proved out.
//
// Adaptation for the proxy world: rtk matches a shell command string; we match a
// per-message "selector" (the tool name, or the first line of content) since a
// proxy sees tool OUTPUTS, not the command that produced them. The content
// stages are unchanged.
//
// Pipeline order (each stage optional, applied in this exact order):
//  1. strip_ansi          2. replace[]        3. match_output[]+unless
//  4. strip/keep lines     5. truncate_lines_at 6. head/tail
//  7. max_lines           8. on_empty
//
// Filters are lossy (they drop lines), so the cmdfilter component that wraps
// this engine is an Offload: it stashes the original before applying a filter.
package dsl

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Lossiness records what a filter did to its input, so the caller can emit an
// accurate recovery hint (after rtk's Lossiness enum).
type Lossiness int

const (
	LossNone  Lossiness = iota // nothing dropped, or only reversible reformatting
	LossTail                   // a clean contiguous tail was dropped (tail -n +N recovers)
	LossWhole                  // non-contiguous / whole-blob loss; only full retrieval recovers
)

// Caps are the shared line budgets a filter picks by SIGNAL DENSITY (`cap: errors`)
// instead of hand-picking a max_lines per filter. The first four names and values
// are rtk's (src/core/truncate.rs); `buildlog` is ours — a build/plan transcript is
// mostly noise but the line that matters can sit anywhere in it, so it gets a
// deliberately generous budget. One map tunes the whole filter set.
var Caps = map[string]int{
	"errors":    20, // most actionable, shown the most
	"warnings":  10, // lower signal density than errors
	"list":      20, // flat lists (packages, services): one line per item
	"inventory": 50, // exhaustive lookups (installed packages, file listings)
	"buildlog":  80, // full build/plan transcripts: verbose, signal is positional
}

// ReducedCap is rtk's `reduced` deviation helper: a cap lowered for a more verbose
// data class, underflow-safe — a deviation can never empty the budget.
func ReducedCap(cap, by int) int {
	if by > 0 && by < cap {
		return cap - by
	}
	return cap
}

// Def is a raw filter definition (from YAML). All fields except Match are optional.
type Def struct {
	Description string `yaml:"description"`
	Match       string `yaml:"match"` // regex against the selector key
	// Family groups filters for per-family metrics (builds, tests, iac, pkg, net, ...).
	Family string `yaml:"family"`
	// Priority orders matching: higher first, then by name. Absent (0) = today's
	// behavior (name order). Use it to put a specific filter ahead of a generic one,
	// which matters more here than in rtk because we match on output shape.
	Priority int `yaml:"priority"`
	// Cap selects a shared budget class from Caps instead of a literal MaxLines;
	// CapReduce lowers it for an extra-verbose variant. MaxLines wins if both are set.
	Cap                string        `yaml:"cap"`
	CapReduce          int           `yaml:"cap_reduce"`
	StripANSI          bool          `yaml:"strip_ansi"`
	Replace            []ReplaceRule `yaml:"replace"`
	MatchOutput        []MatchRule   `yaml:"match_output"`
	StripLinesMatching []string      `yaml:"strip_lines_matching"`
	KeepLinesMatching  []string      `yaml:"keep_lines_matching"`
	TruncateLinesAt    *int          `yaml:"truncate_lines_at"`
	HeadLines          *int          `yaml:"head_lines"`
	TailLines          *int          `yaml:"tail_lines"`
	MaxLines           *int          `yaml:"max_lines"`
	OnEmpty            *string       `yaml:"on_empty"`
}

// ReplaceRule is a chained line-by-line regex substitution ($1 backrefs allowed).
type ReplaceRule struct {
	Pattern     string `yaml:"pattern"`
	Replacement string `yaml:"replacement"`
}

// MatchRule short-circuits: if Pattern matches the whole blob (and Unless does
// not), return Message immediately.
type MatchRule struct {
	Pattern string `yaml:"pattern"`
	Message string `yaml:"message"`
	Unless  string `yaml:"unless"`
}

// File is a filter document: a schema version + named filters + inline tests.
type File struct {
	SchemaVersion int                   `yaml:"schema_version"`
	Filters       map[string]Def        `yaml:"filters"`
	Tests         map[string][]TestCase `yaml:"tests"`
}

// TestCase is an inline filter test (input -> expected), run by RunTests.
type TestCase struct {
	Name     string `yaml:"name"`
	Input    string `yaml:"input"`
	Expected string `yaml:"expected"`
}

// Compiled is a filter with its regexes prebuilt.
type Compiled struct {
	Name       string
	match      *regexp.Regexp
	def        Def
	replace    []compiledReplace
	matchOut   []compiledMatch
	stripLines []*regexp.Regexp
	keepLines  []*regexp.Regexp
}

type compiledReplace struct {
	re   *regexp.Regexp
	repl string
}
type compiledMatch struct {
	re     *regexp.Regexp
	msg    string
	unless *regexp.Regexp
}

var ansiRe = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")

// Compile validates and precompiles a filter definition.
func Compile(name string, d Def) (*Compiled, error) {
	if d.Match == "" {
		return nil, fmt.Errorf("dsl: filter %q missing match", name)
	}
	if len(d.StripLinesMatching) > 0 && len(d.KeepLinesMatching) > 0 {
		return nil, fmt.Errorf("dsl: filter %q sets both strip_lines_matching and keep_lines_matching", name)
	}
	if d.Cap != "" {
		base, ok := Caps[d.Cap]
		if !ok {
			return nil, fmt.Errorf("dsl: filter %q unknown cap %q", name, d.Cap)
		}
		if d.MaxLines == nil { // an explicit max_lines still wins
			n := ReducedCap(base, d.CapReduce)
			d.MaxLines = &n
		}
	} else if d.CapReduce != 0 {
		return nil, fmt.Errorf("dsl: filter %q sets cap_reduce without cap", name)
	}
	// The selector spans a few leading lines, not one, so `^`/`$` in a match regex must
	// mean "start/end of A line" rather than "of the whole selector". Without (?m) a
	// filter anchored at ^ only matches output whose very FIRST line is its signature,
	// which is exactly the output-framing dependence a multi-line selector removes.
	m, err := regexp.Compile("(?m)" + d.Match)
	if err != nil {
		return nil, fmt.Errorf("dsl: filter %q match: %w", name, err)
	}
	c := &Compiled{Name: name, match: m, def: d}
	for i, r := range d.Replace {
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return nil, fmt.Errorf("dsl: filter %q replace[%d]: %w", name, i, err)
		}
		c.replace = append(c.replace, compiledReplace{re: re, repl: r.Replacement})
	}
	for i, r := range d.MatchOutput {
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return nil, fmt.Errorf("dsl: filter %q match_output[%d]: %w", name, i, err)
		}
		cm := compiledMatch{re: re, msg: r.Message}
		if r.Unless != "" {
			u, err := regexp.Compile(r.Unless)
			if err != nil {
				return nil, fmt.Errorf("dsl: filter %q match_output[%d].unless: %w", name, i, err)
			}
			cm.unless = u
		}
		c.matchOut = append(c.matchOut, cm)
	}
	for _, p := range d.StripLinesMatching {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("dsl: filter %q strip_lines_matching: %w", name, err)
		}
		c.stripLines = append(c.stripLines, re)
	}
	for _, p := range d.KeepLinesMatching {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("dsl: filter %q keep_lines_matching: %w", name, err)
		}
		c.keepLines = append(c.keepLines, re)
	}
	return c, nil
}

// Apply runs the 8-stage pipeline over input, returning the filtered output and
// what kind of loss occurred.
func Apply(c *Compiled, input string) (string, Lossiness) {
	lines := strings.Split(input, "\n")

	// 1. strip_ansi
	if c.def.StripANSI {
		for i := range lines {
			lines[i] = ansiRe.ReplaceAllString(lines[i], "")
		}
	}
	// 2. replace (chained, line-by-line)
	if len(c.replace) > 0 {
		for i := range lines {
			for _, r := range c.replace {
				lines[i] = r.re.ReplaceAllString(lines[i], r.repl)
			}
		}
	}
	// 3. match_output — first matching rule wins, unless the guard matches.
	if len(c.matchOut) > 0 {
		blob := strings.Join(lines, "\n")
		for _, m := range c.matchOut {
			if m.re.MatchString(blob) && (m.unless == nil || !m.unless.MatchString(blob)) {
				return m.msg, LossWhole
			}
		}
	}
	// 4. strip/keep lines (mutually exclusive)
	if len(c.stripLines) > 0 {
		lines = filterLines(lines, c.stripLines, false)
	} else if len(c.keepLines) > 0 {
		lines = filterLines(lines, c.keepLines, true)
	}
	// 5. truncate_lines_at (unicode-safe per-line cap). An intra-line cut is a REAL
	// loss and is non-contiguous by nature (every long line loses its own tail), so
	// it types as LossWhole; and it appends an ellipsis, because a silent mid-line
	// cut reads as corrupted output to a model (both after rtk).
	loss := LossNone
	if c.def.TruncateLinesAt != nil {
		n := *c.def.TruncateLinesAt
		for i, l := range lines {
			if t := TruncateRunes(l, n); t != l {
				lines[i] = t
				loss = LossWhole
			}
		}
	}

	// 6. head/tail
	if c.def.HeadLines != nil || c.def.TailLines != nil {
		var htLoss Lossiness
		lines, htLoss = headTail(lines, c.def.HeadLines, c.def.TailLines)
		if htLoss > loss { // keep the more severe of an intra-line cut and a line drop
			loss = htLoss
		}
	}
	// 7. max_lines (absolute cap, counts the omission marker)
	if c.def.MaxLines != nil && len(lines) > *c.def.MaxLines {
		n := *c.def.MaxLines
		omitted := len(lines) - n
		lines = append(lines[:n:n], fmt.Sprintf("... (%d lines truncated)", omitted))
		if loss == LossNone {
			loss = LossTail
		} else {
			loss = LossWhole // already lossy above: the cap is no longer a clean tail cut
		}
	}
	out := strings.Join(lines, "\n")
	// 8. on_empty
	if strings.TrimSpace(out) == "" && c.def.OnEmpty != nil {
		return *c.def.OnEmpty, LossNone
	}
	return out, loss
}

// TruncateRunes caps a line at n runes, marking the cut with an ellipsis that fits
// INSIDE the budget (so the result is never longer than n). Ported from rtk's
// utils::truncate.
//
// Exported because the generic per-line cap in the `linecap` component applies the same
// rule outside this package, and a second copy of "the ellipsis has to fit inside the
// budget" is the kind of subtlety that drifts.
func TruncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n < 3 {
		return "..."
	}
	return string(r[:n-3]) + "..."
}

func filterLines(lines []string, res []*regexp.Regexp, keep bool) []string {
	out := lines[:0:0]
	for _, l := range lines {
		matched := false
		for _, re := range res {
			if re.MatchString(l) {
				matched = true
				break
			}
		}
		if matched == keep {
			out = append(out, l)
		}
	}
	return out
}

// headTail keeps head+tail lines with an omission marker between; returns the
// loss kind (LossTail when a clean contiguous tail was dropped, else LossWhole).
func headTail(lines []string, head, tail *int) ([]string, Lossiness) {
	total := len(lines)
	switch {
	case head != nil && tail != nil:
		if total <= *head+*tail {
			return lines, LossNone
		}
		omitted := total - *head - *tail
		out := append([]string{}, lines[:*head]...)
		out = append(out, fmt.Sprintf("... (%d lines omitted)", omitted))
		out = append(out, lines[total-*tail:]...)
		return out, LossWhole // non-contiguous drop
	case head != nil:
		if total <= *head {
			return lines, LossNone
		}
		out := append([]string{}, lines[:*head]...)
		out = append(out, fmt.Sprintf("... (%d lines omitted)", total-*head))
		return out, LossTail // clean tail drop
	case tail != nil:
		if total <= *tail {
			return lines, LossNone
		}
		out := []string{fmt.Sprintf("... (%d lines omitted)", total-*tail)}
		out = append(out, lines[total-*tail:]...)
		return out, LossWhole
	}
	return lines, LossNone
}

// Family is the filter's command family, used for per-family metrics. "" when unset.
func (c *Compiled) Family() string {
	if c.def.Family == "" {
		return "other"
	}
	return c.def.Family
}

// Registry holds compiled filters, matched by descending priority then by name for
// determinism (specific-before-generic without relying on alphabetical luck).
type Registry struct {
	filters []*Compiled
	names   map[string]struct{}
	// gate is the alternation of every loaded filter's match pattern: one RE2 pass that
	// answers "could ANY filter match this key". See Match.
	gate *regexp.Regexp
}

// Load parses a YAML filter document and appends its filters to the registry.
// schema_version must be 1. Duplicate filter names are rejected (a silently
// shadowed filter is a debugging trap — rtk's build.rs rejects them too) and any
// inline tests the document carries must pass, so a broken filter fails loudly at
// load instead of quietly mangling output. (Requiring a test to EXIST is enforced
// for the shipped builtins by a unit test, not here — user configs stay free.)
func (r *Registry) Load(b []byte) error {
	var f File
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return fmt.Errorf("dsl: %w", err)
	}
	if f.SchemaVersion != 1 {
		return fmt.Errorf("dsl: unsupported schema_version %d (want 1)", f.SchemaVersion)
	}
	names := make([]string, 0, len(f.Filters))
	for n := range f.Filters {
		names = append(names, n)
	}
	sort.Strings(names)
	if r.names == nil {
		r.names = map[string]struct{}{}
	}
	for _, n := range names {
		if _, dup := r.names[n]; dup {
			return fmt.Errorf("dsl: duplicate filter name %q", n)
		}
		c, err := Compile(n, f.Filters[n])
		if err != nil {
			return err
		}
		for _, tc := range f.Tests[n] {
			if got, _ := Apply(c, tc.Input); !sameText(got, tc.Expected) {
				return fmt.Errorf("dsl: filter %q test %q failed: got %q want %q", n, tc.Name, got, tc.Expected)
			}
		}
		r.names[n] = struct{}{}
		r.filters = append(r.filters, c)
	}
	sort.SliceStable(r.filters, func(i, j int) bool {
		if r.filters[i].def.Priority != r.filters[j].def.Priority {
			return r.filters[i].def.Priority > r.filters[j].def.Priority
		}
		return r.filters[i].Name < r.filters[j].Name
	})
	r.buildGate()
	return nil
}

// buildGate compiles the union of every filter's match pattern into ONE regex, so the
// common case — no filter matches — costs a single RE2 pass instead of one per filter.
//
// The registry is a linear scan of every match pattern, and on real traffic it MISSES for
// 21 of the ~43 candidate messages per request: those 21 must each fail all 26 patterns
// before the miss can be reported, which is 550 full-registry evaluations per request. RE2
// compiles an alternation into a single automaton, so the gate answers the same question
// in one pass (rtk does this with regex::RegexSet).
//
// Priority order is untouched: a gate HIT still runs the ordered scan to find WHICH filter
// matched, so the winner is exactly the one the pre-gate code would have picked. If the
// union fails to compile (a pattern with a construct that does not survive grouping) the
// gate is left nil and Match falls back to the plain scan — same answers, old speed.
func (r *Registry) buildGate() {
	r.gate = nil
	if len(r.filters) == 0 {
		return
	}
	pats := make([]string, 0, len(r.filters))
	for _, c := range r.filters {
		// An empty pattern matches everything, so a registry containing one can never be
		// gated out — skip the gate entirely rather than build one that always hits.
		if c.def.Match == "" {
			return
		}
		// (?m:...), matching Compile: the selector spans several lines, so `^`/`$` in a
		// filter pattern mean start/end of A LINE. A gate built without the flag treats
		// them as start/end of the whole key and misses every filter anchored at `^`
		// whenever the key carries a `$ <command>` prefix line — a silent under-match,
		// which is the one failure mode a pre-gate must not have. The per-pattern scope
		// also keeps a pattern's own inline flags from leaking into its neighbours.
		pats = append(pats, "(?m:"+c.def.Match+")")
	}
	gate, err := regexp.Compile(strings.Join(pats, "|"))
	if err != nil {
		return // fail open: no gate, plain scan
	}
	r.gate = gate
}

// Match returns the first filter whose match regex matches key, or nil.
//
// The union gate short-circuits the miss (see buildGate); on a hit the ordered scan runs
// exactly as before, so priority still decides the winner.
func (r *Registry) Match(key string) *Compiled {
	if r.gate != nil && !r.gate.MatchString(key) {
		return nil
	}
	for _, c := range r.filters {
		if c.match.MatchString(key) {
			return c
		}
	}
	return nil
}

func sameText(got, want string) bool {
	return strings.TrimRight(got, "\n") == strings.TrimRight(want, "\n")
}

// Len reports how many filters are loaded.
func (r *Registry) Len() int { return len(r.filters) }

// RunTests compiles and runs the inline [tests] in a filter document, returning
// the names of failing cases (empty = all pass). Powers a `verify` command.
func RunTests(b []byte) (failures []string, err error) {
	var f File
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	for name, def := range f.Filters {
		c, err := Compile(name, def)
		if err != nil {
			return nil, err
		}
		for _, tc := range f.Tests[name] {
			got, _ := Apply(c, tc.Input)
			if !sameText(got, tc.Expected) {
				failures = append(failures, name+"/"+tc.Name)
			}
		}
	}
	sort.Strings(failures)
	return failures, nil
}
