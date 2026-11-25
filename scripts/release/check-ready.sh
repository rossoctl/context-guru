#!/usr/bin/env bash
# check-ready.sh <pr> [<repo>]: say whether a PR may merge, from what GitHub holds now.
#
# It checks, at the PR's CURRENT head:
#   - the PR is open, not a draft, and has no conflict;
#   - the last offload-review round has an approving marker from reviewer 1 AND reviewer 2,
#     both naming this exact head (an approval of an older head does not count; see
#     approval-still-valid.sh for a head that moved);
#   - every check run on the head has finished with success (or skipped / neutral). The heavy CI jobs
#     do not run on a PR (lean process, 2026-11-24), so the next point replaces them;
#   - the PR body or a PR comment holds the line "Local tests: waived (owner rule)" (any case): the
#     interim rule in CLAUDE.md, "Working on a task". It replaces the next point. Or:
#   - a comment from scripts/dev/local-tests.sh names this exact head with result=pass and tier=2:
#     <!-- local-tests sha=S result=pass tier=2 scope=none|changed|full -->. A later commit, even
#     comment-only, needs a new run. scope=none must match the PR's files (no Go file, go.mod, go.sum,
#     testdata, embedded file or scripts/ file): scripts/dev/tier2-steps.sh --classify decides.
#     An old marker with no tier counts until OLD_MARKER_VALID_UNTIL (below);
#   - the head has a "CodeQL" check run from the code-scanning app that passed. GitHub's
#     code-scanning rule on main reads this run ("No new alerts in code changed by this pull
#     request"). A PR opened before the CodeQL workflow existed (#945) has none; it needs a new
#     push before it can merge. Without this check, an --admin merge bypasses the code-scanning
#     rule with no sign (#943, #937, #944);
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
# Last day (YYYY-MM-DD, inclusive) on which a local-tests marker with no "tier=" (the old format, a
# full Part A run) still counts. It stops a mass re-run of the open PRs on the day tier 2 lands. To
# change the cut-off, change this one line. CG_TODAY overrides today's date (for the tests).
OLD_MARKER_VALID_UNTIL=2026-12-02
today=${CG_TODAY:-$(date +%F)}
here=$(cd "$(dirname "$0")" && pwd)
pr=${1:?usage: check-ready.sh <pr> [<repo>]}
repo=${2:-rossoctl/context-guru}
fail=0
no() { echo "NOT READY: $*"; fail=1; }

# api <what> <gh args...>: run gh, and stop with exit 2 and a clear message if the call fails, so a
# network or login error never reads as "not ready" or as "no reviews".
api() { local what=$1; shift; gh "$@" || { echo "ERROR: could not read $what from GitHub (gh $1 failed)" >&2; exit 2; }; }

pj=$(api "the PR" pr view "$pr" --repo "$repo" --json state,isDraft,mergeable,headRefOid,title,body)
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
runs=$(api "the check runs" api --paginate "repos/$repo/commits/$sha/check-runs" --jq '.check_runs[] | "\(.status)\t\(.conclusion)\t\(.name)\t\(.app.slug)"')
[ -n "$runs" ] || no "no CI check ran on the head (retarget to main, or close and reopen the PR)"
while IFS=$'\t' read -r status concl name _app; do
  [ -n "$name" ] || continue
  case "$status/$concl" in
    completed/success|completed/skipped|completed/neutral) echo "check $name: $concl" ;;
    completed/*) no "check $name: $concl" ;;
    *) no "check $name: still $status" ;;
  esac
done <<<"$runs"

# Local tests: the heavy suites run on the PR owner's laptop (scripts/dev/local-tests.sh), which posts
# a marker comment. The last marker decides. It must name the current head and say pass. A green heavy
# CI run on this head (workflow_dispatch on the PR branch) is accepted instead.
cbodies=$(api "the comments" api --paginate "repos/$repo/issues/$pr/comments" --jq '.[].body')
waiver=$(printf '%s\n%s\n' "$(jq -r '.body // ""' <<<"$pj")" "$cbodies" | grep -ciE '^[[:space:]]*Local tests: waived \(owner rule\)[[:space:]]*$' || true)
lt=$(printf '%s\n' "$cbodies" | grep -oE '<!-- local-tests sha=[0-9a-f]+ result=[a-z]+( tier=[0-9]+)?( scope=[a-z]+)? -->' | tail -1 || true)
lsha=; [ -z "$lt" ] || lsha=$(sed -E 's/.*sha=([0-9a-f]+).*/\1/' <<<"$lt")
# A marker at the current head decides, also when it says fail. The waiver applies only when there
# is no marker, or the marker names an older head.
if [ "${waiver:-0}" -gt 0 ] && [ "$lsha" != "$sha" ]; then
  echo "local tests: waived (owner rule)"
elif [ -z "$lt" ]; then
  heavy=$(printf '%s\n' "$runs" | awk -F'\t' '$3=="build-test" && $1=="completed" && $2=="success"{print "ok"; exit}')
  if [ "$heavy" = ok ]; then echo "local tests: none, but the heavy CI job passed on the head"
  else no "no local-tests comment (run scripts/dev/local-tests.sh $pr in the PR worktree)"; fi
else
  lres=$(sed -E 's/.*result=([a-z]+).*/\1/' <<<"$lt")
  ltier=; grep -q ' tier=' <<<"$lt" && ltier=$(sed -E 's/.* tier=([0-9]+).*/\1/' <<<"$lt")
  lscope=; grep -q ' scope=' <<<"$lt" && lscope=$(sed -E 's/.* scope=([a-z]+).*/\1/' <<<"$lt")
  if [ "$lsha" != "$sha" ]; then no "the last local-tests comment is for ${lsha:0:12}, not the head (run scripts/dev/local-tests.sh $pr again)"
  elif [ "$lres" != pass ]; then no "local tests at the head: result=$lres"
  elif [ -z "$ltier" ]; then
    # The old format: a full Part A run. It counts until the cut-off.
    if [[ "$today" > "$OLD_MARKER_VALID_UNTIL" ]]; then
      no "the local-tests marker is the old format (no tier=2), and it stopped counting after $OLD_MARKER_VALID_UNTIL (run scripts/dev/local-tests.sh $pr again)"
    else echo "local tests: pass at the head (old marker, no tier; counts until $OLD_MARKER_VALID_UNTIL)"; fi
  elif [ "$ltier" != 2 ]; then no "the local-tests marker says tier=$ltier; tier=2 is needed"
  elif [ "$lscope" = none ]; then
    # scope=none claims the PR has no Go file. Check it against the PR's files (renames: both names).
    pfiles=$(api "the PR files" api --paginate "repos/$repo/pulls/$pr/files" --jq '.[] | .filename, (.previous_filename // empty)')
    real=$(cd "$here" && ./../dev/tier2-steps.sh --classify <<<"$pfiles")
    if [ "$real" = none ]; then echo "local tests: pass at the head (tier 2, scope none)"
    else no "the local-tests marker says scope=none, but the PR changes files of scope $real (run scripts/dev/local-tests.sh $pr again)"; fi
  else echo "local tests: pass at the head (tier 2, scope ${lscope:-unknown})"; fi
fi

# Code scanning: the "CodeQL" check run, posted by GitHub's code-scanning app
# (github-advanced-security), not the "analyze" job of the workflow and not any other job with that
# name. A missing run means no scan of this head reached the rule. It exists only on a PR to main,
# where the workflow runs. There is no check of the alert list: the list for the merge ref includes
# the alerts of main itself, and it is empty, not an error, for a merge ref with no analysis yet.
cq=$(printf '%s\n' "$runs" | awk -F'\t' '$3=="CodeQL" && $4=="github-advanced-security"{print $1"/"$2; exit}')
case "$cq" in
  completed/success) echo "code scanning: CodeQL check run passed on the head" ;;
  "") no "no CodeQL check run on the head (the PR must target main; then push again, or close and reopen it, so the CodeQL workflow scans it)" ;;
  completed/*) no "CodeQL check run: ${cq#completed/}" ;;
  *) no "CodeQL check run still ${cq%%/*}; wait for it" ;;
esac

# DCO: every commit carries a sign-off. Only the author can add one; a merge past a missing
# sign-off needs the release owner, case by case.
unsigned=$(api "the commits" api --paginate "repos/$repo/pulls/$pr/commits" \
  --jq '.[] | select(.commit.message | test("Signed-off-by: ") | not) | .sha[0:12]')
[ -z "$unsigned" ] || no "commits with no Signed-off-by: $(tr '\n' ' ' <<<"$unsigned")"

if [ "$fail" = 0 ]; then echo "READY (now read the diff for a behavior change)"; fi
exit "$fail"
