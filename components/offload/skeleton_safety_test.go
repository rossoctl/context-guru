//go:build cg_skeleton

// Safety tests for the skeleton component. Skeleton is the only component whose
// loss is DANGEROUS rather than merely inconvenient — it removes code bodies from a
// file the agent is reading, and an agent cannot tell an elided body from an empty
// one. These tests pin the three properties that keep that survivable: the drop is
// always recoverable byte-for-byte, positions stay valid, and identifiers stay
// verbatim. They are the reason the component can be evaluated at all; they are not
// an argument for enabling it (see docs/components/skeleton.md).

package offload

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// goFixture is a fenced Go block with bodies well over min_tokens and a distinctive
// error string inside one of them.
const goFixture = "reading main.go\n```go\n" +
	"package main\n" +
	"\n" +
	"import (\n\t\"errors\"\n\t\"fmt\"\n)\n" +
	"\n" +
	"var ErrNope = errors.New(\"sentinel error string\")\n" +
	"\n" +
	"type Adder struct{ Base int }\n" +
	"\n" +
	"func (a *Adder) Add(x, y int) (int, error) {\n" +
	"\tsum := a.Base\n" +
	"\tsum += x\n" +
	"\tsum += y\n" +
	"\tfor i := 0; i < 10; i++ {\n\t\tsum += i\n\t}\n" +
	"\tif sum < 0 {\n\t\treturn 0, fmt.Errorf(\"negative total: %w\", ErrNope)\n\t}\n" +
	"\treturn sum, nil\n" +
	"}\n" +
	"\n" +
	"func Mul(a, b int) int {\n" +
	"\tp := 0\n" +
	"\tfor i := 0; i < b; i++ {\n\t\tp += a\n\t}\n" +
	"\tp += 0\n\tp += 0\n\tp += 0\n" +
	"\treturn p\n" +
	"}\n" +
	"```\n"

func skeletonFor(t *testing.T, yaml string) *Skeleton {
	t.Helper()
	comp, err := newSkeleton([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	return comp.(*Skeleton)
}

// runSkeleton offloads one tool message and returns (rewritten, report, store).
func runSkeleton(t *testing.T, s *Skeleton, st store.Store, content string) (string, *components.Report) {
	t.Helper()
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{tool(content)}}
	rep := &components.Report{}
	if _, err := s.Offload(req, rep, &components.Ctx{Session: "s", Store: st}); err != nil {
		t.Fatal(err)
	}
	return schema.MessageText(req.Input[0]), rep
}

// The single most important property: whatever skeleton elides must come back
// BYTE-FOR-BYTE through the stash the marker names. Not "contains the body" —
// identical, because the agent will edit against those bytes.
func TestSkeletonExpandRestoresOriginalByteForByte(t *testing.T) {
	st := store.NewMemory(store.Options{})
	got, rep := runSkeleton(t, skeletonFor(t, "min_tokens: 5\n"), st, goFixture)
	if rep.Skipped || got == goFixture {
		t.Fatalf("expected a skeletonized rewrite, got skipped=%v text=%q", rep.Skipped, got)
	}
	if rep.Irreversible {
		t.Fatal("skeleton must never record an irreversible drop")
	}
	keys := expand.ParseMarkers(got)
	if len(keys) != 1 {
		t.Fatalf("expected exactly one resolvable marker, got %d in %q", len(keys), got)
	}
	orig, ok := expand.Resolve(st, keys[0])
	if !ok {
		t.Fatal("marker did not resolve — the elision would be unrecoverable")
	}
	if orig != goFixture {
		t.Fatalf("round-trip is not byte-for-byte:\n want %q\n  got %q", goFixture, orig)
	}
}

// Positions must survive. An agent that reads a line number (grep hit, stack frame,
// compiler error) and then edits that line must not be pointed at a different line
// because a body above it collapsed. So the skeleton keeps the file's line count.
func TestSkeletonPreservesLineNumbers(t *testing.T) {
	got, _ := runSkeleton(t, skeletonFor(t, "min_tokens: 5\n"), store.NewMemory(store.Options{}), goFixture)
	// The appended marker adds trailing lines; compare the fenced block only.
	block := func(s string) []string {
		i := strings.Index(s, "```go\n")
		j := strings.Index(s[i+6:], "```")
		return strings.Split(s[i+6:i+6+j], "\n")
	}
	before, after := block(goFixture), block(got)
	if len(before) != len(after) {
		t.Fatalf("line count changed: %d -> %d\n%s", len(before), len(after), got)
	}
	// Every line that was not part of an elided body must be byte-identical, at the
	// same index — that is what makes a preserved line number meaningful.
	for i, ln := range before {
		if strings.Contains(ln, "func ") || strings.HasPrefix(ln, "type ") ||
			strings.HasPrefix(ln, "var ") || strings.HasPrefix(ln, "import") ||
			strings.HasPrefix(ln, "package") {
			if after[i] != ln && !strings.HasPrefix(after[i], ln) {
				t.Fatalf("line %d moved or changed: want %q got %q", i, ln, after[i])
			}
		}
	}
}

// Signatures, imports, types and top-level error strings stay verbatim; only bodies go.
func TestSkeletonKeepsSignaturesAndDropsBodies(t *testing.T) {
	got, _ := runSkeleton(t, skeletonFor(t, "min_tokens: 5\n"), store.NewMemory(store.Options{}), goFixture)
	for _, want := range []string{
		"package main", "\"errors\"", "type Adder struct{ Base int }",
		"var ErrNope = errors.New(\"sentinel error string\")",
		"func (a *Adder) Add(x, y int) (int, error)", "func Mul(a, b int) int",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("skeleton lost verbatim content %q:\n%s", want, got)
		}
	}
	for _, gone := range []string{"sum += x", "p += a"} {
		if strings.Contains(got, gone) {
			t.Fatalf("body statement %q survived: %s", gone, got)
		}
	}
	// An error string INSIDE an elided body does not survive — that is a real loss,
	// pinned here so the doc's risk table cannot drift from the behaviour.
	if strings.Contains(got, "negative total") {
		t.Fatal("unexpected: in-body string literal survived; update the risk table")
	}
}

// A Python body elided to a bare "…" would be VALID Python (Ellipsis), so an agent
// that wrote the skeleton back to disk would silently ship stubbed functions. The
// placeholder must be a syntax error instead.
func TestSkeletonPythonPlaceholderIsNotValidPython(t *testing.T) {
	body := ""
	for i := 0; i < 12; i++ {
		body += "    data = data.strip().lower().replace(\"a\", \"b\")\n"
	}
	src := "```python\n" +
		"import os\n\n" +
		"def load(path):\n" +
		"    data = open(path).read()\n" + body +
		"    return os.path.basename(path), data\n" +
		"```\n"
	got, rep := runSkeleton(t, skeletonFor(t, "min_tokens: 5\n"), store.NewMemory(store.Options{}), src)
	if rep.Skipped {
		t.Fatalf("python body should have been elided:\n%s", got)
	}
	if !strings.Contains(got, expand.SummaryMarker) {
		t.Fatalf("python elision must carry a syntax-error sentinel, got:\n%s", got)
	}
	if strings.Contains(got, "def load(path):\n    …\n") {
		t.Fatalf("bare ellipsis body is valid Python and would stub the file silently:\n%s", got)
	}
}

// With a store that cannot persist, tryMark would degrade full to off — for skeleton
// that means permanently elided bodies with nothing to expand back. It must decline.
func TestSkeletonDeclinesWhenStoreCannotPersist(t *testing.T) {
	got, rep := runSkeleton(t, skeletonFor(t, "min_tokens: 5\n"), store.Nop{}, goFixture)
	if got != goFixture {
		t.Fatalf("skeleton elided bodies with no stash available:\n%s", got)
	}
	if !rep.Skipped {
		t.Fatal("declining pass must report Skipped")
	}
}

// The irreversible marker modes are refused at config time, not silently honoured.
func TestSkeletonRejectsIrreversibleMarkerModes(t *testing.T) {
	for _, mode := range []string{"summary", "off"} {
		if _, err := newSkeleton([]byte("marker_mode: " + mode + "\n")); err == nil {
			t.Fatalf("marker_mode %q must be rejected", mode)
		}
	}
	if _, err := newSkeleton([]byte("marker_mode: full\n")); err != nil {
		t.Fatalf("marker_mode full must load: %v", err)
	}
}

// Re-sending the same output on a later turn must reduce to the SAME bytes, or the
// message flips inside the provider's cached prefix and forces a cache write.
func TestSkeletonIsDeterministicAcrossTurns(t *testing.T) {
	st := store.NewMemory(store.Options{})
	s := skeletonFor(t, "min_tokens: 5\n")
	first, _ := runSkeleton(t, s, st, goFixture)
	for turn := 0; turn < 3; turn++ {
		again, _ := runSkeleton(t, s, st, goFixture)
		if again != first {
			t.Fatalf("turn %d produced different bytes:\n%q\n%q", turn, first, again)
		}
	}
	// And its own output is never re-reduced (it carries a marker).
	twice, rep := runSkeleton(t, s, st, first)
	if !rep.Skipped || twice != first {
		t.Fatalf("skeleton re-reduced its own output: skipped=%v %q", rep.Skipped, twice)
	}
}

// Unparseable / unknown-language / already-small blocks are left exactly alone.
func TestSkeletonFailsOpen(t *testing.T) {
	for name, in := range map[string]string{
		"unknown language": "```bash\n" + strings.Repeat("echo hello world\n", 40) + "```\n",
		"no fence":         strings.Repeat("func Add(a, b int) int { return a + b }\n", 40),
		"broken go":        "```go\nfunc (((\n" + strings.Repeat("!!!\n", 40) + "```\n",
	} {
		got, rep := runSkeleton(t, skeletonFor(t, "min_tokens: 5\n"), store.NewMemory(store.Options{}), in)
		if got != in || !rep.Skipped {
			t.Fatalf("%s: expected untouched fail-open, got skipped=%v %q", name, rep.Skipped, got)
		}
	}
}

// --- the line-numbered file-dump path -------------------------------------
//
// A Read tool_result is a raw dump with an "NNN\t" gutter, not a fenced block. These
// tests cover the path that actually reaches it, and the guards that make a LOSSY
// rewrite of the agent's working file survivable.

// numberedRead renders src with the "   N\t" gutter Claude Code's Read puts on it,
// starting at line 1.
func numberedRead(src string) string {
	lines := strings.Split(strings.TrimSuffix(src, "\n"), "\n")
	var b strings.Builder
	for i, ln := range lines {
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, ln)
	}
	return b.String()
}

// goSource is a plausible Go file: imports, a const, a type, two methods with bodies.
const goSource = `package widget

import (
	"errors"
	"fmt"
)

const DefaultBase = 7

var ErrNegative = errors.New("negative total")

// Adder accumulates into Base.
type Adder struct {
	Base int
	Name string
}

func (a *Adder) Add(x, y int) (int, error) {
	sum := a.Base
	sum += x
	sum += y
	for i := 0; i < 10; i++ {
		sum += i
	}
	if sum < 0 {
		return 0, fmt.Errorf("total %d: %w", sum, ErrNegative)
	}
	return sum, nil
}

func Mul(a, b int) int {
	p := 0
	for i := 0; i < b; i++ {
		p += a
	}
	p += 0
	p += 0
	return p
}
`

// readReq builds the request shape a real Read produces: an assistant tool_use naming
// the path, then the tool_result carrying the guttered dump. Extra dumps of the SAME
// path are appended so the newest-Read guard has something older to act on.
func readReq(path string, bodies ...string) *bschemas.BifrostChatRequest {
	req := &bschemas.BifrostChatRequest{}
	for i, body := range bodies {
		id := fmt.Sprintf("t%d", i)
		args := fmt.Sprintf(`{"file_path":%q}`, path)
		name := "Read"
		tc := bschemas.ChatAssistantMessageToolCall{
			ID:       &id,
			Function: bschemas.ChatAssistantMessageToolCallFunction{Name: &name, Arguments: args},
		}
		req.Input = append(req.Input, bschemas.ChatMessage{
			Role:                 bschemas.ChatMessageRoleAssistant,
			ChatAssistantMessage: &bschemas.ChatAssistantMessage{ToolCalls: []bschemas.ChatAssistantMessageToolCall{tc}},
		})
		m := tool(body)
		m.ChatToolMessage = &bschemas.ChatToolMessage{ToolCallID: &id}
		req.Input = append(req.Input, m)
	}
	return req
}

// runReq offloads a whole request and returns the rewritten text of message idx.
func runReq(t *testing.T, s *Skeleton, st store.Store, req *bschemas.BifrostChatRequest) (*components.Report, []string) {
	t.Helper()
	rep := &components.Report{}
	keys, err := s.Offload(req, rep, &components.Ctx{Session: "s", Store: st})
	if err != nil {
		t.Fatal(err)
	}
	return rep, keys
}

// The headline capability: a raw, unfenced, line-numbered Read is reached at all.
// Signatures, imports, the const, the type and its fields stay verbatim; only bodies go.
func TestSkeletonReducesNumberedRead(t *testing.T) {
	dump := numberedRead(goSource)
	req := readReq("/w/adder.go", dump, "unrelated newest read\n")
	_, _ = runReq(t, skeletonFor(t, "min_tokens: 5\n"), store.NewMemory(store.Options{}), req)
	got := schema.MessageText(req.Input[1])
	if got == dump {
		t.Fatalf("numbered Read was not reduced:\n%s", got)
	}
	for _, want := range []string{
		"package widget", `"errors"`, "const DefaultBase = 7",
		`var ErrNegative = errors.New("negative total")`,
		"// Adder accumulates into Base.", "type Adder struct {", "Base int", "Name string",
		"func (a *Adder) Add(x, y int) (int, error)", "func Mul(a, b int) int",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("lost verbatim content %q:\n%s", want, got)
		}
	}
	for _, gone := range []string{"sum += x", "p += a", "total %d"} {
		if strings.Contains(got, gone) {
			t.Fatalf("body content %q survived:\n%s", gone, got)
		}
	}
	if schema.TextTokens(got) >= schema.TextTokens(dump) {
		t.Fatalf("skeleton did not shrink the dump: %d -> %d", schema.TextTokens(dump), schema.TextTokens(got))
	}
}

// A skeleton the model cannot LOCATE is not usable. Every surviving line must keep its
// original line number in the gutter, and the placeholder must name the range it ate.
func TestSkeletonDumpPreservesLineNumbers(t *testing.T) {
	dump := numberedRead(goSource)
	req := readReq("/w/adder.go", dump, "newest\n")
	runReq(t, skeletonFor(t, "min_tokens: 5\n"), store.NewMemory(store.Options{}), req)
	got := schema.MessageText(req.Input[1])
	// The truth table is number -> original text, keyed by the NUMBER (line texts like
	// "}" repeat, so keying by text would compare against the wrong line).
	orig := map[string]string{}
	for _, ln := range strings.Split(dump, "\n") {
		if m := gutterRe.FindString(ln); m != "" {
			orig[strings.TrimSpace(strings.TrimSuffix(m, "\t"))] = ln[len(m):]
		}
	}
	seen := 0
	for _, ln := range strings.Split(got, "\n") {
		m := gutterRe.FindString(ln)
		if m == "" {
			continue
		}
		num, text := strings.TrimSpace(strings.TrimSuffix(m, "\t")), ln[len(m):]
		want, ok := orig[num]
		if !ok {
			t.Fatalf("emitted line number %s was not in the original dump:\n%s", num, got)
		}
		// Either the line is verbatim, or it is a signature line whose body collapsed —
		// in which case the original line is still a PREFIX of what was emitted.
		if text != want && !strings.HasPrefix(text, want) {
			t.Fatalf("line %s is neither verbatim nor a signature+placeholder:\n want %q\n  got %q", num, want, text)
		}
		seen++
	}
	if seen < 10 {
		t.Fatalf("only %d numbered lines survived; the gutter is not being preserved:\n%s", seen, got)
	}
	// And the elision says which lines it replaced, so the model can re-read exactly them.
	if !regexp.MustCompile(`\d+ lines elided: \d+-\d+`).MatchString(got) {
		t.Fatalf("placeholder does not name the elided line range:\n%s", got)
	}
}

// Round-trip through the COMPONENT (not the stash layer): the marker skeleton leaves
// must resolve to the original dump byte-for-byte, because the agent will edit against
// those bytes.
func TestSkeletonDumpRoundTripsThroughComponent(t *testing.T) {
	st := store.NewMemory(store.Options{})
	dump := numberedRead(goSource)
	req := readReq("/w/adder.go", dump, "newest\n")
	rep, keys := runReq(t, skeletonFor(t, "min_tokens: 5\n"), st, req)
	if rep.Skipped || rep.Irreversible || len(keys) != 1 {
		t.Fatalf("expected one recoverable elision: skipped=%v irreversible=%v keys=%v", rep.Skipped, rep.Irreversible, keys)
	}
	got := schema.MessageText(req.Input[1])
	marks := expand.ParseMarkers(got)
	if len(marks) != 1 {
		t.Fatalf("expected exactly one resolvable marker, got %d in:\n%s", len(marks), got)
	}
	orig, ok := expand.Resolve(st, marks[0])
	if !ok {
		t.Fatal("marker did not resolve — the elision would be unrecoverable")
	}
	if orig != dump {
		t.Fatalf("round-trip is not byte-for-byte:\n want %q\n  got %q", dump, orig)
	}
	// And after the agent expands it, it is never re-reduced (the bounce loop).
	MarkKeptVerbatim(st, orig)
	again := readReq("/w/adder.go", dump, "newest\n")
	rep2, _ := runReq(t, skeletonFor(t, "min_tokens: 5\n"), st, again)
	if !rep2.Skipped || schema.MessageText(again.Input[1]) != dump {
		t.Fatalf("re-reduced content the agent had expanded: skipped=%v", rep2.Skipped)
	}
}

// The file the agent is mid-edit on. The NEWEST Read of a path is the agent's current
// picture of it and must never be skeletonized, however big it is.
func TestSkeletonNeverTouchesNewestRead(t *testing.T) {
	dump := numberedRead(goSource)
	req := readReq("/w/adder.go", dump, dump) // same path read twice
	runReq(t, skeletonFor(t, "min_tokens: 5\n"), store.NewMemory(store.Options{}), req)
	if schema.MessageText(req.Input[3]) != dump {
		t.Fatalf("newest Read of the path was skeletonized:\n%s", schema.MessageText(req.Input[3]))
	}
	if schema.MessageText(req.Input[1]) == dump {
		t.Fatal("the OLDER read of the same path should have been reduced")
	}
	// A single read of a path is by definition the newest one: left completely alone.
	only := readReq("/w/adder.go", dump)
	rep, _ := runReq(t, skeletonFor(t, "min_tokens: 5\n"), store.NewMemory(store.Options{}), only)
	if !rep.Skipped || schema.MessageText(only.Input[1]) != dump {
		t.Fatalf("the only Read of a path must be untouched: skipped=%v", rep.Skipped)
	}
}

// A dump that does not parse CLEANLY is left verbatim. This is the guard that makes the
// loose "which command produced this" heuristic safe: a partial window, a grep of a
// source file, or a truncated read all fail here rather than having a mis-identified
// "block" deleted out of them.
func TestSkeletonRejectsUncleanParse(t *testing.T) {
	cases := map[string]string{
		// `sed -n '18,24p' adder.go`: a real window into a real file, but not a file.
		"partial window": strings.Join(strings.Split(numberedRead(goSource), "\n")[17:24], "\n") + "\n",
		"truncated":      numberedRead(goSource[:len(goSource)-40]),
		"grep output":    numberedRead("adder.go:19:\tsum := a.Base\nadder.go:20:\tsum += x\n" + strings.Repeat("adder.go:21:\tsum += y\n", 30)),
	}
	for name, dump := range cases {
		req := readReq("/w/adder.go", dump, "newest\n")
		runReq(t, skeletonFor(t, "min_tokens: 5\n"), store.NewMemory(store.Options{}), req)
		if got := schema.MessageText(req.Input[1]); got != dump {
			t.Fatalf("%s: elided from text that does not parse cleanly:\n%s", name, got)
		}
	}
}

// parseClean is the guard itself: it must reject an ERROR node and a MISSING node, not
// merely a parser that refused to run.
func TestParseCleanRejectsErrorAndMissing(t *testing.T) {
	if !parseClean("go", []byte(goSource)) {
		t.Fatal("a well-formed Go file must parse clean")
	}
	for name, src := range map[string]string{
		"error node":      "package p\nfunc ((( !!! }\n",
		"missing brace":   "package p\nfunc f() int {\n\treturn 1\n",
		"unknown grammar": goSource, // via the grammar argument below
	} {
		grammar := "go"
		if name == "unknown grammar" {
			grammar = "brainfuck"
		}
		if parseClean(grammar, []byte(src)) {
			t.Fatalf("%s: parseClean must reject", name)
		}
	}
}

// Determinism across process restarts. Go map iteration is randomized, so a rewrite
// that touched a map on its output path would differ run to run — which would both
// re-anchor the provider's prompt cache every turn AND change what content is lost.
// Re-running the whole component in a fresh subprocess must produce the same bytes.
func TestSkeletonDumpDeterministicAcrossProcesses(t *testing.T) {
	dump := numberedRead(goSource)
	render := func() string {
		req := readReq("/w/adder.go", dump, "newest\n")
		runReq(t, skeletonFor(t, "min_tokens: 5\n"), store.NewMemory(store.Options{}), req)
		return schema.MessageText(req.Input[1])
	}
	first := render()
	for i := 0; i < 50; i++ { // 50 fresh renders inside this process
		if got := render(); got != first {
			t.Fatalf("iteration %d differs:\n%q\n%q", i, first, got)
		}
	}
	if os.Getenv("CG_SKEL_CHILD") == "1" {
		fmt.Println("RENDER:" + strconv.Quote(first)) // the child reports; the parent compares
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run", "^"+t.Name()+"$")
	cmd.Env = append(os.Environ(), "CG_SKEL_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child process: %v\n%s", err, out)
	}
	for _, ln := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(ln, "RENDER:") {
			continue
		}
		got, uerr := strconv.Unquote(strings.TrimPrefix(ln, "RENDER:"))
		if uerr != nil {
			t.Fatal(uerr)
		}
		if got != first {
			t.Fatalf("a separate process (own map seed) rendered different bytes:\n%q\n%q", first, got)
		}
		return
	}
	t.Fatalf("child produced no rendering:\n%s", out)
}

// The provider's already-cached prefix must come out of this component byte-identical:
// rewriting a message inside it would force a cache write of everything after it.
func TestSkeletonLeavesFrozenPrefixUntouched(t *testing.T) {
	dump := numberedRead(goSource)
	req := readReq("/w/adder.go", dump, dump, "newest\n")
	before := schema.MessageText(req.Input[1])
	rep := &components.Report{}
	// MaxCachedIdx=1 marks messages 0..1 as already committed to the provider's cache;
	// message 3 is the same dump in the uncached tail, message 5 is the newest read.
	ctx := &components.Ctx{Session: "s", Store: store.NewMemory(store.Options{}), CacheAware: true, MaxCachedIdx: 1}
	if _, err := skeletonFor(t, "min_tokens: 5\n").Offload(req, rep, ctx); err != nil {
		t.Fatal(err)
	}
	if got := schema.MessageText(req.Input[1]); got != before {
		t.Fatalf("rewrote a message inside the cached prefix:\n%s", got)
	}
	if schema.MessageText(req.Input[3]) == dump {
		t.Fatal("the uncached-tail read should still have been reduced")
	}
}

// A Bash file dump (`cat -n`, `sed -n … | cat -n`) is reached too, and a Bash command
// that is NOT a file dump is not — the grammar comes from an operand's extension, and
// only for a program whose stdout IS the file.
func TestSkeletonBashDumpSelection(t *testing.T) {
	for cmd, want := range map[string]string{
		"cat -n /w/adder.go":          "go",
		"sed -n '1,200p' /w/adder.go": "go",
		"head -100 /w/thing.py":       "python",
		"go test ./... -run TestAdd":  "",
		"grep -rn foo /w/adder.go":    "",
		"cat /w/a.go /w/b.py":         "",
		"cat /w/notes.txt":            "",
	} {
		args, _ := json.Marshal(map[string]string{"command": cmd})
		got := dumpGrammar(schema.ToolCall{Name: "Bash", Args: string(args)})
		if got != want {
			t.Fatalf("dumpGrammar(%q) = %q, want %q", cmd, got, want)
		}
	}
}
