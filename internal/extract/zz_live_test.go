package extract

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/internal/cheapmodel"
	"github.com/rossoctl/context-guru/internal/tokens"
)

// Live, model-in-the-loop example of extract strategy: code on a realistic raw-text
// tool output. Gated by CG_LIVE=1 (+ CG_BASE/CG_TOKEN) so it never runs in CI.
func TestLiveExtractCodeExample(t *testing.T) {
	if os.Getenv("CG_LIVE") == "" {
		t.Skip("set CG_LIVE=1 CG_BASE=... CG_TOKEN=... to run the live example")
	}
	model := cheapmodel.Anthropic{
		BaseURL: os.Getenv("CG_BASE"), APIKey: os.Getenv("CG_TOKEN"), AuthScheme: "bearer",
		Model: liveModel(), MaxTokens: 4096,
	}
	// A realistic pytest run: 1 failure + traceback buried in many PASSED lines.
	var b strings.Builder
	b.WriteString("============================= test session starts ==============================\n")
	b.WriteString("collected 214 items\n\n")
	for i := 0; i < 90; i++ {
		b.WriteString("tests/test_matrices.py::test_case_" + itoa(i) + " PASSED                       [  " + itoa(i) + "%]\n")
	}
	b.WriteString("tests/test_matrices.py::test_col_insert FAILED                          [ 61%]\n")
	for i := 0; i < 90; i++ {
		b.WriteString("tests/test_solvers.py::test_ok_" + itoa(i) + " PASSED                        [ 99%]\n")
	}
	b.WriteString(`
=================================== FAILURES ===================================
_____________________________ test_col_insert _________________________________
    def test_col_insert():
        M = Matrix([[1, 2], [3, 4]])
>       result = M.col_insert(1, Matrix([[5], [6]]))
E       IndexError: Index out of range: a[2]
sympy/matrices/common.py:86: IndexError
=========================== short test summary info ============================
FAILED tests/test_matrices.py::test_col_insert - IndexError: Index out of range
======================== 1 failed, 180 passed in 4.21s =========================
`)
	body := b.String()
	goal := "Fix the failing test test_col_insert in sympy/matrices/common.py (col_insert IndexError)."
	keep := HarvestIdentifiers(goal, 40)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// One model call: capture the exact filter, then run THAT source (so the
	// printed program is precisely the one that produced the result).
	src, err := model.Complete(ctx, buildCodePrompt(body, goal, keep, false))
	if err != nil {
		t.Fatal(err)
	}
	src = stripFences(src)
	out := execStarlark(ctx, body, src)
	okv, _ := validateExtraction(out, body, keep, DefaultCfg())
	ok := out != "" && out != body && okv
	t.Logf("\n----- MODEL-WRITTEN STARLARK FILTER -----\n%s\n", src)
	t.Logf("\n----- RESULT -----\nbefore=%d tokens (%d lines)  after=%d tokens (%d lines)  accepted(contained+sane+smaller)=%v\n\n--- AFTER (first 25 lines) ---\n%s\n",
		tokens.Count(body), strings.Count(body, "\n")+1,
		tokens.Count(out), strings.Count(out, "\n")+1, ok, firstLines(out, 25))
	if !ok {
		t.Fatalf("expected an accepted reduction; got %d->%d", tokens.Count(body), tokens.Count(out))
	}
}

// Live example on a JSON tool output (code-search hits): most records are
// irrelevant; the model-written filter keeps only the ones touching the goal.
func TestLiveExtractCodeJSON(t *testing.T) {
	if os.Getenv("CG_LIVE") == "" {
		t.Skip("set CG_LIVE=1 CG_BASE=... CG_TOKEN=... to run the live example")
	}
	model := cheapmodel.Anthropic{
		BaseURL: os.Getenv("CG_BASE"), APIKey: os.Getenv("CG_TOKEN"),
		Model: "claude-sonnet-4-6", MaxTokens: 4096,
	}
	var recs []string
	for i := 0; i < 22; i++ {
		p := "src/unrelated/module_" + itoa(i) + ".py"
		s := "def helper_" + itoa(i) + "(): return " + itoa(i)
		if i == 7 {
			p, s = "sympy/matrices/common.py", "def col_insert(self, pos, other): ..."
		}
		if i == 15 {
			p, s = "sympy/matrices/tests/test_commonmatrix.py", "def test_col_insert(): assert ..."
		}
		recs = append(recs, `{"path":"`+p+`","line":`+itoa(i*3+1)+`,"match":"`+s+`"}`)
	}
	body := "[" + strings.Join(recs, ",\n ") + "]"
	goal := "Fix test_col_insert (col_insert in sympy/matrices/common.py)."
	keep := HarvestIdentifiers(goal, 40)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	src, err := model.Complete(ctx, buildCodePrompt(body, goal, keep, false))
	if err != nil {
		t.Fatal(err)
	}
	src = stripFences(src)
	out := execStarlark(ctx, body, src)
	okv, _ := validateExtraction(out, body, keep, DefaultCfg())
	ok := out != "" && out != body && okv
	t.Logf("\n----- MODEL-WRITTEN STARLARK FILTER -----\n%s\n", src)
	t.Logf("\n----- RESULT -----\nbefore=%d tokens (%d records)  after=%d tokens  accepted=%v\n\n--- AFTER ---\n%s\n",
		tokens.Count(body), len(recs), tokens.Count(out), ok, out)
	if !ok {
		t.Fatalf("expected accepted reduction; got %d->%d", tokens.Count(body), tokens.Count(out))
	}
}

func liveModel() string {
	if m := os.Getenv("CG_MODEL"); m != "" {
		return m
	}
	return "aws/claude-sonnet-5"
}

// TestLiveExtractRewriteSummary exercises the DEFAULT (rewrite:true) contract plus
// the SUMMARY global on a verbose install log — the new powerful path. Prints the
// program, the reduction, and the captured SUMMARY.
func TestLiveExtractRewriteSummary(t *testing.T) {
	if os.Getenv("CG_LIVE") == "" {
		t.Skip("set CG_LIVE=1 CG_BASE=... CG_TOKEN=... to run")
	}
	model := cheapmodel.Anthropic{
		BaseURL: os.Getenv("CG_BASE"), APIKey: os.Getenv("CG_TOKEN"), AuthScheme: "bearer",
		Model: liveModel(), MaxTokens: 4096,
	}
	var b strings.Builder
	b.WriteString("Collecting Sphinx==4.0.0\n")
	for i := 0; i < 40; i++ {
		b.WriteString("Requirement already satisfied: dep-" + itoa(i) + "<=1.0 in /opt/miniconda3/lib/python3.11/site-packages (from Sphinx)\n")
	}
	b.WriteString("Installing collected packages: Sphinx\n  Attempting uninstall: Sphinx\n    Found existing installation: Sphinx 4.0.0\nSuccessfully installed Sphinx-4.0.0\n")
	body := b.String()
	goal := "Install Sphinx and run the docs build."
	keep := HarvestIdentifiers(goal, 40)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	src, err := model.Complete(ctx, buildCodePrompt(body, goal, keep, true)) // rewrite=true (default)
	if err != nil {
		t.Fatal(err)
	}
	src = stripFences(src)
	out, summary := execStarlarkSummary(ctx, body, src)
	cfg := DefaultCfg()
	cfg.Rewrite = true
	okv, _ := validateExtraction(out, body, keep, cfg)
	ok := out != "" && out != body && okv
	t.Logf("\n----- PROGRAM -----\n%s\n----- RESULT -----\nbefore=%d tok after=%d tok accepted=%v\nSUMMARY=%q\n--- AFTER ---\n%s\n",
		src, tokens.Count(body), tokens.Count(out), ok, summary, out)
	if !ok {
		t.Fatalf("expected accepted reduction; got %d->%d", tokens.Count(body), tokens.Count(out))
	}
}

func firstLines(s string, n int) string {
	ls := strings.Split(s, "\n")
	if len(ls) <= n {
		return s
	}
	return strings.Join(ls[:n], "\n") + "\n… (" + itoa(len(ls)-n) + " more lines)"
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var d []byte
	for i > 0 {
		d = append([]byte{byte('0' + i%10)}, d...)
		i /= 10
	}
	return string(d)
}
