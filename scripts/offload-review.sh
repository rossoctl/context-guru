#!/usr/bin/env bash
# Offload two independent reviews of one PR to an agent tool, in parallel, on this machine.
#
#   scripts/offload-review.sh <pr> [--backend <name>] [--live-tests <rules-file>] [--same-head]
#   scripts/offload-review.sh <pr> --cleanup
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
# --same-head starts the next round on a head that already has both reviews of its round. Use it
# only when the user asked for it: for example, the reviewers could not run their tests, or the
# author replied to a finding without a code change. The round still counts toward the budget.
#
# --cleanup runs after the PR merges or closes. It removes the reviewer worktrees, the backend's
# sessions for them, and the live-test rules file.
#
# Exit codes:
#   0  both reviews are posted (or --cleanup is done)
#   1  a reviewer failed, or the input is wrong
#   2  the round budget is used; only the user can grant more (OFFLOAD_MAX_ROUNDS)
#   3  this head already has both reviews of its round; push the fixes first (or --same-head)
#
# Files for each reviewer, in its worktree:
#   .review-prompt.md  the prompt this script writes for the reviewer
#   .review.md         the review the reviewer writes and posts
#   review.log         the tool's output; this script writes it, the reviewer does not
set -euo pipefail
# A step that fails under `set -e` would otherwise end the script with no message.
trap 'echo "offload-review: line $LINENO failed (exit $?)" >&2' ERR

usage="usage: offload-review.sh <pr> [--backend <name>] [--live-tests <rules-file>] [--same-head] | <pr> --cleanup"
pr=${1:?$usage}
shift
backend=${OFFLOAD_BACKEND:-bob}
rules=
cleanup=0
same_head=0
while [ $# -gt 0 ]; do
  case $1 in
    --backend) backend=${2:?$usage}; shift 2 ;;
    --live-tests)
      rules=${2:?$usage}; shift 2
      [ -s "$rules" ] || { echo "rules file $rules is empty or missing" >&2; exit 1; } ;;
    --cleanup) cleanup=1; shift ;;
    --same-head) same_head=1; shift ;;
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
[[ $pr =~ ^[0-9]+$ ]] || { echo "bad PR number: $pr" >&2; exit 1; }

if [ "$cleanup" = 1 ]; then
  state=$(gh pr view "$pr" --repo "$repo" --json state --jq .state)
  if [ "$state" = OPEN ] && [ "${OFFLOAD_FORCE:-0}" != 1 ]; then
    echo "PR #$pr is still open. Clean up after it merges or closes (or set OFFLOAD_FORCE=1)." >&2
    exit 1
  fi
  for wt in "$main"/.claude/worktrees/review-pr"$pr"-*-r[12]; do
    [ -d "$wt" ] || continue
    name=${wt##*/}; b=${name#review-pr"$pr"-}; b=${b%-r[12]}
    if [[ $b =~ ^[a-z0-9_-]+$ ]] && [ -f "$root/scripts/offload-review/backends/$b.sh" ]; then
      # A subshell, so one backend's functions do not leak into the next worktree's cleanup.
      # shellcheck source=/dev/null
      ( . "$root/scripts/offload-review/backends/$b.sh"; cd "$wt"
        if declare -F backend_cleanup >/dev/null; then backend_cleanup; fi ) \
        || echo "Backend $b could not clean up its sessions for $wt" >&2
    fi
    # --force: the worktree holds the reviewer's untracked files (.review.md and the rest).
    git -C "$main" worktree remove --force "$wt"
    echo "Removed $wt"
  done
  rm -f "/tmp/offload-review-pr$pr-rules.md"
  exit 0
fi

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
rehead=0 # 1: this round reviews the same head as the round before it
if [ "$last" -gt 0 ] && { posted 1 "$last" || posted 2 "$last"; }; then
  # The last round is on this head. Finish it: run only the reviewer that has not posted.
  round=$last
  for r in 1 2; do posted "$r" "$round" || todo+=("$r"); done
  if [ ${#todo[@]} -eq 0 ]; then
    if [ "$same_head" = 0 ]; then
      echo "Head $sha already has both round-$round reviews. Push the fixes first," \
        "or run with --same-head if the user asked for a round on this head." >&2
      exit 3
    fi
    round=$((last + 1))
    todo=(1 2)
    rehead=1
  fi
else
  round=$((last + 1))
  todo=(1 2)
fi
if [ "$round" -gt "$max_rounds" ]; then
  echo "PR #$pr used its $max_rounds review rounds. Ask the user for more rounds" \
    "(OFFLOAD_MAX_ROUNDS=$((max_rounds + 1))), or let a person decide." >&2
  exit 2
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
    if [ "$mode" = resumed ] && [ "$rehead" = 1 ]; then
      echo "# Re-check, round $round, on the same head"
      echo "Your round-$((round - 1)) review is earlier in this session. The head did not move."
      echo "This round is for what is new on the PR since then: replies to findings, test results,"
      echo "and a working eval box if your earlier tests could not run. Run the tests you could not."
      echo "Follow the same rules and format as before; they are repeated below."
      echo
    elif [ "$mode" = resumed ]; then
      echo "# Re-check, round $round"
      echo "Your round-$((round - 1)) review is earlier in this session. The author pushed fixes."
      echo "Follow the same rules and format as before; they are repeated below."
      echo
    fi
    if [ "$round" -gt 1 ]; then
      # A plain file with placeholders, not echo lines: escaping jq inside echo broke twice.
      sed -e "s|{{PR}}|$pr|g" -e "s|{{REPO}}|$repo|g" -e "s|{{ROUND}}|$round|g" \
        -e "s|{{REVIEWER}}|$r|g" "$skill/review-prompt-later-rounds.md"
      echo
    fi
    cat "$skill/review-prompt.md"
    echo
    echo "## This review"
    echo "- PR: #$pr in $repo, head $sha, base $base."
    echo "- Read the diff with: git diff $base..HEAD"
    echo "- You are reviewer $r of 2, round $round of $max_rounds, context: $mode."
    [ "$rehead" = 0 ] || echo "- The head is the same as in round $((round - 1)). No new commits is expected, not a skip."
    echo "- Your folder is $wt. It is a git worktree of the PR head, only for you."
    echo "  Write your review to $wt/.review.md. Do not change any file outside this folder."
    [ "$round" -gt 1 ] || echo "  Do not read the other review-pr* folders in $main/.claude/worktrees."
    echo "- Marker line (line 1 of .review.md): $marker"
    echo "- Identity table (lines 2-4):"
    echo "  | Reviewer | Kind | Started by | Context | Review | Round | Head | Verdict |"
    echo "  |---|---|---|---|---|---|---|---|"
    echo "  | Reviewer $r ($backend, user of @$me) | $label | offload-review skill, @$me's session | $mode | $r of 2 | $round of $max_rounds | \`${sha:0:12}\` | **<verdict>** |"
    echo "- Post with: gh pr review $pr --repo $repo --comment --body-file $wt/.review.md"
    echo "- $live"
  } > .review-prompt.md

  backend_run .review-prompt.md "$resume" > review.log 2>&1
}

# A reviewer may write only in its own worktree. Its worktree is checked below. The checkouts
# where people work are checked here, but only with a warning: the user may edit them during
# the run.
watched=("$main"); [ "$root" = "$main" ] || watched+=("$root")
before=()
for d in "${watched[@]}"; do before+=("$(git -C "$d" status --porcelain)"); done

[ ${#todo[@]} -eq 2 ] || echo "Round $round: reviewer ${todo[*]} has not posted yet; running it only."
pids=()
for r in "${todo[@]}"; do run_reviewer "$r" & pids+=("$!"); done
rc=0
for i in "${!todo[@]}"; do
  wait "${pids[$i]}" || { echo "Reviewer ${todo[$i]} failed; see its review.log" >&2; rc=1; }
done

for i in "${!watched[@]}"; do
  [ "$(git -C "${watched[$i]}" status --porcelain)" = "${before[$i]}" ] \
    || echo "Warning: files changed in ${watched[$i]} during the review. Check that a reviewer did not write there." >&2
done

reviews=$(gh api --paginate "repos/$repo/pulls/$pr/reviews" --jq '.[].body')
for r in "${todo[@]}"; do
  wt=$(wt_of "$r")
  [ -d "$wt" ] || { echo "Reviewer $r has no worktree at $wt" >&2; rc=1; continue; }
  # A reviewer may write only its own files. Anything else is a broken rule.
  extra=$(git -C "$wt" status --porcelain | grep -vE ' (\.review(-prompt)?\.md|\.review-in-(reviews|comments|inline)\.txt|review\.log)$' || true)
  [ -z "$extra" ] || { echo "Reviewer $r changed files:"$'\n'"$extra" >&2; rc=1; }
  [ "$(git -C "$wt" rev-parse HEAD)" = "$sha" ] || { echo "Reviewer $r moved HEAD" >&2; rc=1; }
  grep -qE "<!-- offload-review reviewer=$r round=$round sha=$sha backend=$backend verdict=(approve|blocking)" <<<"$reviews" \
    || { echo "Reviewer $r did not post its review" >&2; rc=1; }
done

[ "$rc" -eq 0 ] && echo "Round $round: both $backend reviews are on PR #$pr for ${sha:0:12}."
exit "$rc"
