#!/usr/bin/env bash
# heavy-ci.sh [--wait] [<ref>] [<repo>]: start the heavy CI jobs (race tests, coverage, record) on
# <ref> (default main). Print the run URL. With --wait, wait for the run and exit with its result.
set -euo pipefail
wait=0; [ "${1:-}" = --wait ] && { wait=1; shift; }
ref=${1:-main}
repo=${2:-rossoctl/context-guru}
gh workflow run ci.yaml --repo "$repo" --ref "$ref"
sleep 5
id=$(gh run list --repo "$repo" --workflow ci.yaml --branch "$ref" --event workflow_dispatch --limit 1 --json databaseId --jq '.[0].databaseId')
echo "run: https://github.com/$repo/actions/runs/$id"
[ "$wait" = 0 ] || gh run watch "$id" --repo "$repo" --exit-status
