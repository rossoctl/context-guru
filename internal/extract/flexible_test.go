package extract

import (
	"context"
	"strings"
	"testing"
)

// TestExecStarlarkRegex: a model-written filter can use the injected re_ helpers
// to trim within lines (surgical deletion), and the result is deletion-only so it
// passes containment.
func TestExecStarlarkRegex(t *testing.T) {
	body := "test_a PASSED [ 10%]\ntest_b FAILED [ 20%]\ntest_c PASSED [ 30%]"
	// Drop PASSED lines, then strip the trailing "[ NN%]" progress column.
	src := `lines = [ln for ln in INPUT.split("\n") if "PASSED" not in ln]
OUTPUT = re_sub(" \\[ *[0-9]+%\\]", "", "\n".join(lines))`
	out := execStarlark(context.Background(), body, src)
	if out != "test_b FAILED" {
		t.Fatalf("regex trim failed: %q", out)
	}
	if !IsContained(out, body) {
		t.Fatal("a pure deletion (drop lines + strip columns) must be contained")
	}
}

// TestExecStarlarkBadRegexFailsOpen: an invalid pattern errors the program, which
// yields "" (the caller falls back to deterministic).
func TestExecStarlarkBadRegexFailsOpen(t *testing.T) {
	if out := execStarlark(context.Background(), "abc", `OUTPUT = re_sub("(", "", INPUT)`); out != "" {
		t.Fatalf("bad regex must fail open (empty), got %q", out)
	}
}

// TestRewriteModeStillRequiresTheResultToDeriveFromTheInput pins the boundary of the
// rewrite contract. Rewrite mode drops the exact subsequence proof — a filter that strips
// a progress column and adds "N lines elided" markers is doing what it was asked — but it
// must NOT accept a result the model largely retyped: a paraphrase, a renumbered file, an
// invented value. Before the derivation floor, the default mode verified NOTHING about the
// relationship between output and input, and the only backstop was that the original stayed
// recoverable via context_guru_expand, at the cost of an agent turn.
func TestRewriteModeStillRequiresTheResultToDeriveFromTheInput(t *testing.T) {
	body := "the build failed because widget.c has an index error at line 86"
	def, rw := Cfg{}, Cfg{Rewrite: true}
	for _, bad := range []string{
		"widget.c raised a NullPointerException at line 421", // fabricated values
		"index error at line 86 in widget.c (build failed)",  // reordered paraphrase
	} {
		if IsContained(bad, body) {
			t.Skipf("test premise: %q must not be a subsequence", bad)
		}
		if ok, _ := validateExtraction(bad, body, nil, def); ok {
			t.Fatalf("default (deletion-only) must reject %q", bad)
		}
		if ok, why := validateExtraction(bad, body, nil, rw); ok {
			t.Fatalf("rewrite mode must refuse %q, got accepted (%q)", bad, why)
		}
	}
	// A deletion plus an elision marker — the shape the prompt teaches — is accepted.
	kept := "the build failed because widget.c\n# ... 1 line elided (call context_guru_expand) ..."
	if ok, why := validateExtraction(kept, body, nil, rw); !ok {
		t.Fatalf("rewrite mode must accept a marked deletion: %s", why)
	}
	// And a within-line rewrite that only deletes characters stays acceptable.
	if ok, why := validateExtraction("build failed widget.c index error line 86", body, nil, rw); !ok {
		t.Fatalf("rewrite mode must accept a character-deletion rewrite: %s", why)
	}
	// Even in rewrite mode, a KEEP id that vanished still fails the sanity gate.
	if ok, _ := validateExtraction("nothing relevant", body, []string{"widget.c"}, rw); ok {
		t.Fatal("rewrite mode must still preserve KEEP identifiers (sanity gate)")
	}
}

// guard: the JSON example filter still works with the new predeclared set.
func TestExecStarlarkJSONStillWorks(t *testing.T) {
	body := `[{"path":"a.py","m":"keep"},{"path":"b.py","m":"drop"}]`
	src := `data = json.decode(INPUT)
OUTPUT = json.encode([r for r in data if "keep" in r["m"]])`
	out := execStarlark(context.Background(), body, src)
	if strings.Contains(out, "drop") || !strings.Contains(out, "keep") {
		t.Fatalf("json filter regressed: %q", out)
	}
}
