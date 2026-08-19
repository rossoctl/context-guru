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
