#!/usr/bin/env bash
# unsigned-commits.sh: read a PR's commits as JSON (the pulls/N/commits shape) on stdin, and print the
# short SHA of each commit that has no sign-off. Print nothing when every commit has one.
#
# A commit has a sign-off when its own message has a `Signed-off-by: ` line, or when a remediation commit
# later in the PR signs it off. That is the rule of the DCO app with `allowRemediationCommits.individual`
# (.github/dco.yml). A remediation line is
#
#   I, <name> <<email>>, hereby add my Signed-off-by to this commit: <full sha>
#
# in the message of ANOTHER commit of the PR. It counts only when
#   - the SHA is the full 40-character SHA of the unsigned commit, and
#   - <name> and <email> equal the author name and email of the unsigned commit (case-insensitive).
# A line written by someone else does not count: thirdParty stays off. The app also wants the
# remediation commit itself to be signed off, so a remediation commit with no `Signed-off-by: ` of its own
# does not count either.
set -euo pipefail
jq -r '
  def lc: ascii_downcase;
  . as $all
  | [ $all[]
      | select(.commit.message | test("Signed-off-by: "))
      | .commit.message
      | scan("(?m)^I, (.*) <(.*)>, hereby add my Signed-off-by to this commit: ([0-9a-fA-F]{40})[ \t\r]*$")
      | {name: (.[0] | lc), email: (.[1] | lc), sha: (.[2] | lc)} ] as $remed
  | $all[]
  | select(.commit.message | test("Signed-off-by: ") | not)
  | . as $c
  | select(any($remed[]; .sha == ($c.sha | lc)
                         and .name == ($c.commit.author.name | lc)
                         and .email == ($c.commit.author.email | lc)) | not)
  | .sha[0:12]'
