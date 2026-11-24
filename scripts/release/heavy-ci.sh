#!/usr/bin/env bash
# heavy-ci.sh [--wait] [<ref>] [<repo>]: start the heavy CI jobs (race tests, coverage, record) on
# <ref> (default main). Print the run URL. With --wait, wait for the run and exit with its result.
#
# The script finds its run by the head sha of <ref> and a created_at at or after the dispatch time.
# It polls up to 30 times, 2 seconds apart, and fails with a clear error if the run does not show.
set -euo pipefail
wait=0; [ "${1:-}" = --wait ] && { wait=1; shift; }
ref=${1:-main}
repo=${2:-rossoctl/context-guru}
sha=$(gh api "repos/$repo/commits/$ref" --jq .sha)
since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
gh workflow run ci.yaml --repo "$repo" --ref "$ref"
id=
for _ in $(seq 1 30); do
  id=$(gh run list --repo "$repo" --workflow ci.yaml --event workflow_dispatch --limit 20 \
    --json databaseId,headSha,createdAt \
    --jq "[.[] | select(.headSha == \"$sha\" and .createdAt >= \"$since\")] | sort_by(.createdAt) | last | .databaseId // empty")
  [ -n "$id" ] && break
  sleep 2
done
[ -n "$id" ] || { echo "ERROR: the dispatched run of $ref ($sha) did not appear after 60 s. Look at https://github.com/$repo/actions/workflows/ci.yaml" >&2; exit 1; }
echo "run: https://github.com/$repo/actions/runs/$id"
[ "$wait" = 0 ] || gh run watch "$id" --repo "$repo" --exit-status
