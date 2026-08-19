package offload

import "fmt"

// The filter sets written from the MISS LEDGER — the output a coding agent actually
// produces, as opposed to the 26 builtins, which cover CI/build/IaC/package-manager
// output and fire on essentially none of it.
//
// Why two extra documents instead of more builtins: `codesmart` and `codesafe` are the
// published SWE-bench arms, so a filter added to `builtinFilters` silently moves a
// measured baseline. These load only under `agent_filters: safe|lossy`, the same way
// `textclean` and `searchfold` landed outside the measured presets.
//
// Filters here are keyed on the COMMAND (`^\$ …`, available since cmdfilter began
// prefixing the selector with the paired tool_use) with a shape alternative wherever the
// output self-identifies — because that is exactly what the ledger says the misses have
// in common: the output shapes are generic plain text, the commands are not.
//
// WHAT THE LEDGER SAID, and therefore what is NOT here. Mining 1,657 distinct
// (command, output) pairs / 637,634 tokens across every capture on this box —
// TestLedgerRankedMisses, re-runnable — ranked by tokens that reached the filter and
// matched nothing:
//
//	class                              tokens   share   filter?
//	file dumps (Read/cat/head/
//	  tail/sed -n)                    516,205   81.0%   NO — see below
//	pytest / unittest / runtests.py    65,469   10.3%   yes
//	git (diff/log/show/stash)          62,273    9.8%   NO — a diff is content; the only
//	                                                    boilerplate (`index ab..cd 100644`)
//	                                                    is ~4 tokens per changed file
//	pip / npm                          21,246    3.3%   yes (pip-resolver), narrowly
//	ruff / mypy / pylint / flake8              ~0       NO — the 44 apparent hits were
//	                                                    `Read` of paths containing "pylint"
//	go test / cargo / jest / vitest /
//	  tsc / eslint / docker / kubectl          0        NO — zero occurrences in the corpus.
//	                                                    Writing these would be intuition.
//
// The file-dump class is 81% of the mass and has no LOSSLESS reduction: its content IS
// the file. The only removable bytes are `Read`'s `NNN\t` gutter, and a line number is on
// the never-drop list. Shrinking a file dump needs signature extraction (`skeleton.go`,
// tree-sitter), a different mechanism from a line filter. Recorded here so the next person
// does not re-derive it from the same corpus.
//
// grep/find/ls path-prefix output and ANSI/\r terminal noise are deliberately absent:
// `components/reformat/searchfold.go` and `textclean.go` already remove both LOSSLESSLY,
// with no marker and no stash, which strictly dominates a filter here.
//
// YAML strings are SINGLE-quoted so regex backslashes stay literal.

// agentSafeFilters removes only ENUMERATED known-boilerplate line shapes. Like the
// builtins, an unrecognised line is never dropped, so an unanticipated diagnostic
// survives. Measured on the corpus above:
//
//	filter            fire rate        tokens removed
//	django-runtests   69/1657 (4.2%)   16,252 (2.55% of corpus, 68.9% of what it saw)
//	pip-resolver       2/1657 (0.1%)    6,537 (1.03% of corpus, 93.1% of what it saw)
//	pytest-warnings    9/1657 (0.5%)      853 (0.13% of corpus, 32.6% of what it saw)
const agentSafeFilters = `
schema_version: 1
filters:

  django-runtests:
    description: drop Django test-runner setup/teardown chatter and verbose PASS lines
    family: tests
    priority: 20
    match: '(^\$ .*runtests\.py|^Testing against Django installed in )'
    strip_lines_matching:
      - '^Testing against Django installed in '
      - '^Importing application \S+$'
      - '^Skipping setup of unused database'
      - '^Operations to perform:$'
      - '^  Synchronize unmigrated apps: '
      - '^  Apply all migrations: '
      - '^Synchronizing apps without migrations:$'
      - '^  Creating tables\.\.\.$'
      - '^    Creating table \S+$'
      - '^    Running deferred SQL\.\.\.$'
      - '^Running migrations:$'
      - '^  Applying \S+\.\.\. OK$'
      - '^(Creating|Cloning|Destroying) test database for alias '
      - '^System check identified no issues'
      - '\.\.\. ok$'
    cap: buildlog

  pytest-warnings:
    description: drop the third-party half of pytest's warnings summary (site-packages only)
    family: tests
    priority: 6
    match: '^(=+ warnings summary|\.\./\S*/site-packages/\S+:\d+$)'
    strip_lines_matching:
      - '^\.\./\S*/site-packages/\S+:\d+$'
      - '^\s+\S*/site-packages/\S+:\d+: \w+Warning:'
      - '^-- Docs: https://docs\.pytest\.org'
    cap: buildlog

  pip-resolver:
    description: cap pip's resolver version dumps, which put a thousand versions on one line
    family: pkg
    priority: 20
    match: '^ERROR: (Ignored the following|Could not find a version)'
    truncate_lines_at: 200
    cap: errors

tests:
  django-runtests:
    - name: setup chatter and passing tests go, failures and counts stay
      input: |
        Testing against Django installed in '/testbed/django' with up to 16 processes
        Importing application admin_utils
        Operations to perform:
          Apply all migrations: admin, sites
        Running migrations:
          Applying admin.0001_initial... OK
            Creating table django_content_type
        Creating test database for alias 'default'...
        test_cyclic (admin_utils.tests.NestedObjectsTests) ... ok
        test_broken (admin_utils.tests.NestedObjectsTests) ... FAIL
        System check identified no issues (0 silenced).
        Ran 2 tests in 0.460s
        Destroying test database for alias 'default'...
      expected: |
        test_broken (admin_utils.tests.NestedObjectsTests) ... FAIL
        Ran 2 tests in 0.460s
  pytest-warnings:
    - name: a site-packages deprecation header goes, a first-party warning stays
      input: |
        =========================== warnings summary ============================
        ../opt/miniconda3/lib/python3.9/site-packages/pkg_resources/__init__.py:3154
          /opt/miniconda3/lib/python3.9/site-packages/pkg_resources/__init__.py:3154: DeprecationWarning: deprecated
            declare_namespace(pkg)
        sphinx/registry.py:22
          /testbed/sphinx/registry.py:22: DeprecationWarning: pkg_resources is deprecated
        -- Docs: https://docs.pytest.org/en/stable/how-to/capture-warnings.html
      expected: |
        =========================== warnings summary ============================
            declare_namespace(pkg)
        sphinx/registry.py:22
          /testbed/sphinx/registry.py:22: DeprecationWarning: pkg_resources is deprecated
  pip-resolver:
    - name: the version list is cut, the error text is not
      input: |
        ERROR: Could not find a version that satisfies the requirement django==6.0 (from versions: 1.1.3, 1.1.4, 1.2, 1.2.1, 1.2.2, 1.2.3, 1.2.4, 1.2.5, 1.2.6, 1.2.7, 1.3, 1.3.1, 1.3.2, 1.3.3, 1.3.4, 1.3.5, 1.3.6, 1.3.7, 1.4, 1.4.1, 1.4.2, 1.4.3, 1.4.4, 1.4.5, 1.4.6, 1.4.7, 1.4.8)
        ERROR: No matching distribution found for django==6.0
      expected: |
        ERROR: Could not find a version that satisfies the requirement django==6.0 (from versions: 1.1.3, 1.1.4, 1.2, 1.2.1, 1.2.2, 1.2.3, 1.2.4, 1.2.5, 1.2.6, 1.2.7, 1.3, 1.3.1, 1.3.2, 1.3.3, 1.3.4, 1.3.5...
        ERROR: No matching distribution found for django==6.0
`

// agentLossyFilters is LOSSY IN A WAY THE SAFE SET IS NOT, and that is why it sits behind
// its own mode. `pytest-signal` uses keep_lines_matching — an allow-list of what to KEEP,
// which is a catch-all DROP of everything else, so a diagnostic nobody anticipated
// disappears from the view the model sees. The original is still stashed and the marker
// still names the recovery tool, so nothing is unrecoverable; but the model has to know to
// ask, and on the safe set it never has to.
//
// It is also the largest single win the ledger yielded, which is the whole reason it is
// offered at all:
//
//	filter          fire rate         tokens removed
//	pytest-signal   44/1657 (2.7%)    18,811 (2.95% of corpus, 78.4% of what it saw)
//
// Priority 15 puts it ahead of both pytest-warnings (6) and the builtin shape-keyed
// pytest (10), so under `lossy` a pytest run routes here and nowhere else — asking for the
// aggressive reducer and silently getting the conservative one would be worse than either.
const agentLossyFilters = `
schema_version: 1
filters:

  pytest-signal:
    description: LOSSY — keep only pytest's failure/error/summary lines, drop everything else
    family: tests
    priority: 15
    match: '^\$ .*(\bpytest\b|-m\s+pytest\b)'
    keep_lines_matching:
      - '^(FAILED|ERROR) '
      - '^E   '
      - '^>   '
      - '^_{5,}'
      - '^=+ .* =+$'
      - '^\d+ (passed|failed|error|skipped|warning)'
      - '(Error|Exception|Traceback)'
      - '^\S+\.py:\d+'
    cap: buildlog

tests:
  pytest-signal:
    - name: the failure and the counts stay, the warnings summary does not
      input: |
        ============================= test session starts =============================
        collected 12 items
        tests/test_a.py ....F.......
        E       AssertionError: assert '5:11:17 AM' == '5:11:17 AM'
        tests/test_a.py:41: AssertionError
        somewhere/harmless.txt was written
        1 failed, 11 passed in 0.42s
      expected: |
        ============================= test session starts =============================
        E       AssertionError: assert '5:11:17 AM' == '5:11:17 AM'
        tests/test_a.py:41: AssertionError
        1 failed, 11 passed in 0.42s
`

// agentFilterDocs returns the extra filter documents a mode asks for, innermost first.
// An unknown mode is an ERROR, not a silent "off": a filter set that fails to load is
// indistinguishable from one that never matches, and that class of silent no-op is
// expensive to notice.
func agentFilterDocs(mode string) ([]string, error) {
	switch mode {
	case "", "off":
		return nil, nil
	case "safe":
		return []string{agentSafeFilters}, nil
	case "lossy":
		return []string{agentSafeFilters, agentLossyFilters}, nil
	}
	return nil, fmt.Errorf("cmdfilter: unknown agent_filters %q (want off, safe or lossy)", mode)
}
