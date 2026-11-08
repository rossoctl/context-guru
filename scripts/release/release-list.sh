#!/usr/bin/env bash
# release-list.sh [<since-tag>] [<repo>]: list what is on origin/main since the last release.
#
# Build the release list from this, not from memory: a list kept by hand once mixed in PRs of the
# release before. For each commit it prints the short SHA, the PR number and the subject, and
# marks the kinds that stay out of the release notes (docs, test, ci, chore, refactor).
# The default <since-tag> is the newest v* tag.
set -euo pipefail
git fetch -q --tags origin main
tag=${1:-$(git tag --list 'v*' --sort=-v:refname | head -1)}
repo=${2:-rossoctl/context-guru}
echo "since $tag ($(git rev-list -n1 "$tag" | cut -c1-12)) on origin/main ($(git rev-parse --short=12 origin/main)):"
git log --first-parent --reverse --format='%h%x09%s' "$tag..origin/main" | while IFS=$'\t' read -r sha subj; do
  n=$(grep -oE '\(#[0-9]+\)$' <<<"$subj" | tr -dc 0-9 || true)
  if [ -z "$n" ]; then
    n=$(grep -oE 'pull request #[0-9]+' <<<"$subj" | tr -dc 0-9 || true)
    [ -z "$n" ] || subj=$(gh pr view "$n" --repo "$repo" --json title --jq .title)
  fi
  kind=feature/fix
  grep -qiE '^(docs|test|ci|chore|refactor|build)(\(|:)' <<<"$subj" && kind="no note"
  printf '%s  #%-5s %-12s %s\n' "$sha" "${n:-?}" "[$kind]" "$subj"
done
