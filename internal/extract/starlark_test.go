package extract

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

// starlarkModel returns a fixed Starlark program that keeps records whose name
// contains "keep" — exercises real code execution over the full input.
type starlarkModel struct{}

func (starlarkModel) Complete(_ context.Context, _ string) (string, error) {
	return `
data = json.decode(INPUT)
kept = [r for r in data if "keep" in r["name"]]
OUTPUT = json.encode(kept)
`, nil
}

func TestRunStarlarkFiltersFullBody(t *testing.T) {
	var recs []string
	for i := 0; i < 100; i++ {
		name := "drop"
		if i%10 == 0 {
			name = "keep"
		}
		recs = append(recs, `{"id":`+strconv.Itoa(i)+`,"name":"`+name+`"}`)
	}
	body := "[" + strings.Join(recs, ",") + "]"
	out, _, _ := runStarlark(context.Background(), body, "find keep", nil, starlarkModel{}, false, false, AggroMedium)
	if out == "" {
		t.Fatal("expected a Starlark result")
	}
	if !IsContained(parseBody(out), parseBody(body)) {
		t.Fatalf("Starlark output must be a contained subset: %s", out)
	}
	if strings.Contains(out, "drop") {
		t.Fatal("filter should have dropped non-keep records")
	}
	if !strings.Contains(out, "keep") {
		t.Fatal("filter should have kept the keep records (recall, not truncation)")
	}
}

// malicious program must fail-open (no panic, returns "").
type evilModel struct{}

func (evilModel) Complete(_ context.Context, _ string) (string, error) {
	return `load("os", "x")`, nil // imports disabled
}

func TestRunStarlarkFailsOpenOnDisallowed(t *testing.T) {
	if out, _, _ := runStarlark(context.Background(), `[{"a":1}]`, "", nil, evilModel{}, false, false, AggroMedium); out != "" {
		t.Fatalf("disallowed program must fail open to \"\", got %q", out)
	}
}

// TestAPythonismIsReportedAsASyntaxRejection pins the diagnosis that was impossible before.
// claude-haiku-4-5 reliably writes Python, not Starlark: a generator expression inside any()
// is the single most common form. Measured on real Claude Code traffic, 12 of 13 calls were
// discarded here and every one of them reported "no usable program or reply", so the failure
// read as "the model ignored the prompt" when it had answered and been thrown away.
func TestAPythonismIsReportedAsASyntaxRejection(t *testing.T) {
	// A generator expression: valid Python, not a thing in Starlark.
	out, _, reason := execStarlarkDetail(context.Background(), "a\nb\nc\n",
		"OUTPUT = \"\\n\".join([l for l in INPUT.split(\"\\n\") if any(k in l for k in [\"a\"])])")
	if out != "" {
		t.Fatal("a generator expression must not execute")
	}
	if !strings.HasPrefix(reason, "syntax error") {
		t.Fatalf("the sandbox rejection must be reported, got %q", reason)
	}
	// The list-comprehension form the prompt now mandates must run.
	out, _, reason = execStarlarkDetail(context.Background(), "a\nb\nc\n",
		"OUTPUT = \"\\n\".join([l for l in INPUT.split(\"\\n\") if any([k in l for k in [\"a\"]])])")
	if reason != "" || out != "a" {
		t.Fatalf("the mandated form must run: out=%q reason=%q", out, reason)
	}
}

// TestTheContractTeachesTheSandboxsRestrictions guards the prompt text itself: these lines
// took extract_llm from a 92% program-rejection rate to 0/3 failures on the same real file
// read, so deleting them silently returns the component to useless.
func TestTheContractTeachesTheSandboxsRestrictions(t *testing.T) {
	for _, want := range []string{"STARLARK IS NOT PYTHON", "generator expression", "f-string", "type annotation"} {
		if !strings.Contains(codeContract, want) {
			t.Fatalf("codeContract no longer warns about %q", want)
		}
	}
}

// TestTheContractOnlyBansWhatTheSandboxActuallyRejects checks the prompt's prohibitions
// against the REAL interpreter, in both directions. It exists because the block carried
// false bans for months — %-formatting, dict.setdefault and sorted(key=...) were forbidden
// and all three work — which costs tokens on every single call and pushes the model toward
// contortions for no reason, while the two forms that actually killed calls in production
// (a type annotation, a set comprehension) were not named at all.
func TestTheContractOnlyBansWhatTheSandboxActuallyRejects(t *testing.T) {
	rejected := map[string]string{
		"generator expression": `ids = ["a"]` + "\n" + `OUTPUT = str(any(k in INPUT for k in ids))`,
		"type annotation":      "kept: list = []\nOUTPUT = str(len(kept))",
		"f-string":             "n = 3\nOUTPUT = f\"n={n}\"",
		"set literal":          `s = {"a", "b"}` + "\nOUTPUT = str(len(s))",
		"set comprehension":    `s = {x for x in ["a"]}` + "\nOUTPUT = str(len(s))",
		"while loop":           "i = 0\nwhile i < 2:\n  i = i + 1\nOUTPUT = str(i)",
		"try/except":           "try:\n  OUTPUT = INPUT\nexcept:\n  OUTPUT = \"\"",
		"global":               "def f():\n  global OUTPUT\n  OUTPUT = \"x\"\nf()",
	}
	for name, src := range rejected {
		if _, _, reason := execStarlarkDetail(context.Background(), "a\nb\n", src); reason == "" {
			t.Fatalf("%s: the contract bans this but the sandbox accepts it — the ban costs "+
				"tokens on every call for nothing", name)
		}
	}
	accepted := map[string]string{
		"dict comprehension": `d = {k: 1 for k in ["a", "b"]}` + "\nOUTPUT = str(len(d))",
		"%-formatting":       `OUTPUT = "%d of %d" % (1, 2)`,
		"lambda":             "f = lambda x: x + 1\nOUTPUT = str(f(1))",
		"sorted(key=)":       `OUTPUT = ",".join(sorted(["bb", "a"], key=len))`,
		"dict.setdefault":    "d = {}\nd.setdefault(\"a\", 1)\nOUTPUT = str(len(d))",
		"list comprehension": `OUTPUT = "\n".join([l for l in INPUT.split("\n") if l])`,
	}
	for name, src := range accepted {
		out, _, reason := execStarlarkDetail(context.Background(), "a\nb\n", src)
		if reason != "" || out == "" {
			t.Fatalf("%s: the contract says this is fine, sandbox says %q", name, reason)
		}
	}
}

// TestATruncatedReplyIsNotReportedAsASyntaxError: a reply cut off at the output cap leaves
// an incomplete program, which the parser reports as whatever error the cut happened to
// produce. Reported as "syntax error" it says "fix the prompt"; the actual fix is the reply
// budget. Measured in production: 2 of 30 calls ended this way, silently.
func TestATruncatedReplyIsNotReportedAsASyntaxError(t *testing.T) {
	cut := []string{
		"lines = INPUT.split(\"\\n\")\nkept = [ln for ln in lines if \"E\" in l",
		"OUTPUT = \"abc",
		"out = []\nfor ln in INPUT.split(\"\\n\"):\n  out.append(re_sub(\"a\", \"b\",",
	}
	for _, src := range cut {
		_, _, reason := execStarlarkDetail(context.Background(), "a\nb\n", src)
		if !strings.HasPrefix(reason, "truncated reply") {
			t.Fatalf("a cut-off program must be reported as truncated, got %q", reason)
		}
	}
	// A complete program that is merely wrong must NOT be called truncated.
	_, _, reason := execStarlarkDetail(context.Background(), "a\nb\n", "kept: list = []\nOUTPUT = \"a\"")
	if !strings.HasPrefix(reason, "syntax error") {
		t.Fatalf("a complete-but-invalid program is a syntax error, got %q", reason)
	}
	// And a program whose strings and brackets balance is never called truncated.
	if incompleteProgram("OUTPUT = \"a#b('\" + str(len([1, 2])) # ] trailing comment\n") {
		t.Fatal("a balanced program with quotes and comments read as truncated")
	}
}

// TestOneRepairRoundTripFixesAPythonism: the repair leg sends the parser's own message back
// and takes the corrected program. Bounded to ONE retry — a second billed call has to earn
// its place, and a model that cannot fix its program from the parser's message will not fix
// it on the third try.
func TestOneRepairRoundTripFixesAPythonism(t *testing.T) {
	before, _ := RepairStats()
	m := &scriptedModel{replies: []string{
		`OUTPUT = "\n".join([l for l in INPUT.split("\n") if any(k in l for k in ["a"])])`, // Python
		`OUTPUT = "\n".join([l for l in INPUT.split("\n") if any([k in l for k in ["a"]])])`,
	}}
	out, _, reason := runStarlark(context.Background(), "a\nb\n", "goal", nil, m, true, false, AggroMedium)
	if reason != "" || out != "a" {
		t.Fatalf("the repair round-trip must recover the call: out=%q reason=%q", out, reason)
	}
	if m.calls != 2 {
		t.Fatalf("expected exactly 2 calls (original + one repair), got %d", m.calls)
	}
	if _, fixed := RepairStats(); fixed == 0 {
		t.Fatal("a successful repair must be counted, or it cannot be priced")
	}
	if tried, _ := RepairStats(); tried <= before {
		t.Fatal("an attempted repair must be counted")
	}
	// A repair that fails must not hide the original cause, and must not retry again.
	m2 := &scriptedModel{replies: []string{"kept: list = []", "still: broken = 1"}}
	_, _, reason = runStarlark(context.Background(), "a\nb\n", "goal", nil, m2, true, false, AggroMedium)
	if !strings.HasPrefix(reason, "syntax error") || !strings.Contains(reason, "repair:") {
		t.Fatalf("a failed repair must report both causes, got %q", reason)
	}
	if m2.calls != 2 {
		t.Fatalf("the repair must be bounded to one retry, got %d calls", m2.calls)
	}
}

// scriptedModel returns canned replies in order, counting calls.
type scriptedModel struct {
	replies []string
	calls   int
}

func (m *scriptedModel) Complete(_ context.Context, _ string) (string, error) {
	i := m.calls
	m.calls++
	if i >= len(m.replies) {
		return "", nil
	}
	return m.replies[i], nil
}
