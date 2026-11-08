#!/usr/bin/env bash
# approval-still-valid.sh <pr> <approved-sha> [<repo>]: say whether an approval of an older head
# still counts for the PR's current head (RELEASING.md, "When the head moves after an approval").
#
# It compares the PR's change at the approved head with its change at the current head, each
# taken against its own merge base with the PR's base branch. Same patch-id: only the base moved
# (a clean rebase, or a clean merge of the base), so the approval still counts. Different: the
# head carries new code or a conflict resolution, and a reviewer must re-check the delta.
# One more case: the head descends from the approved head, has the same merge base, and the
# commits in between change only comments (see comments_only below). Owner rule, 2026-11-08: a
# comments-only commit keeps both approvals. It prints `COMMENTS ONLY` and exits 0. When unsure,
# a line counts as code.
# It prints `git range-diff` either way, so the result can be posted on the PR. When the new head
# is a merge of the base into the branch, also check `git show --remerge-diff <merge>`: it must be
# empty, which shows the merge resolved no conflict by hand.
#
# Run it inside a clone of the repo. Exit 0: same change, or comments only. Exit 1: changed. Exit 2: wrong input.
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
# comments_only <old> <new>: succeed only when every changed file is .go or .sh and every added
# or removed line is blank or a whole-line comment. A line counts as a comment only when it is:
#   .go: `//` or `// text` (not `//go:`, `//line`, `//export`, `// +build`, or `//text` with no
#        space), or one `/* ... */` that opens and closes on the line with no other marker.
#   .sh: `# text` or `#` (not `#!`, not `# shellcheck`, not `#text` with no space).
# A line of code with a trailing comment is code. A line inside a multi-line block comment is code.
comments_only() {
  local files f line t kind n=0
  files=$(git diff --name-only "$1" "$2") || return 1
  [ -n "$files" ] || return 1
  while IFS= read -r f; do
    case "$f" in *.go) kind=go ;; *.sh) kind=sh ;; *) return 1 ;; esac
    while IFS= read -r line; do
      case "$line" in "+++"*|"---"*) continue ;; "+"*|"-"*) ;; *) continue ;; esac
      t=${line:1}
      t=${t#"${t%%[![:space:]]*}"}
      n=$((n + 1))
      [ -z "$t" ] && continue
      if [ "$kind" = go ]; then
        case "$t" in
          "//go:"*|"//line"*|"//export"*|"// +build"*|"//+build"*) return 1 ;;
          "//"|"// "*) continue ;;
          "/*"*"*/")
            t=${t#"/*"}; t=${t%"*/"}
            case "$t" in *"/*"*|*"*/"*) return 1 ;; esac
            continue ;;
        esac
      else
        case "$t" in
          "#!"*|"# shellcheck"*) return 1 ;;
          "#"|"# "*) continue ;;
        esac
      fi
      return 1
    done < <(git diff -U0 "$1" "$2" -- "$f")
  done <<<"$files"
  # a change with no added or removed line (a mode change, for example) is not a comment change
  [ "$n" -gt 0 ]
}
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
if [ "$ob" = "$nb" ] && git merge-base --is-ancestor "$old" "$new" && comments_only "$old" "$new"; then
  echo "COMMENTS ONLY: ${old:0:12}..${new:0:12} changes only comment lines in .go and .sh files. The approval of ${old:0:12} still counts. Say so on the PR."
  exit 0
fi
echo "CHANGED: the head has new code or a conflict resolution. Each reviewer re-checks the delta."
exit 1
