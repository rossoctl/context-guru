#!/usr/bin/env bash
# changed-since-green.sh <event> <sha> [<repo>]: print "run=true" or "run=false" (and a reason on
# stderr). The CI job changed-since-green calls it. It decides if the heavy jobs (build-test and
# record) run.
#
# - workflow_dispatch and a push of a v* tag: always run.
# - schedule: run only when main moved since the last green heavy run. The last green heavy run is
#   the newest run of ci.yaml with a green build-test, from a schedule or a workflow_dispatch event
#   on main, or a v* tag push. A run counts as green only when its job build-test succeeded: a run that skipped the heavy
#   jobs does not count. With no such run, run.
#
# GH: the gh binary (default: gh). It needs GH_TOKEN (or a login) with read access to Actions.
set -euo pipefail
event=${1:?usage: changed-since-green.sh <event> <sha> [<repo>]}
sha=${2:?usage: changed-since-green.sh <event> <sha> [<repo>]}
repo=${3:-${GITHUB_REPOSITORY:-rossoctl/context-guru}}
GH=${GH:-gh}

case $event in
  schedule) ;;
  *) echo "reason: $event always runs the heavy jobs" >&2; echo "run=true"; exit 0 ;;
esac

# For each event, scan the newest runs first until one has a green build-test. Do not filter by the
# run's own conclusion: a run whose build-test passed but whose fast job failed is red overall, and
# it still proves the heavy suites at its sha. Skipped scheduled runs pile up (8 a day), so the limit
# is far above that. A v* tag run (event push, head branch v<N>) also counts, when it ran at main's sha.
limit=${CG_GREEN_SCAN_LIMIT:-300}
green=
for ev in schedule workflow_dispatch push; do
  case $ev in
    push) branch=; filter='select(.headBranch | test("^v[0-9]"))' ;;
    *)    branch=main; filter='.' ;;
  esac
  ids=$("$GH" run list --repo "$repo" --workflow ci.yaml ${branch:+--branch "$branch"} --event "$ev" \
    --limit "$limit" --json databaseId,createdAt,headBranch \
    --jq "sort_by(.createdAt) | reverse | .[] | $filter | .databaseId")
  for id in $ids; do
    # The run is green for tier 3 only when build-test ran and passed.
    row=$("$GH" run view "$id" --repo "$repo" --json jobs,headSha,createdAt \
      --jq 'select([.jobs[] | select(.name == "build-test" and .conclusion == "success")] | length > 0) | "\(.createdAt) \(.headSha)"')
    [ -n "$row" ] || continue
    green+="$row"$'\n'
    break
  done
done
last=$(printf '%s' "$green" | sort | tail -1 | awk '{print $2}')
if [ -z "$last" ]; then echo "reason: no earlier green heavy run" >&2; echo "run=true"; exit 0; fi
if [ "$last" = "$sha" ]; then
  echo "reason: main is still at $sha, the last green heavy run" >&2; echo "run=false"
else
  echo "reason: main moved from $last to $sha since the last green heavy run" >&2; echo "run=true"
fi
