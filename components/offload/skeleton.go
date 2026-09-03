//go:build cg_skeleton

// skeleton is the only cgo component (tree-sitter). It is gated behind the
// cg_skeleton build tag so the default build — and the AuthBridge plugin that
// embeds this module — stays pure-Go (CGO_ENABLED=0), static, and small. Build a
// coding-agent variant that includes it with: go build -tags cg_skeleton (and
// CGO_ENABLED=1). Without the tag, "skeleton" is simply not registered, so a
// config naming it fails at config.Build with a clear "unknown component" error.
//
// It is LOCAL-ONLY and EVALUATION-ONLY: off by default, in no preset, zero
// production rows. See docs/components/skeleton.md for the measured numbers and
// the risk statement that is the reason it stays that way.

package offload

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/treesitter"
	"github.com/rossoctl/context-guru/schema"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

func init() { components.Register("skeleton", newSkeleton) }

// Skeleton replaces function/method bodies with signatures, keeping imports, type
// declarations, decorators, constants and every top-level line verbatim (after
// headroom's CodeAwareCompressor). It drops information (the bodies), so it is an
// Offload: the whole original message is stashed and an expand marker left for
// recovery. Class bodies are preserved so method signatures survive.
//
// Two input shapes, because they carry POSITION differently:
//
//   - A fenced ```lang block. Nothing else records where a line was, so the
//     placeholder re-emits the elided newlines and the block keeps its line COUNT.
//   - A line-numbered file dump — what a `Read` tool_result and `cat -n`/`sed -n`
//     actually look like, and 81-85% of interactive Claude Code tool tokens. Here the
//     `NNN\t` gutter carries the position of every surviving line, so an elided body
//     collapses to ONE gutter-prefixed line naming the range it replaced. Padding
//     would be pure cost: the numbers, not the physical line count, are the anchor.
//
// The producing command is recovered with schema.ToolCalls, so the grammar comes from
// the file's EXTENSION (Read.file_path, or the operand of a cat/head/tail/sed/nl
// command) rather than from a language token the dump does not carry.
type Skeleton struct {
	minTokens int
	mode      markerMode
	coldCache bool
}

type skeletonConfig struct {
	MinTokens  int    `yaml:"min_tokens"`
	MarkerMode string `yaml:"marker_mode"` // full only (see newSkeleton)
	ColdCache  bool   `yaml:"cold_cache"`
}

func newSkeleton(raw []byte) (components.Component, error) {
	cfg := skeletonConfig{MinTokens: 80}
	if err := components.Decode(raw, &cfg); err != nil {
		return nil, err
	}
	// Unlike every other Offload, skeleton refuses the irreversible marker modes.
	// What it drops is CODE BODIES from a file the agent is reading — and unlike a
	// masked command output, an unrecoverable body is not merely "information the
	// agent must re-fetch": the agent cannot tell an elided body from an empty one,
	// so it edits or rewrites the file against a body that never existed. summary
	// and off leave no stash, so they turn the one lossy component whose loss is
	// dangerous into a permanent one. Fail loudly at config time rather than run
	// unrecoverably; the key stays accepted so an existing `marker_mode: full`
	// config still loads (config.LoadBytes rejects unknown keys).
	if mode := parseMarkerMode(cfg.MarkerMode); mode != markerFull {
		return nil, fmt.Errorf("skeleton: marker_mode %q is unrecoverable (elided code bodies could never be restored); only \"full\" is supported", cfg.MarkerMode)
	}
	return &Skeleton{minTokens: cfg.MinTokens, mode: markerFull, coldCache: cfg.ColdCache}, nil
}

func (Skeleton) Name() string                 { return "skeleton" }
func (Skeleton) Enabled(*components.Ctx) bool { return true }

var fenceRe = regexp.MustCompile("(?s)```([A-Za-z0-9+#_-]*)\n(.*?)\n```")

// gutterRe matches the "   123\t" line-number prefix Claude Code's Read (and `cat -n`,
// `nl`, `sed -n | cat -n`) put in front of every line of a file dump.
var gutterRe = regexp.MustCompile(`^\s*\d+\t`)

// fenceLang maps a fenced code-block language token to a tree-sitter grammar. The
// dump path does not use it — there the grammar comes from the file extension via
// treesitter.LangForExt, which is the same table keyed the other way.
var fenceLang = map[string]string{
	"go": "go", "golang": "go", "py": "python", "python": "python",
	"js": "javascript", "javascript": "javascript", "jsx": "javascript",
	"ts": "typescript", "typescript": "typescript", "tsx": "tsx",
	"rs": "rust", "rust": "rust", "java": "java", "c": "c", "h": "c",
	"cpp": "cpp", "c++": "cpp", "cc": "cpp", "rb": "ruby", "ruby": "ruby",
	"php": "php", "cs": "c_sharp", "csharp": "c_sharp", "kt": "kotlin",
	"kotlin": "kotlin", "swift": "swift", "scala": "scala",
}

func (s *Skeleton) Offload(req *schemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) ([]string, error) {
	// No stash, no skeleton. effectiveMode degrades full to off when the store cannot
	// persist, which for every other Offload just means "this drop is irreversible" —
	// for skeleton it would mean permanently elided code bodies with nothing to expand
	// back. Decline the whole pass instead (the sibling offloaders still run).
	if effectiveMode(c, s.mode) != markerFull {
		rep.Gate("no_stash")
		rep.Skipped = true
		return nil, nil
	}
	pairs := schema.ToolCalls(req)
	newest := newestReads(req, pairs)
	var keys []string
	emitted := 0
	for i := range req.Input {
		m := &req.Input[i]
		if m.Role != schemas.ChatMessageRoleTool {
			continue // only tool outputs, like every sibling offloader — never mangle the user's/assistant's own code
		}
		if !schema.Rewritable(*m) {
			rep.Gate("non_text_blocks") // an image Read: a text rewrite would drop it
			continue
		}
		content := schema.MessageText(*m)
		if content == "" {
			continue
		}
		// NEVER touch the file the agent is most likely mid-edit on. The newest Read of
		// a path is the one the next Edit will be written against; an elided body there
		// is the failure this component must not have. (readlifecycle's stale/superseded
		// classes are the complementary half — it removes Reads this one protects.)
		//
		// This runs BEFORE the frozen replay on purpose. Two reads of a path can carry
		// IDENTICAL bytes, and the freeze is keyed by content hash, so replaying first
		// would elide the newest read because an older one had been elided. The order is
		// monotone: an index only ever loses "newest" status as later reads arrive, never
		// gains it, so the guard can never un-freeze something it already allowed.
		if tc, ok := pairs[i]; ok && newest[dumpPath(tc)] == i {
			rep.Gate("newest_read_of_path")
			continue
		}
		// Replay a frozen decision at ANY depth: the agent re-sends the original every
		// turn, so not re-eliding it would flip the message skeleton→full→skeleton and
		// churn the provider's KV cache. Same contract as mask/failed_run/readlifecycle.
		if fk, _, ok := reapplyFrozen(c, rep, s.Name(), m); ok {
			emitted++
			keys = append(keys, fk...)
			continue
		}
		if skipReduce(c, content) {
			rep.Gate("marker_or_kept_verbatim") // already carries a marker, or the agent expanded it
			continue
		}
		if schema.TextTokens(content) < s.minTokens {
			rep.Gate("below_min_tokens")
			continue
		}
		// A NEW elision only in the uncached tail (or at any depth on a provably cold
		// turn, opt-in), plus the lost-freeze repair — there the provider already holds
		// the elided bytes, so re-deriving them preserves the cache.
		if !c.TailOnlyCold(i, s.coldCache) && !repairLostFreeze(c, s.Name(), content) {
			rep.Gate("cached_prefix")
			continue
		}
		body, ok := s.reduce(content, pairs[i], rep)
		if !ok {
			continue
		}
		newText, key, eff, ok := tryMark(c, s.mode, content, " [full source: call "+expand.ToolName+"]",
			func(tok string) string { return body + "\n" + tok })
		if !ok {
			rep.Gate("marker_no_win") // skeleton+marker wouldn't shrink this message; leave it verbatim
			continue
		}
		if !commitMark(c, rep, eff, key, content) {
			continue // the store cannot back the marker; leave this message verbatim
		}
		schema.SetMessageText(m, newText)
		freeze(c, s.Name(), content, newText)
		emitted++
		if key != "" {
			keys = append(keys, key)
		}
	}
	if emitted == 0 {
		rep.Skipped = true
	}
	return keys, nil
}

// reduce produces the skeletonized body text for one tool output, trying the
// line-numbered file-dump shape first (the one real traffic is made of) and falling
// back to fenced ```lang blocks. ok=false means "leave this message verbatim".
func (s *Skeleton) reduce(content string, tc schema.ToolCall, rep *components.Report) (string, bool) {
	if grammar := dumpGrammar(tc); grammar != "" {
		out, ok := skeletonizeDump(content, grammar)
		if ok {
			return out, true
		}
		rep.Gate("dump_not_reducible")
		return "", false
	}
	if !strings.Contains(content, "```") {
		rep.Gate("not_a_code_dump")
		return "", false
	}
	return skeletonizeFenced(content, s.minTokens)
}

// skeletonizeFenced is the original v1 path: fenced ```lang blocks, where the language
// is explicit and nothing but the physical line count records position.
func skeletonizeFenced(content string, minTokens int) (string, bool) {
	matches := fenceRe.FindAllStringSubmatchIndex(content, -1)
	if matches == nil {
		return "", false
	}
	var out strings.Builder
	last, changed := 0, false
	for _, mt := range matches { // mt: full[0,1] lang[2,3] body[4,5]
		grammar := fenceLang[strings.ToLower(content[mt[2]:mt[3]])]
		body := content[mt[4]:mt[5]]
		if grammar == "" || schema.TextTokens(body) < minTokens {
			continue
		}
		skel, ok := skeletonize([]byte(body), grammar)
		if !ok || schema.TextTokens(skel) >= schema.TextTokens(body) {
			continue
		}
		out.WriteString(content[last:mt[4]])
		out.WriteString(skel)
		last = mt[5]
		changed = true
	}
	if !changed {
		return "", false
	}
	out.WriteString(content[last:])
	return out.String(), true
}

// --- which tool output is a source-file dump -------------------------------

// dumpProgs are the shell programs whose stdout IS the named file's bytes. Closed and
// short on purpose: a program we are not certain about produces text that is not the
// file, and the parse-clean guard would reject it anyway — this list only avoids
// wasting a parse. `sed` is included for the `sed -n '100,200p' file` window form.
var dumpProgs = map[string]bool{"cat": true, "head": true, "tail": true, "sed": true, "nl": true}

// dumpGrammar reports the tree-sitter grammar this tool result is a dump of, or "" when
// the call is not a file dump of a supported language. The grammar comes from the file
// EXTENSION, which is why the pairing matters: a raw Read output carries no language
// token at all, so without schema.ToolCalls there is nothing to pick a parser from.
func dumpGrammar(tc schema.ToolCall) string {
	if tc.Name == "Read" {
		return treesitter.LangForExt(dumpPath(tc))
	}
	if tc.Name != "Bash" {
		return "" // Grep/Glob/WebFetch output is not a file; never guess
	}
	cmd := tc.Command()
	fields := strings.Fields(cmd)
	if len(fields) == 0 || !dumpProgs[baseName(fields[0])] {
		return ""
	}
	// Exactly one operand with a supported extension, or we do not know what we are
	// looking at (`diff a.go b.go`, `cat a.go b.py`). One grammar or nothing.
	grammar := ""
	for _, f := range fields[1:] {
		g := treesitter.LangForExt(strings.Trim(f, `'"`))
		if g == "" {
			continue
		}
		if grammar != "" && grammar != g {
			return ""
		}
		grammar = g
	}
	return grammar
}

// dumpPath is the file a Read/Edit-shaped call names. It decodes the arguments here
// rather than through ToolCall.Command() because the path must come back exactly as
// the wire carried it — it is used as a map key that two turns have to agree on.
func dumpPath(tc schema.ToolCall) string {
	if tc.Args == "" || !strings.Contains(tc.Args, `"file_path"`) {
		return ""
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(tc.Args), &obj) != nil {
		return ""
	}
	var p string
	if json.Unmarshal(obj["file_path"], &p) != nil {
		return ""
	}
	return p
}

func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// newestReads maps a file path to the HIGHEST req.Input index at which it was Read.
// That message is the agent's current picture of the file and is never skeletonized.
// The map is only ever LOOKED UP by key, never iterated, so no Go map-order
// randomization can reach the output bytes.
func newestReads(req *schemas.BifrostChatRequest, pairs map[int]schema.ToolCall) map[string]int {
	out := map[string]int{}
	for i := range req.Input { // ascending index order, not map order
		tc, ok := pairs[i]
		if !ok || tc.Name != "Read" {
			continue
		}
		if p := dumpPath(tc); p != "" {
			out[p] = i
		}
	}
	return out
}

// --- the line-numbered file dump ------------------------------------------

// skeletonizeDump rewrites a `NNN\t`-guttered file dump. It strips the gutter to parse
// (tree-sitter would see every line as garbage otherwise) and PRESERVES the line
// numbers of every surviving line, because a signature the model cannot locate is not
// a usable skeleton. An elided body collapses to a single line, prefixed with the
// gutter of the line it starts on and naming the range it replaced:
//
//	41→func (a *Adder) Add(x, y int) (int, error) { … ⟪cg⟫ 12 lines elided: 41-52 }
//
// ok=false (leave verbatim) when: the text is not a numbered dump, the stripped code
// does not parse CLEANLY, there is no body to elide, or the elision would not re-parse.
func skeletonizeDump(raw, grammar string) (string, bool) {
	lines := strings.Split(raw, "\n")
	prefix := make([]string, len(lines))
	code := make([]string, len(lines))
	hits := 0
	for i, ln := range lines {
		if m := gutterRe.FindString(ln); m != "" {
			hits++
			prefix[i], code[i] = m, ln[len(m):]
		} else {
			code[i] = ln
		}
	}
	if hits*2 <= len(lines) {
		return "", false // not a numbered dump (a bare `cat` has no gutter at all)
	}
	src := []byte(strings.Join(code, "\n"))
	ranges, ok := safeBodyRanges(src, grammar)
	if !ok {
		return "", false
	}
	var b strings.Builder
	skip := -1
	for i, ln := range code {
		if i <= skip {
			continue // interior of a body already collapsed onto its opening line
		}
		r, elide := ranges.startingAt(uint(i))
		if !elide {
			b.WriteString(prefix[i])
			b.WriteString(ln)
			if i < len(code)-1 {
				b.WriteByte('\n')
			}
			continue
		}
		// Opening line kept verbatim up to the body, then the placeholder, then whatever
		// followed the body's last byte on its closing line (usually nothing).
		b.WriteString(prefix[i])
		b.WriteString(ln[:min(int(r.startCol), len(ln))])
		b.WriteString(dumpPlaceholder(src[r.startByte:r.endByte], firstLineNo(prefix[i]), firstLineNo(prefix[r.endRow])))
		if tail := code[r.endRow]; int(r.endCol) < len(tail) {
			b.WriteString(tail[r.endCol:])
		}
		if int(r.endRow) < len(code)-1 {
			b.WriteByte('\n')
		}
		skip = int(r.endRow)
	}
	return b.String(), true
}

// firstLineNo reads the number out of a gutter prefix ("   412\t" -> 412), or 0.
func firstLineNo(prefix string) int {
	n, err := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(prefix, "\t")))
	if err != nil {
		return 0
	}
	return n
}

// dumpPlaceholder is the one line that replaces an elided body in a numbered dump.
// Three properties, in order of importance:
//
//   - It states WHERE, using the file's own line numbers, so the model can still ask
//     for exactly that range (and an expand round-trip is a choice, not a necessity).
//   - It carries expand.SummaryMarker, so expand.HasPlaceholder recognizes an echoed
//     block and skipReduce never double-reduces it.
//   - It is a SYNTAX ERROR in every supported language, so a skeleton written back to
//     disk fails loudly instead of silently stubbing the file out. A bare "…" is not:
//     it is valid Python (Ellipsis), so `def f(): …` would compile and quietly turn an
//     elided body into an empty one.
func dumpPlaceholder(seg []byte, startLine, endLine int) string {
	n := bytes.Count(seg, []byte{'\n'}) + 1
	where := ""
	if startLine > 0 && endLine >= startLine {
		where = fmt.Sprintf(": %d-%d", startLine, endLine)
	}
	core := fmt.Sprintf("… %s %d lines elided%s", expand.SummaryMarker, n, where)
	if braced(seg) {
		return "{ " + core + " }"
	}
	return core
}

// --- parsing ---------------------------------------------------------------

// maxParseDepth bounds skeleton's tree-walk recursion over untrusted parse trees so
// pathologically nested input can't overflow the Go stack (an uncatchable crash).
const maxParseDepth = 5000

// bodyRange is one function/method/constructor body, in bytes (for splicing) and in
// rows/columns (for the line-oriented dump rewrite). Ranges are disjoint and strictly
// ascending: the walk never descends into a body it has already claimed.
type bodyRange struct {
	startByte, endByte uint
	startRow, endRow   uint
	startCol, endCol   uint
}

type bodyRanges []bodyRange

// startingAt returns the range whose body OPENS on row i. A range whose opening row is
// also the closing row of the previous one is not returned — two bodies sharing a
// physical line cannot both be collapsed onto it, so the second is left verbatim.
func (rs bodyRanges) startingAt(i uint) (bodyRange, bool) {
	for k, r := range rs {
		if r.startRow != i {
			continue
		}
		if k > 0 && rs[k-1].endRow >= r.startRow {
			return bodyRange{}, false
		}
		return r, true
	}
	return bodyRange{}, false
}

// safeBodyRanges is the whole risk argument in one function. It returns the bodies that
// may be elided, and ONLY when both parses are clean:
//
//  1. The INPUT must parse with no ERROR and no MISSING node. This is the load-bearing
//     guard: tree-sitter's error recovery happily reports a "block" in text that is not
//     the code it looks like (a partial `sed -n '100,200p'` window, a grep of a source
//     file, a diff), and eliding a mis-identified range would delete code that has
//     nothing to do with a function body. A file that does not parse is left alone.
//  2. The OUTPUT must parse too. The emitted placeholder is deliberately a syntax error
//     (see dumpPlaceholder), so what is re-parsed is the same elision rendered with a
//     language-valid EMPTY body — which is the structural question: "does replacing
//     these ranges with an empty body still yield the same well-formed file?"
//
// Same contract as headroom's CodeAwareCompressor (code_compressor.rs:1-40): re-parse,
// and on ERROR/MISSING return the original.
func safeBodyRanges(src []byte, grammar string) (bodyRanges, bool) {
	if !parseClean(grammar, src) {
		return nil, false
	}
	ranges, ok := bodyWalk(src, grammar)
	if !ok {
		return nil, false
	}
	neutral := splice(src, ranges, func(seg []byte) string { return neutralBody(seg, grammar) })
	if !parseClean(grammar, []byte(neutral)) {
		return nil, false
	}
	return ranges, true
}

// parseClean reports whether src parses under grammar with no ERROR and no MISSING
// node anywhere in the tree. Unknown grammar / nil tree counts as not clean (fail-open
// at every call site: the caller leaves the text verbatim).
func parseClean(grammar string, src []byte) bool {
	tree, _, ok := treesitter.Parse(grammar, src)
	if !ok || tree == nil {
		return false
	}
	defer tree.Close()
	root := tree.RootNode()
	// HasError is true when the node IS or CONTAINS an ERROR or a MISSING node, so one
	// call on the root covers both classes for the whole tree.
	return root != nil && !root.HasError()
}

// neutralBody renders an elided body as something the grammar still accepts, for the
// output re-parse only (never emitted). A braced body becomes `{}`. Python's
// indentation-delimited block becomes `pass`. For a grammar with no known empty-body
// form the ORIGINAL segment is returned, which makes guard 2 degenerate to guard 1
// there rather than reject everything — stated plainly because it is a real limit of
// the check, not a hidden one.
func neutralBody(seg []byte, grammar string) string {
	if braced(seg) {
		return "{}"
	}
	if grammar == "python" {
		return "pass"
	}
	return string(seg)
}

func braced(seg []byte) bool {
	return len(seg) >= 2 && seg[0] == '{' && seg[len(seg)-1] == '}'
}

// skeletonize is the fenced-block renderer: elide bodies, keeping the block's LINE
// COUNT (the elided newlines are re-emitted) because a fence carries no line numbers,
// so physical position is the only anchor an agent has. Returns ok=false on an unclean
// parse or when there is nothing to elide.
func skeletonize(src []byte, grammar string) (string, bool) {
	ranges, ok := safeBodyRanges(src, grammar)
	if !ok {
		return "", false
	}
	return splice(src, ranges, placeholder), true
}

// splice rebuilds src with each range replaced by ph(segment).
func splice(src []byte, ranges bodyRanges, ph func(seg []byte) string) string {
	var b strings.Builder
	last := uint(0)
	for _, r := range ranges {
		b.Write(src[last:r.startByte])
		b.WriteString(ph(src[r.startByte:r.endByte]))
		last = r.endByte
	}
	b.Write(src[last:])
	return b.String()
}

// bodyWalk collects the function/method/constructor bodies in src. ok=false when there
// is nothing to elide (fail-open: caller leaves the input untouched).
func bodyWalk(src []byte, grammar string) (bodyRanges, bool) {
	tree, _, ok := treesitter.Parse(grammar, src)
	if !ok || tree == nil {
		return nil, false
	}
	defer tree.Close()
	root := tree.RootNode()
	if root == nil {
		return nil, false
	}
	var ranges bodyRanges
	var walk func(n *sitter.Node, parentKind string, depth int)
	walk = func(n *sitter.Node, parentKind string, depth int) {
		// Bound recursion depth: the parse tree comes from untrusted tool-output text,
		// and a deeply nested input (thousands of nested blocks/parens) would overflow
		// the Go stack — a FATAL runtime throw that recover() cannot catch, crashing the
		// whole proxy. Past the limit we stop descending (fail-open: leave that subtree
		// un-skeletonized). maxParseDepth is far beyond any real source nesting.
		if depth > maxParseDepth {
			return
		}
		kind := n.Kind()
		if isBodyKind(kind) && isDeclKind(parentKind) {
			sp, ep := n.StartPosition(), n.EndPosition()
			ranges = append(ranges, bodyRange{
				startByte: n.StartByte(), endByte: n.EndByte(),
				startRow: sp.Row, endRow: ep.Row,
				startCol: sp.Column, endCol: ep.Column,
			})
			return // don't recurse into an elided body (avoids nested double-elision)
		}
		for i := uint(0); i < n.ChildCount(); i++ {
			if ch := n.Child(i); ch != nil {
				walk(ch, kind, depth+1)
			}
		}
	}
	// Children are visited in ascending order and a claimed body is never descended
	// into, so `ranges` comes out sorted and disjoint by construction — no sort, and
	// no map, on the path that produces output bytes. That is what makes the rewrite
	// byte-identical across runs and process restarts.
	walk(root, "", 0)
	if len(ranges) == 0 {
		return nil, false
	}
	return ranges, true
}

func isBodyKind(kind string) bool {
	switch kind {
	case "block", "statement_block", "compound_statement", "suite", "function_body":
		return true
	}
	return strings.Contains(kind, "body")
}

func isDeclKind(parentKind string) bool {
	return strings.Contains(parentKind, "function") ||
		strings.Contains(parentKind, "method") ||
		strings.Contains(parentKind, "constructor")
}

// placeholder replaces one elided body in a FENCED block, keeping the body's line
// count (see skeletonize) and staying a syntax error in every supported language (see
// dumpPlaceholder for why "…" alone is not).
func placeholder(seg []byte) string {
	nl := strings.Repeat("\n", bytes.Count(seg, []byte{'\n'}))
	if braced(seg) {
		return "{ … }" + nl
	}
	return "… " + expand.SummaryMarker + nl
}

func init() {
	components.RegisterFields("skeleton", skeletonConfig{}, []components.Field{
		{Key: "min_tokens", Type: components.FieldInt, Default: 80, Min: 1,
			Hint: "Only skeletonize a tool output above this many tokens."},
		// Not markerModeField(): skeleton REFUSES the irreversible modes at config time,
		// because an elided function body the agent cannot tell from an empty one gets
		// edited against a body that never existed. The key stays accepted so an existing
		// `marker_mode: full` document still loads.
		{Key: "marker_mode", Type: components.FieldEnum, Default: "full", Options: []string{"full"},
			Hint: "full only. summary and off leave no stash, which would make the one lossy component whose loss is dangerous permanently lossy."},
		coldCacheFieldDefault(false), // not a pure function of (content, config): see coldCacheDefault
	})
}
