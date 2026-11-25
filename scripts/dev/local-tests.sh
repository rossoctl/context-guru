#!/usr/bin/env bash
# local-tests.sh [--dry-run] [--reuse <out-dir>] <pr> [<repo>]
#
# Run the heavy test suites for a PR on this machine, then post ONE PR comment with the result.
# Run it in the PR's worktree, at the PR's head. CI no longer runs these suites on a PR.
#
# The suites are the ones in scripts/release/part-a.sh (about 70 to 90 minutes on a laptop).
# The comment starts with this marker, which scripts/release/check-ready.sh reads:
#   <!-- local-tests sha=<full head sha> result=pass|fail -->
# A step that fails only on tests named in scripts/release/known-laptop-failures.txt does not make
# the result "fail". The comment names those tests.
#
# Timing tests (CG_TIMING_TESTS=1) run only when the PR adds or changes one. See
# scripts/dev/timing-tests-needed.sh. The comment says which. The integrator runs them in heavy CI
# and in Part A.
#
# --dry-run     print the comment instead of posting it.
# --reuse <dir> do not run the suites; read the summary and logs that part-a.sh left in <dir>.
#
# If posting fails, the script says so and prints the comment. It then exits 3.
# Exit 0: result pass. Exit 1: result fail. Exit 2: wrong input. Exit 3: could not post.
set -uo pipefail
dry=0; reuse=
while [ $# -gt 0 ]; do
  case $1 in
    --dry-run) dry=1; shift ;;
    --reuse) reuse=${2:?--reuse needs a directory}; shift 2 ;;
    *) break ;;
  esac
done
pr=${1:?usage: local-tests.sh [--dry-run] [--reuse <out-dir>] <pr> [<repo>]}
repo=${2:-rossoctl/context-guru}
here=$(cd "$(dirname "$0")" && pwd)
root=$(git rev-parse --show-toplevel) || { echo "ERROR: not in a git checkout" >&2; exit 2; }
cd "$root"

head=$(git rev-parse HEAD)
if [ "$dry" = 0 ] || [ -z "$reuse" ]; then
  prhead=$(gh pr view "$pr" --repo "$repo" --json headRefOid --jq .headRefOid) \
    || { echo "ERROR: could not read PR #$pr from GitHub" >&2; exit 2; }
  if [ "$prhead" != "$head" ]; then
    echo "ERROR: this checkout is at ${head:0:12}, but PR #$pr is at ${prhead:0:12}. Check out the PR head." >&2
    [ "$dry" = 1 ] || exit 2
  fi
fi
if [ "$dry" = 0 ] && [ -n "$(git status --porcelain --untracked-files=no)" ]; then
  echo "ERROR: the checkout has uncommitted changes. The result would not describe the head." >&2
  exit 2
fi

out=${reuse:-$(mktemp -d "${TMPDIR:-/tmp}/local-tests-pr$pr.XXXXXX")}
if [ -z "$reuse" ]; then
  if [ "$("$here/timing-tests-needed.sh")" = run ]; then timing=1; else timing=0; fi
  echo "running the suites (timing tests: $timing); logs in $out"
  CG_TIMING_TESTS=$timing "$here/../release/part-a.sh" "$out" || true
fi
sum=$out/summary.txt
[ -s "$sum" ] || { echo "ERROR: no summary at $sum" >&2; exit 2; }
grep -q '^DONE rc=' "$sum" || { echo "ERROR: $sum has no DONE line: the run did not finish" >&2; exit 2; }

# The suites must have run on this head. A summary from another commit says nothing about it.
sumsha=$(sed -n '1s/^commit //p' "$sum")
if [ "$sumsha" != "$head" ]; then
  echo "ERROR: $sum is for commit ${sumsha:-unknown}, not the head ${head:0:12}. Nothing posted." >&2
  exit 2
fi
done_rc=$(sed -n 's/^DONE rc=//p' "$sum" | tail -1)
# part-a.sh records whether the timing tests ran. A summary from an older part-a.sh has no line.
case $(sed -n 's/^timing-tests //p' "$sum" | head -1) in
  1) if [ "$("$here/timing-tests-needed.sh")" = run ]; then timing_line="timing tests: run (this PR adds or changes one)"
     else timing_line="timing tests: run"; fi ;;
  0) timing_line="timing tests: skipped (integrator runs them)" ;;
  *) timing_line="timing tests: unknown (the summary has no timing-tests line)" ;;
esac

known=$(grep -vE '^\s*(#|$)' "${CG_KNOWN_FAILURES_FILE:-$here/../release/known-laptop-failures.txt}")
result=pass
rows=
# part-a.sh gives rc=1 when any step failed, so rc=1 does not decide: the steps do (a step that
# fails only on known laptop failures stays a pass). Any other rc is a run that stopped (143) or
# broke: fail. Every step that part-a.sh runs must also have a line, or the run stopped short.
case $done_rc in 0|1) ;; *) result=fail; rows+="| (run) | **FAIL**: stopped, DONE rc=$done_rc |"$'\n' ;; esac
for st in $(grep -oE '^[[:space:]]*step [a-z0-9-]+' "$here/../release/part-a.sh" | awk '{print $2}'); do
  grep -qE "^(ok|flaky|FAILED|skip) +$st( |\$)" "$sum" || { result=fail; rows+="| $st | **FAIL**: no result line |"$'\n'; }
done
notes=
while read -r state name _; do
  case $state in
    ok) rows+="| $name | pass |"$'\n' ;;
    skip) rows+="| $name | skipped (timing tests off: the integrator runs them) |"$'\n' ;;
    flaky) rows+="| $name | pass (flaky: passed on the 2nd run) |"$'\n' ;;
    FAILED)
      # part-a.sh ran the step twice. The rerun decides, as in its flaky rule: a test that failed
      # only on the 1st run is flaky, not a failure. Without a rerun log, the 1st log decides.
      log=$out/$name.log
      [ -f "$out/$name.rerun.log" ] && log=$out/$name.rerun.log
      failing=$(grep -oE '^--- FAIL: [A-Za-z0-9_]+' "$log" 2>/dev/null | awk '{print $3}' | sort -u)
      if [ "$log" != "$out/$name.log" ]; then
        first=$(grep -oE '^--- FAIL: [A-Za-z0-9_]+' "$out/$name.log" 2>/dev/null | awk '{print $3}' | sort -u)
        flaky1=$(comm -23 <(printf '%s\n' "$first" | sed '/^$/d') <(printf '%s\n' "$failing" | sed '/^$/d'))
        [ -z "$flaky1" ] || notes+="- $name: flaky on the 1st run, passed on the rerun: $(tr '\n' ' ' <<<"$flaky1")"$'\n'
      fi
      unknown=$(comm -23 <(printf '%s\n' "$failing" | sed '/^$/d') <(printf '%s\n' "$known" | sort -u))
      if [ -n "$failing" ] && [ -z "$unknown" ] && ! grep -qE '^(panic:|FAIL\s.*\[build failed\]|FAIL\s.*timed out)' "$log"; then
        rows+="| $name | known laptop failure: $(tr '\n' ' ' <<<"$failing")|"$'\n'
        notes+="- $name failed only on known laptop failures: $(tr '\n' ' ' <<<"$failing")"$'\n'
      else
        result=fail
        tail=$(tail -n 15 "$log" 2>/dev/null | cut -c1-200)
        rows+="| $name | **FAIL** |"$'\n'
        notes+=$'\n'"Log tail of $name:"$'\n\n```\n'"$tail"$'\n```\n'
      fi ;;
  esac
done < <(grep -E '^(ok|flaky|FAILED|skip) ' "$sum")
grep -qE '^(ok|flaky|FAILED) ' "$sum" || result=fail

body=$(mktemp "${TMPDIR:-/tmp}/local-tests-body.XXXXXX")
{
  echo "<!-- local-tests sha=$head result=$result -->"
  echo "Local tests at \`${head:0:12}\`: **$result** ($(uname -s), $(go version 2>/dev/null | awk '{print $3}'))"
  echo
  echo "| Step | Result |"
  echo "| --- | --- |"
  printf '%s' "$rows"
  echo
  echo "Skipped: TestBobReleasePythonSuite (Bob plugin on hold, #1221)."
  echo "$timing_line"
  [ -z "$notes" ] || { echo; printf '%s' "$notes"; }
} > "$body"

if [ "$dry" = 1 ]; then
  cat "$body"
else
  if ! gh pr comment "$pr" --repo "$repo" --body-file "$body"; then
    echo "ERROR: could not post the comment. The result is $result. Comment text:" >&2
    cat "$body" >&2
    exit 3
  fi
  echo "posted: result=$result"
fi
rm -f "$body"
[ "$result" = pass ]
