#!/usr/bin/env bash
# timing-tests-needed.sh [<base> [<head>]]
#
# Say whether a PR must run the timing tests (CG_TIMING_TESTS=1). Print "run" or "skip".
# Exit 0 for both. The rule is a diff search, with no guessing:
#   run  if a changed line (added or removed) holds RequireTiming, TimingEnabled, timingOnly, timingEnabled or CG_TIMING_TESTS, or
#        if a changed test file holds one of them (so a new test in that file also runs them).
#   skip otherwise.
# <base> defaults to the merge base of HEAD and origin/main. <head> defaults to HEAD.
# If the base cannot be found, print "run": a check that cannot decide does not skip.
# See CLAUDE.md, "Timing tests".
set -uo pipefail
# Not `grep -q`: it exits at the first match, the writer then dies of SIGPIPE, and pipefail turns
# the pipeline's status into 141. On a large file or diff that printed "skip" at random (#1378).
head=${2:-HEAD}
base=${1:-$(git merge-base "$head" origin/main 2>/dev/null)} || base=
[ -n "$base" ] || { echo run; exit 0; }
pat='RequireTiming|TimingEnabled|timingOnly|timingEnabled|CG_TIMING_TESTS'

if git diff -U0 "$base" "$head" | grep -E '^[+-][^+-]' | grep -E "$pat" >/dev/null; then echo run; exit 0; fi

# A test file that has a timing switch: any change to the file counts. The file as of <head>.
while IFS= read -r f; do
  case $f in
    *_test.go|*.test.mjs|*.test.ts|*.py|*/helpers.mjs|internal/testutil/testutil.go) ;;
    *) continue ;;
  esac
  if git show "$head:$f" 2>/dev/null | grep -E "$pat" >/dev/null; then echo run; exit 0; fi
done < <(git diff --name-only "$base" "$head")
echo skip
