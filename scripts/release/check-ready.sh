#!/usr/bin/env bash
# check-ready.sh <pr> [<repo>]: say whether a PR may merge, from what GitHub holds now.
#
# It checks, at the PR's CURRENT head:
#   - the PR is open, not a draft, and has no conflict;
#   - the last offload-review round has an approving marker from reviewer 1 AND reviewer 2,
#     both naming this exact head (an approval of an older head does not count; see
#     approval-still-valid.sh for a head that moved);
#   - every check run on the head has finished with success (or skipped / neutral);
#   - every commit on the PR has a Signed-off-by line (the DCO check needs it).
# It does NOT check for a behavior change. The integrator reads the diff for that, and needs a
# human developer's yes in words (RELEASING.md, step 2).
#
# The review markers are trusted as offload-review trusts them. Do not filter out reviews by the
# PR author: offload-review posts both reviews with the login of the person who ran it, which is
# often the author.
#
# Exit 0: ready. Exit 1: not ready (each reason is printed). Exit 2: wrong input, or a gh call
# failed (the message names it).
set -euo pipefail
pr=${1:?usage: check-ready.sh <pr> [<repo>]}
repo=${2:-rossoctl/context-guru}
fail=0
no() { echo "NOT READY: $*"; fail=1; }

# api <what> <gh args...>: run gh, and stop with exit 2 and a clear message if the call fails, so a
# network or login error never reads as "not ready" or as "no reviews".
api() { local what=$1; shift; gh "$@" || { echo "ERROR: could not read $what from GitHub (gh $1 failed)" >&2; exit 2; }; }

pj=$(api "the PR" pr view "$pr" --repo "$repo" --json state,isDraft,mergeable,headRefOid,title)
sha=$(jq -r .headRefOid <<<"$pj")
echo "PR #$pr: $(jq -r .title <<<"$pj")"
echo "head: $sha"
[ "$(jq -r .state <<<"$pj")" = OPEN ] || no "the PR is $(jq -r .state <<<"$pj")"
[ "$(jq -r .isDraft <<<"$pj")" = false ] || no "the PR is a draft"
m=$(jq -r .mergeable <<<"$pj")
[ "$m" = MERGEABLE ] || no "mergeable=$m (CONFLICTING: the author rebases; UNKNOWN: ask again in a minute)"

# Review markers: <!-- offload-review reviewer=N round=R sha=S backend=B verdict=V -->
bodies=$(api "the reviews" api --paginate "repos/$repo/pulls/$pr/reviews" --jq '.[].body')
markers=$(printf '%s\n' "$bodies" \
  | grep -oE '<!-- offload-review reviewer=[12] round=[0-9]+ sha=[0-9a-f]+ backend=[a-z0-9_-]+ verdict=[a-z]+' || true)
if [ -z "$markers" ]; then
  no "no offload-review marker on the PR"
else
  last=$(sed -E 's/.*round=([0-9]+).*/\1/' <<<"$markers" | sort -n | tail -1)
  echo "last review round: $last"
  for r in 1 2; do
    line=$(grep -E "reviewer=$r round=$last " <<<"$markers" | tail -1 || true)
    if [ -z "$line" ]; then no "reviewer $r has no review in round $last"; continue; fi
    rsha=$(sed -E 's/.*sha=([0-9a-f]+).*/\1/' <<<"$line")
    verdict=$(sed -E 's/.*verdict=([a-z]+).*/\1/' <<<"$line")
    backend=$(sed -E 's/.*backend=([a-z0-9_-]+).*/\1/' <<<"$line")
    echo "reviewer $r: round $last, backend $backend, verdict $verdict, head ${rsha:0:12}"
    [ "$verdict" = approve ] || no "reviewer $r verdict is $verdict"
    [ "$rsha" = "$sha" ] || no "reviewer $r approved ${rsha:0:12}, not the head (run approval-still-valid.sh)"
  done
fi

# CI: every check run on the head (the check-runs API; a check that reports only through the
# older commit-status API does not show here. The DCO rule is checked separately below.)
runs=$(api "the check runs" api --paginate "repos/$repo/commits/$sha/check-runs" --jq '.check_runs[] | "\(.status)\t\(.conclusion)\t\(.name)"')
[ -n "$runs" ] || no "no CI check ran on the head (retarget to main, or close and reopen the PR)"
while IFS=$'\t' read -r status concl name; do
  [ -n "$name" ] || continue
  case "$status/$concl" in
    completed/success|completed/skipped|completed/neutral) echo "check $name: $concl" ;;
    completed/*) no "check $name: $concl" ;;
    *) no "check $name: still $status" ;;
  esac
done <<<"$runs"

# DCO: every commit carries a sign-off. Only the author can add one; a merge past a missing
# sign-off needs the release owner, case by case.
unsigned=$(api "the commits" api --paginate "repos/$repo/pulls/$pr/commits" \
  --jq '.[] | select(.commit.message | test("Signed-off-by: ") | not) | .sha[0:12]')
[ -z "$unsigned" ] || no "commits with no Signed-off-by: $(tr '\n' ' ' <<<"$unsigned")"

if [ "$fail" = 0 ]; then echo "READY (now read the diff for a behavior change)"; fi
exit "$fail"
