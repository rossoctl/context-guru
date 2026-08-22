package dsl

import (
	"strings"
	"testing"
)

const makeFilter = `
schema_version: 1
filters:
  make:
    match: "^make"
    strip_lines_matching:
      - "^make\\[\\d+\\]:"
      - "^\\s*$"
    max_lines: 50
    on_empty: "make: ok"
tests:
  make:
    - name: strips entering/leaving
      input: |
        make[1]: Entering directory '/x'
        gcc -O2 foo.c
        make[1]: Leaving directory '/x'
      expected: |
        gcc -O2 foo.c
    - name: on_empty when all stripped
      input: |
        make[1]: Entering directory '/x'
        make[1]: Leaving directory '/x'
      expected: "make: ok"
`

func TestStripLinesAndOnEmpty(t *testing.T) {
	var r Registry
	if err := r.Load([]byte(makeFilter)); err != nil {
		t.Fatal(err)
	}
	c := r.Match("make test")
	if c == nil {
		t.Fatal("filter did not match selector")
	}
	out, _ := Apply(c, "make[1]: Entering directory '/x'\ngcc -O2 foo.c\nmake[1]: Leaving directory '/x'")
	if strings.TrimSpace(out) != "gcc -O2 foo.c" {
		t.Fatalf("strip_lines wrong: %q", out)
	}
	empty, _ := Apply(c, "make[1]: Entering directory '/x'\nmake[1]: Leaving directory '/x'")
	if empty != "make: ok" {
		t.Fatalf("on_empty wrong: %q", empty)
	}
}

func TestInlineTestsRun(t *testing.T) {
	fails, err := RunTests([]byte(makeFilter))
	if err != nil {
		t.Fatal(err)
	}
	if len(fails) != 0 {
		t.Fatalf("inline tests should pass, failures: %v", fails)
	}
}

func TestMatchOutputWithUnless(t *testing.T) {
	doc := `
schema_version: 1
filters:
  build:
    match: "build"
    match_output:
      - pattern: "BUILD SUCCESSFUL"
        message: "build: ok"
        unless: "WARNING"
`
	var r Registry
	if err := r.Load([]byte(doc)); err != nil {
		t.Fatal(err)
	}
	c := r.Match("build")
	if out, loss := Apply(c, "compiling...\nBUILD SUCCESSFUL in 3s"); out != "build: ok" || loss != LossWhole {
		t.Fatalf("match_output should collapse to message: %q loss=%d", out, loss)
	}
	// unless guard: a warning present -> do NOT collapse
	if out, _ := Apply(c, "WARNING: deprecated\nBUILD SUCCESSFUL"); out == "build: ok" {
		t.Fatal("unless guard should have prevented collapse")
	}
}

func TestMaxLinesTailLossiness(t *testing.T) {
	doc := "schema_version: 1\nfilters:\n  log:\n    match: log\n    max_lines: 2\n"
	var r Registry
	if err := r.Load([]byte(doc)); err != nil {
		t.Fatal(err)
	}
	out, loss := Apply(r.Match("log"), "a\nb\nc\nd")
	if !strings.Contains(out, "truncated") || loss != LossTail {
		t.Fatalf("max_lines should truncate with Tail loss: %q loss=%d", out, loss)
	}
}

func TestStripKeepMutuallyExclusive(t *testing.T) {
	doc := "schema_version: 1\nfilters:\n  x:\n    match: x\n    strip_lines_matching: ['a']\n    keep_lines_matching: ['b']\n"
	var r Registry
	if err := r.Load([]byte(doc)); err == nil {
		t.Fatal("expected compile error for strip+keep together")
	}
}

// The union gate is a pre-filter, so the ONLY thing that can go wrong is an
// under-match: a key some filter would have matched that the gate rejects, which turns a
// filter into a silent no-op. This asserts the gate never changes Match's answer, over
// the pattern shapes that actually appear in the builtins and in user filters — and it is
// the test that catches the real bug this optimisation shipped with first, a gate built
// without (?m) that rejected every `^`-anchored filter as soon as the key carried a
// leading `$ <command>` line.
func TestUnionGateNeverChangesMatchsAnswer(t *testing.T) {
	const doc = `
schema_version: 1
filters:
  anchored:
    match: '^=+ test session starts'
    strip_lines_matching: [' PASSED']
  command:
    match: '^\$ (rg|grep)\s'
    strip_lines_matching: ['^Binary file ']
  nocase:
    match: '(?i)^error:'
    strip_lines_matching: ['^\s+at ']
  alternation:
    match: 'Compiling|Downloading|Fetching'
    strip_lines_matching: ['^\s*Compiling ']
  endanchored:
    match: 'bytes written$'
    strip_lines_matching: ['^wrote ']
  dollarcost:
    match: '^\$\$\$'
    strip_lines_matching: ['^x']
`
	var r Registry
	if err := r.Load([]byte(doc)); err != nil {
		t.Fatal(err)
	}
	if r.gate == nil {
		t.Fatal("no union gate was built, so this test proves nothing")
	}
	keys := []string{
		"=== test session starts ===\nplatform linux",
		"$ python -m pytest -q\n=== test session starts ===\nplatform linux", // the (?m) case
		"$ rg -n foo /src\n/src/a.go:1:foo",
		"$ grep -rn foo /src\nBinary file /src/a.bin matches",
		"ERROR: could not resolve\n    at frame",
		"error: could not resolve\n    at frame",
		"$ cat x\nerror: could not resolve",
		"   Compiling serde v1.0.0",
		"$ cargo build\n   Downloading crates",
		"wrote out.bin\n4096 bytes written",
		"$$$ weird",
		"", // empty key
		"just some prose with nothing special in it",
		"$ cat /x/a.txt\njust some prose",
		"a\nb\nc\nd\ne\nf",
	}
	for _, key := range keys {
		gated := r.Match(key)
		saved := r.gate
		r.gate = nil
		plain := r.Match(key) // the same ordered scan, with no pre-filter at all
		r.gate = saved
		gn, pn := "<nil>", "<nil>"
		if gated != nil {
			gn = gated.Name
		}
		if plain != nil {
			pn = plain.Name
		}
		if gn != pn {
			t.Errorf("key %q: gated match %s, ungated %s — the gate changed the answer", key, gn, pn)
		}
	}
}

// A filter with an empty match cannot exist (Compile rejects it), but a registry that
// somehow held one would be gated by a pattern that matches everything — so buildGate
// declines to build a gate at all rather than build a useless one. Asserted through the
// only reachable path: a registry with no filters has no gate and still answers nil.
func TestEmptyRegistryHasNoGate(t *testing.T) {
	var r Registry
	if r.gate != nil {
		t.Fatal("an unloaded registry must have no gate")
	}
	if got := r.Match("anything at all"); got != nil {
		t.Fatalf("empty registry matched %s", got.Name)
	}
}
