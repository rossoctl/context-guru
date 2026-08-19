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
	if !strings.Contains(reason, "program rejected") {
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
	for _, want := range []string{"STARLARK IS NOT PYTHON", "generator expression", "f-string"} {
		if !strings.Contains(codeContract, want) {
			t.Fatalf("codeContract no longer warns about %q", want)
		}
	}
}
