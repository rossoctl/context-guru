#!/usr/bin/env bash
# approval-still-valid.sh <pr> <approved-sha> [<repo>]: say whether an approval of an older head
# still counts for the PR's current head (RELEASING.md, "When the head moves after an approval").
#
# It compares the PR's change at the approved head with its change at the current head, each
# taken against its own merge base with the PR's base branch. Same patch-id: only the base moved
# (a clean rebase, or a clean merge of the base), so the approval still counts. Different: the
# head carries new code or a conflict resolution, and a reviewer must re-check the delta.
# It prints `git range-diff` either way, so the result can be posted on the PR. When the new head
# is a merge of the base into the branch, also check `git show --remerge-diff <merge>`: it must be
# empty, which shows the merge resolved no conflict by hand.
#
# Run it inside a clone of the repo. Exit 0: same change. Exit 1: changed. Exit 2: wrong input.
set -euo pipefail
pr=${1:?usage: approval-still-valid.sh <pr> <approved-sha> [<repo>]}
old=${2:?usage: approval-still-valid.sh <pr> <approved-sha> [<repo>]}
repo=${3:-rossoctl/context-guru}
pj=$(gh pr view "$pr" --repo "$repo" --json headRefOid,baseRefName)
new=$(jq -r .headRefOid <<<"$pj")
base=$(jq -r .baseRefName <<<"$pj")
git fetch -q origin "$base" "pull/$pr/head" || exit 2
git cat-file -e "$old^{commit}" 2>/dev/null || git fetch -q origin "$old" || {
  echo "cannot fetch the approved head $old" >&2; exit 2; }
ob=$(git merge-base "origin/$base" "$old")
nb=$(git merge-base "origin/$base" "$new")
pid() { git diff "$1" "$2" | git patch-id --stable | cut -d' ' -f1; }
op=$(pid "$ob" "$old")
np=$(pid "$nb" "$new")
echo "approved: ${old:0:12} (base ${ob:0:12}) patch-id ${op:0:12}"
echo "head:     ${new:0:12} (base ${nb:0:12}) patch-id ${np:0:12}"
echo "--- git range-diff"
git range-diff "$ob..$old" "$nb..$new" || true
if [ "$op" = "$np" ]; then
  echo "SAME CHANGE: only the base moved. The approval of ${old:0:12} still counts. Say so on the PR."
  exit 0
fi
echo "CHANGED: the head has new code or a conflict resolution. Each reviewer re-checks the delta."
exit 1
