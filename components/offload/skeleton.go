//go:build cg_skeleton

// skeleton is the only cgo component (tree-sitter). It is gated behind the
// cg_skeleton build tag so the default build — and the AuthBridge plugin that
// embeds this module — stays pure-Go (CGO_ENABLED=0), static, and small. Build a
// coding-agent variant that includes it with: go build -tags cg_skeleton (and
// CGO_ENABLED=1). Without the tag, "skeleton" is simply not registered, so a
// config naming it fails at config.Build with a clear "unknown component" error.

package offload

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/treesitter"
	"github.com/rossoctl/context-guru/schema"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

func init() { components.Register("skeleton", newSkeleton) }

// Skeleton strips function/method bodies from fenced code blocks, keeping
// signatures/imports/types (after headroom's code-aware compressor). It drops
// information (the bodies), so it is an Offload: the whole original message is
// stashed and an expand marker left for recovery. Class bodies are preserved so
// method signatures survive.
//
// v1 targets fenced ```lang code blocks (where the language is explicit); file
// reads without a fence/path are a later addition.
type Skeleton struct {
	minTokens int
	mode      markerMode
}

type skeletonConfig struct {
	MinTokens  int    `yaml:"min_tokens"`
	MarkerMode string `yaml:"marker_mode"` // full only (see newSkeleton)
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
	return &Skeleton{minTokens: cfg.MinTokens, mode: markerFull}, nil
}

func (Skeleton) Name() string                 { return "skeleton" }
func (Skeleton) Enabled(*components.Ctx) bool { return true }

var fenceRe = regexp.MustCompile("(?s)```([A-Za-z0-9+#_-]*)\n(.*?)\n```")

// fenceLang maps a fenced code-block language token to a tree-sitter grammar.
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
		rep.Skipped = true
		return nil, nil
	}
	var keys []string
	emitted := 0
	for i := range req.Input {
		m := &req.Input[i]
		if m.Role != schemas.ChatMessageRoleTool {
			continue // only tool outputs, like every sibling offloader — never mangle the user's/assistant's own code
		}
		if !schema.Rewritable(*m) {
			continue // non-text blocks would be dropped by a text rewrite
		}
		content := schema.MessageText(*m)
		if content == "" || !strings.Contains(content, "```") {
			continue
		}
		if skipReduce(c, content) {
			continue // already carries a marker, or was expanded by the agent — don't re-reduce
		}
		matches := fenceRe.FindAllStringSubmatchIndex(content, -1)
		if matches == nil {
			continue
		}
		var out strings.Builder
		last, changed := 0, false
		for _, mt := range matches { // mt: full[0,1] lang[2,3] body[4,5]
			grammar := fenceLang[strings.ToLower(content[mt[2]:mt[3]])]
			body := content[mt[4]:mt[5]]
			if grammar == "" || schema.TextTokens(body) < s.minTokens {
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
			continue
		}
		out.WriteString(content[last:])
		body := out.String()
		newText, key, eff, ok := tryMark(c, s.mode, content, " [full source: call "+expand.ToolName+"]",
			func(tok string) string { return body + "\n" + tok })
		if !ok {
			continue // skeleton+marker wouldn't shrink this message; leave it verbatim
		}
		commitMark(c, rep, eff, key, content)
		schema.SetMessageText(m, newText)
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

// maxParseDepth bounds skeleton's tree-walk recursion over untrusted parse trees so
// pathologically nested input can't overflow the Go stack (an uncatchable crash).
const maxParseDepth = 5000

// skeletonize parses src and replaces function/method/constructor bodies with a
// placeholder, keeping everything else. Returns ok=false on parse failure or
// when there is nothing to elide (fail-open: caller leaves the block untouched).
func skeletonize(src []byte, grammar string) (string, bool) {
	tree, _, ok := treesitter.Parse(grammar, src)
	if !ok || tree == nil {
		return "", false
	}
	defer tree.Close()
	root := tree.RootNode()
	if root == nil {
		return "", false
	}
	var ranges [][2]uint
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
			ranges = append(ranges, [2]uint{n.StartByte(), n.EndByte()})
			return // don't recurse into an elided body (avoids nested double-elision)
		}
		for i := uint(0); i < n.ChildCount(); i++ {
			if ch := n.Child(i); ch != nil {
				walk(ch, kind, depth+1)
			}
		}
	}
	walk(root, "", 0)
	if len(ranges) == 0 {
		return "", false
	}
	var b strings.Builder
	last := uint(0)
	for _, r := range ranges {
		b.Write(src[last:r[0]])
		b.WriteString(placeholder(src[r[0]:r[1]]))
		last = r[1]
	}
	b.Write(src[last:])
	return b.String(), true
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

// placeholder replaces one elided body. Two properties matter more than brevity:
//
//   - It keeps the body's LINE COUNT (the elided newlines are re-emitted), so every
//     line after it still sits at its original line number. Otherwise a skeleton and
//     the file on disk disagree about positions, and an agent that read a line number
//     from a grep/stack trace and then edited "line 412" edits the wrong line. Runs of
//     newlines are near-free to tokenize; the marker-inclusive never-worse guard in
//     tryMark still drops the rewrite if they cost more than the body.
//   - It is a SYNTAX ERROR in every supported language, so a skeleton written back to
//     disk fails loudly instead of silently stubbing the file out. A bare "…" is not:
//     it is valid Python (Ellipsis), so `def f(): …` would compile and quietly turn an
//     elided body into an empty one. ⟪cg⟫ (expand's placeholder sentinel, so
//     expand.HasPlaceholder also recognizes an echoed block) cannot be an identifier
//     in any of them.
func placeholder(seg []byte) string {
	nl := strings.Repeat("\n", bytes.Count(seg, []byte{'\n'}))
	if len(seg) >= 2 && seg[0] == '{' && seg[len(seg)-1] == '}' {
		return "{ … }" + nl
	}
	return "… " + expand.SummaryMarker + nl
}

func init() {
	components.RegisterFields("skeleton", skeletonConfig{}, []components.Field{
		{Key: "min_tokens", Type: components.FieldInt, Default: 80, Min: 1,
			Hint: "Only skeletonize a code block above this many tokens."},
		// Not markerModeField(): skeleton REFUSES the irreversible modes at config time,
		// because an elided function body the agent cannot tell from an empty one gets
		// edited against a body that never existed. The key stays accepted so an existing
		// `marker_mode: full` document still loads.
		{Key: "marker_mode", Type: components.FieldEnum, Default: "full", Options: []string{"full"},
			Hint: "full only. summary and off leave no stash, which would make the one lossy component whose loss is dangerous permanently lossy."},
	})
}
