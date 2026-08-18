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
	out, _ := runStarlark(context.Background(), body, "find keep", nil, starlarkModel{}, false, AggroMedium)
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
	if out, _ := runStarlark(context.Background(), `[{"a":1}]`, "", nil, evilModel{}, false, AggroMedium); out != "" {
		t.Fatalf("disallowed program must fail open to \"\", got %q", out)
	}
}
