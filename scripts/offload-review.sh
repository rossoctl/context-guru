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
# A step that fails under `set -e` would otherwise end the script with no message.
trap 'echo "offload-review: line $LINENO failed (exit $?)" >&2' ERR

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
# A plain name only: `--backend ../x` must not source a file from outside backends/.
[[ $backend =~ ^[a-z0-9_-]+$ ]] || { echo "bad backend name: $backend" >&2; exit 1; }
[ -f "$root/scripts/offload-review/backends/$backend.sh" ] || { echo "no backend $backend in scripts/offload-review/backends" >&2; exit 1; }
# shellcheck source=/dev/null
. "$root/scripts/offload-review/backends/$backend.sh"
for fn in backend_label backend_has_session backend_run; do
  declare -F "$fn" >/dev/null || { echo "backend $backend does not define $fn" >&2; exit 1; }
done

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
# posted <reviewer> <round>: exit 0 when that reviewer posted that round on this head.
posted() { grep -qE "<!-- offload-review reviewer=$1 round=$2 sha=$sha backend=[a-z0-9_-]+ verdict=(approve|blocking)" <<<"$reviews"; }

# The last round is the highest round in any marker, from either reviewer and any backend.
# `|| true`: grep exits 1 on a PR with no reviews yet, and pipefail would end the script.
last=$({ grep -oE '<!-- offload-review reviewer=[12] round=[0-9]+' <<<"$reviews" || true; } \
  | sed -E 's/.*round=//' | sort -n | tail -1)
last=${last:-0}
todo=()
if [ "$last" -gt 0 ] && { posted 1 "$last" || posted 2 "$last"; }; then
  # The last round is on this head. Finish it: run only the reviewer that has not posted.
  round=$last
  for r in 1 2; do posted "$r" "$round" || todo+=("$r"); done
  if [ ${#todo[@]} -eq 0 ]; then
    echo "Head $sha already has both round-$round reviews. Push the fixes first." >&2
    exit 1
  fi
else
  round=$((last + 1))
  todo=(1 2)
fi
if [ "$round" -gt "$max_rounds" ]; then
  echo "PR #$pr used its $max_rounds review rounds. A person must decide now." >&2
  exit 3
fi

git -C "$main" fetch -q origin "$head_ref" "$base_ref"
label=$(backend_label)

wt_of() { echo "$main/.claude/worktrees/review-pr$pr-$backend-r$1"; }

# One after the other, not in the reviewers' parallel jobs: two `git worktree add` calls at the
# same time can fail on a git lock file.
for r in "${todo[@]}"; do
  wt=$(wt_of "$r")
  if [ -d "$wt" ]; then
    git -C "$wt" checkout -q --detach "$sha"
    git -C "$wt" clean -qfd
  else
    git -C "$main" worktree add -q --detach "$wt" "$sha"
  fi
done

run_reviewer() {
  # Two `local` lines: bash expands a whole `local` line before it sets any name on it.
  local r=$1
  local wt mode=fresh resume=0
  local live="Live tests are NOT approved. Read the code only."
  wt=$(wt_of "$r")
  cd "$wt"
  if [ "$round" -gt 1 ]; then
    if backend_has_session 2>>review.log; then
      mode=resumed; resume=1
    else
      echo "Reviewer $r: no earlier session found; round $round starts fresh." >&2
    fi
  fi

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
      echo "Follow the same rules and format as before; they are repeated below."
      echo
    fi
    if [ "$round" -gt 1 ]; then
      echo "# Read the PR conversation first"
      echo "Read every review and comment on the PR, from the author and from the other reviewer:"
      echo "  gh pr view $pr --repo $repo --comments"
      echo "  gh api --paginate repos/$repo/pulls/$pr/reviews --jq '.[].body'"
      echo "  gh api --paginate repos/$repo/pulls/$pr/comments --jq '.[] | \"\\(.path):\\(.line) \\(.body)\"'"
      echo "Then:"
      echo "- For each earlier finding (yours and the other reviewer's), say: fixed, not fixed, or"
      echo "  rejected by the author. If the author rejected it with a reason, check the reason."
      echo "  Accept it when it is right. Keep the finding only if you can show the reason is wrong."
      echo "- Do not repeat a finding the other reviewer already raised. Write \"agree with"
      echo "  reviewer N, finding M\" and add only new proof."
      echo "- Review the new changes since round $((round - 1)). Raise a new finding on old code"
      echo "  only when it is serious."
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

[ ${#todo[@]} -eq 2 ] || echo "Round $round: reviewer ${todo[*]} has not posted yet; running it only."
pids=()
for r in "${todo[@]}"; do run_reviewer "$r" & pids+=("$!"); done
rc=0
for i in "${!todo[@]}"; do
  wait "${pids[$i]}" || { echo "Reviewer ${todo[$i]} failed; see its review.log" >&2; rc=1; }
done

reviews=$(gh api --paginate "repos/$repo/pulls/$pr/reviews" --jq '.[].body')
for r in "${todo[@]}"; do
  wt=$(wt_of "$r")
  [ -d "$wt" ] || { echo "Reviewer $r has no worktree at $wt" >&2; rc=1; continue; }
  # A reviewer may write only its own files. Anything else is a broken rule.
  extra=$(git -C "$wt" status --porcelain | grep -vE ' \.review(-prompt)?\.md$| review\.log$' || true)
  [ -z "$extra" ] || { echo "Reviewer $r changed files:"$'\n'"$extra" >&2; rc=1; }
  [ "$(git -C "$wt" rev-parse HEAD)" = "$sha" ] || { echo "Reviewer $r moved HEAD" >&2; rc=1; }
  grep -qE "<!-- offload-review reviewer=$r round=$round sha=$sha backend=$backend verdict=(approve|blocking)" <<<"$reviews" \
    || { echo "Reviewer $r did not post its review" >&2; rc=1; }
done

[ "$rc" -eq 0 ] && echo "Round $round: both $backend reviews are on PR #$pr for ${sha:0:12}."
exit "$rc"
