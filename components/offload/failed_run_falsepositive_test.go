package offload

import (
	"strings"
	"testing"
)

// The four shapes below are the misclassifications a replay over 1,795 real SWE-bench
// requests found: 9 of 81 failed_run collapses were not superseded test runs at all, but
// source files and a commit diff the agent had just read in order to write its patch.
// Each was collapsed to a ~100-byte pointer and labelled "superseded by a later
// failed→re-run", because a run marker matched somewhere in the middle of the content.
//
// They are asserted at the REGEX level rather than through the component because that is
// where the defect was, and because a pipeline-level test would also have to satisfy the
// two-runs and cache gates, which would obscure what is being pinned.
func TestRunMarkersDoNotMatchSourceReadsOrDiffs(t *testing.T) {
	// A line-numbered file read, as `cat -n` / the Read tool emits. Every line begins with
	// its number, which is exactly why anchoring is sufficient here.
	numberedSourceWithTraceback := strings.Join([]string{
		"   1\timport numpy as np",
		"   2\t",
		`   3\t"""Example output when the file is malformed:`,
		"   4\tTraceback (most recent call last):",
		`   5\t  ValueError: bad table`,
		`   6\t"""`,
		"   7\tdef read(path):",
	}, "\n")

	numberedSourceWithFailuresAssignment := strings.Join([]string{
		"  10\t    failures = []",
		"  11\t    n_failures = failures",
		"  12\t    if n_failures:",
	}, "\n")

	gitShowDiff := strings.Join([]string{
		"commit 9f2ab7c41d0e",
		"    Fix qdp parsing",
		"",
		"diff --git a/astropy/io/ascii/qdp.py b/astropy/io/ascii/qdp.py",
		"--- a/astropy/io/ascii/qdp.py",
		"+++ b/astropy/io/ascii/qdp.py",
		"+    # Traceback (most recent call last) used to appear here",
		"-    raise ValueError",
	}, "\n")

	numberedTestFile := strings.Join([]string{
		"  42\tdef test_thing():",
		"  43\t    # FAILED cases are listed in the docstring below",
		"  44\t    assert True",
	}, "\n")

	for name, content := range map[string]string{
		"line-numbered source containing a traceback in a docstring": numberedSourceWithTraceback,
		"line-numbered source containing a failures assignment":      numberedSourceWithFailuresAssignment,
		"git show diff mentioning a traceback":                       gitShowDiff,
		"line-numbered test file mentioning FAILED in a comment":     numberedTestFile,
	} {
		if runMarkers.MatchString(content) {
			t.Errorf("%s: runMarkers matched, so failed_run would collapse a file the agent "+
				"just read and label it a superseded test run\n---\n%s\n---",
				name, content)
		}
		if failMarkers.MatchString(content) {
			t.Errorf("%s: failMarkers matched, so this content would be treated as a FAILURE\n---\n%s\n---",
				name, content)
		}
	}
}

// The anchoring must not cost us the real detections. These are the genuine run shapes
// read out of the same replay, including the ones that are mid-line by construction.
func TestRunMarkersStillMatchRealRuns(t *testing.T) {
	realRuns := map[string]string{
		"pytest padded summary (mid-line by construction)": "============ 1 failed, 40 passed in 12.31s ============",
		"pytest FAILURES banner": "=================================== FAILURES ===================================\n" +
			"____________________ test_prefix ____________________",
		"pytest short summary line": "FAILED tests/test_cache.py::test_prefix - AssertionError: 3 != 4",
		"a real traceback": "Traceback (most recent call last):\n" +
			"  File \"/testbed/run.py\", line 3, in <module>\n" +
			"ModuleNotFoundError: No module named 'asgiref'",
		"indented traceback inside captured output": "  Traceback (most recent call last):\n    IndexError: tuple index out of range",
		"go panic":      "panic: runtime error: index out of range [3]",
		"npm error":     "npm ERR! code ELIFECYCLE",
		"build failure": "BUILD FAILED in 2s",
	}
	for name, content := range realRuns {
		if !runMarkers.MatchString(content) {
			t.Errorf("%s: runMarkers no longer matches a real run — anchoring went too far\n---\n%s\n---",
				name, content)
		}
	}
	// Every one of those except the passing-summary case is also a FAILURE.
	for name, content := range realRuns {
		if name == "pytest padded summary (mid-line by construction)" {
			continue // covered below, with the pass/fail distinction
		}
		if !failMarkers.MatchString(content) {
			t.Errorf("%s: failMarkers no longer matches a real failure\n---\n%s\n---", name, content)
		}
	}
	// The pass/fail distinction that keeps failed_run from hiding a successful result.
	if failMarkers.MatchString("======== 41 passed, 0 failed in 9.10s ========") {
		t.Error(`"0 failed" is a PASS and must not be treated as a failure`)
	}
	if !failMarkers.MatchString("======== 1 failed, 40 passed in 12.31s ========") {
		t.Error("a non-zero failed count must be treated as a failure")
	}
}
