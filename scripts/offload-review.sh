#!/usr/bin/env bash
# Offload two independent reviews of one PR to an agent tool, in parallel, on this machine.
#
#   scripts/offload-review.sh <pr> [--backend <name>] [--live-tests <rules-file>]
#
# --backend picks the reviewer tool from scripts/offload-review/backends/<name>.sh.
# The default is $OFFLOAD_BACKEND, else bob.
#
# Each reviewer gets its own worktree, .claude/worktrees/review-pr<N>-<backend>-r<1|2>. A fixed
# path per reviewer lets round 2 resume that reviewer's own round-1 session, and keeps the two
# reviewers' sessions apart.
#
# --live-tests gives the reviewers permission to run tests on the eval box. Use it only after the
# user approved it. <rules-file> holds the box rules; they are not in this repo, so the caller
# passes them in.
#
# Exit 0: both reviews are posted. Exit 1: a reviewer failed. Exit 3: the round budget is used.
# Output for each reviewer: <worktree>/.review.md and <worktree>/review.log.
set -euo pipefail

usage="usage: offload-review.sh <pr> [--backend <name>] [--live-tests <rules-file>]"
pr=${1:?$usage}
shift
backend=${OFFLOAD_BACKEND:-bob}
rules=
while [ $# -gt 0 ]; do
  case $1 in
    --backend) backend=${2:?$usage}; shift 2 ;;
    --live-tests)
      rules=${2:?$usage}; shift 2
      [ -s "$rules" ] || { echo "rules file $rules is empty or missing" >&2; exit 1; } ;;
    *) echo "$usage" >&2; exit 1 ;;
  esac
done

root=$(git rev-parse --show-toplevel)
skill=$root/.claude/skills/offload-review
[ -f "$root/scripts/offload-review/backends/$backend.sh" ] || { echo "no backend $backend in scripts/offload-review/backends" >&2; exit 1; }
# shellcheck source=/dev/null
. "$root/scripts/offload-review/backends/$backend.sh"

max_rounds=${OFFLOAD_MAX_ROUNDS:-2}
# Worktrees hang off the main checkout, even when this runs inside another worktree.
main=$(dirname "$(git rev-parse --path-format=absolute --git-common-dir)")
repo=$(gh repo view --json nameWithOwner --jq .nameWithOwner)
me=$(gh api user --jq .login)

pr_json=$(gh pr view "$pr" --repo "$repo" --json headRefName,headRefOid,baseRefName)
head_ref=$(jq -r .headRefName <<<"$pr_json")
sha=$(jq -r .headRefOid <<<"$pr_json")
base_ref=$(jq -r .baseRefName <<<"$pr_json")

reviews=$(gh api --paginate "repos/$repo/pulls/$pr/reviews" --jq '.[].body')
# Round = rounds that reviewer 1 already posted, plus 1. Rounds count across backends.
done_rounds=$(grep -oE '<!-- offload-review reviewer=1 round=[0-9]+' <<<"$reviews" | sort -u | wc -l | tr -d ' ')
round=$((done_rounds + 1))
if [ "$round" -gt "$max_rounds" ]; then
  echo "PR #$pr used its $max_rounds review rounds. A person must decide now." >&2
  exit 3
fi
if grep -qE "<!-- offload-review reviewer=1 round=[0-9]+ sha=$sha " <<<"$reviews"; then
  echo "Head $sha already has a review. Push the fixes first." >&2
  exit 1
fi

git -C "$main" fetch -q origin "$head_ref" "$base_ref"
label=$(backend_label)

run_reviewer() {
  local r=$1 wt=$main/.claude/worktrees/review-pr$pr-$backend-r$r mode=fresh resume=0
  local live="Live tests are NOT approved. Read the code only."
  if [ -d "$wt" ]; then
    git -C "$wt" checkout -q --detach "$sha"
    git -C "$wt" clean -qfd
  else
    git -C "$main" worktree add -q --detach "$wt" "$sha"
  fi
  cd "$wt"
  if [ "$round" -gt 1 ] && backend_has_session; then mode=resumed; resume=1; fi

  if [ -n "$rules" ]; then
    live="Live tests ARE approved by the user. Use your own box folder, review-pr$pr-r$r.
Follow every rule below.

$(cat "$rules")"
  fi

  local base marker
  base=$(git merge-base "origin/$base_ref" HEAD)
  marker="<!-- offload-review reviewer=$r round=$round sha=$sha backend=$backend verdict=<approve|blocking> -->"
  {
    if [ "$mode" = resumed ]; then
      echo "# Re-check, round $round"
      echo "Your round-$((round - 1)) review is earlier in this session. The author pushed fixes."
      echo "Check that each of your earlier findings is fixed. Review only the new changes."
      echo "Raise a new finding on old code only when it is serious."
      echo "Follow the same rules and format as before; they are repeated below."
      echo
    fi
    cat "$skill/review-prompt.md"
    echo
    echo "## This review"
    echo "- PR: #$pr in $repo, head $sha, base $base."
    echo "- Read the diff with: git diff $base..HEAD"
    echo "- You are reviewer $r of 2, round $round of $max_rounds, context: $mode."
    echo "- Marker line (line 1 of .review.md): $marker"
    echo "- Identity table (lines 2-4):"
    echo "  | Reviewer | Kind | Started by | Context | Review | Round | Head | Verdict |"
    echo "  |---|---|---|---|---|---|---|---|"
    echo "  | Reviewer $r ($backend, user of @$me) | $label | offload-review skill, @$me's session | $mode | $r of 2 | $round of $max_rounds | \`${sha:0:12}\` | **<verdict>** |"
    echo "- Post with: gh pr review $pr --repo $repo --comment --body-file .review.md"
    echo "- $live"
  } > .review-prompt.md

  backend_run .review-prompt.md "$resume" > review.log 2>&1
}

run_reviewer 1 & p1=$!
run_reviewer 2 & p2=$!
rc=0
wait "$p1" || { echo "Reviewer 1 failed; see its review.log" >&2; rc=1; }
wait "$p2" || { echo "Reviewer 2 failed; see its review.log" >&2; rc=1; }

reviews=$(gh api --paginate "repos/$repo/pulls/$pr/reviews" --jq '.[].body')
for r in 1 2; do
  wt=$main/.claude/worktrees/review-pr$pr-$backend-r$r
  # A reviewer may write only its own files. Anything else is a broken rule.
  extra=$(git -C "$wt" status --porcelain | grep -vE ' \.review(-prompt)?\.md$| review\.log$' || true)
  [ -z "$extra" ] || { echo "Reviewer $r changed files:"$'\n'"$extra" >&2; rc=1; }
  [ "$(git -C "$wt" rev-parse HEAD)" = "$sha" ] || { echo "Reviewer $r moved HEAD" >&2; rc=1; }
  grep -qE "<!-- offload-review reviewer=$r round=$round sha=$sha backend=$backend verdict=(approve|blocking)" <<<"$reviews" \
    || { echo "Reviewer $r did not post its review" >&2; rc=1; }
done

[ "$rc" -eq 0 ] && echo "Round $round: both $backend reviews are on PR #$pr for ${sha:0:12}."
exit "$rc"
