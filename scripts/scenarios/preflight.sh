#!/bin/bash
# PREFLIGHT — the checks that must not be discovered by a paid run.
#
# Every arm here costs real money and roughly an hour of wall-clock, most of it sleeping past cache
# TTLs. A syntax error in an arm's embedded python is invisible to `bash -n` (a heredoc is just text
# to the shell), so without this the thing that finds one is a run that has already spent its budget
# and then dies at the summary step with nothing to show.
#
# The python half asserts its own COVERAGE as well as its subject: zero heredocs found is reported as a
# failure, because a checker that silently matches nothing looks exactly like a clean tree.
#
# This exists because a commit message once claimed the compile check was in the repo when it was
# only ever run by hand during development. A check that lives in someone's shell is not a check.
#
# Run by run-all.sh before the first arm, and safe to run alone: it starts nothing, spends nothing,
# and needs no CG_SCEN_* configuration.
set -u
here="$(cd "$(dirname "$0")" && pwd)"
fail=0

# 1. Shell syntax, every arm and the library.
for f in "$here"/*.sh; do
  bash -n "$f" || { echo "SYNTAX (bash): $f"; fail=1; }
done

# 2. Python syntax, for every heredoc an arm feeds to python3. `bash -n` cannot see inside one.
#
# Delegated to check_heredocs.py rather than inlined, for two reasons found by review:
#   - the scanner keys on the python3 INVOCATION rather than on the tag `PY`, so an arm added later
#     with `<<'EOF'` is not skipped in silence — which is the failure this preflight exists to remove,
#     not relocate;
#   - a scanner for heredocs, written inside a heredoc, matched its own regex literal as if it were
#     one. Its own file has no such problem.
python3 "$here/check_heredocs.py" "$here" || fail=1

# 3. sqlite3 and curl are what every arm reads its results with; python3 is what interprets them.
for cmd in python3 sqlite3 curl; do
  command -v "$cmd" >/dev/null || { echo "MISSING: $cmd"; fail=1; }
done

if [ "$fail" -ne 0 ]; then
  echo "PREFLIGHT FAILED — fix the above before spending an hour of gateway time."
  exit 1
fi
echo "preflight ok"
