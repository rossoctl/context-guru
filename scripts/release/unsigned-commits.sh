#!/usr/bin/env bash
# unsigned-commits.sh: read a PR's commits as JSON (the pulls/N/commits shape) on stdin, and print the
# short SHA of each commit that has no sign-off. Print nothing when every commit has one.
#
# A merge commit (more than one parent) is never listed: the DCO app skips it, and a git merge message has no
# sign-off.
#
# Any other commit has a sign-off when its own message has a `Signed-off-by: ` line, or when a remediation
# commit in the PR signs it off. That is the rule of the DCO app with `allowRemediationCommits.individual`
# (.github/dco.yml). A remediation line is
#
#   I, <name> <<email>>, hereby add my Signed-off-by to this commit: <full sha>
#
# in the message of ANOTHER commit of the PR. It counts only when all of these hold, compared without regard
# to case:
#   - the SHA is the full 40-character SHA of the unsigned commit;
#   - <name> and <email> equal the author name and email of the unsigned commit;
#   - the author name and email of the commit that CARRIES the line equal those of the unsigned commit.
#     The text of a line is only text: this is what ties it to a real person. A line written by someone
#     else does not count, because thirdParty stays off;
#   - the commit that carries the line has a `Signed-off-by: ` of its own.
set -euo pipefail
jq -r '
  def lc: ascii_downcase;
  . as $all
  | [ $all[]
      | select(.commit.message | test("Signed-off-by: "))
      | . as $r
      | $r.commit.message
      | scan("(?m)^I, (.*) <(.*)>, hereby add my Signed-off-by to this commit: ([0-9a-fA-F]{40})[ \t\r]*$")
      | {name: (.[0] | lc), email: (.[1] | lc), sha: (.[2] | lc),
         by_name: ($r.commit.author.name | lc), by_email: ($r.commit.author.email | lc)} ] as $remed
  | $all[]
  | select((.parents | length) < 2)
  | select(.commit.message | test("Signed-off-by: ") | not)
  | . as $c
  | select(any($remed[]; .sha == ($c.sha | lc)
                         and .name == ($c.commit.author.name | lc)
                         and .email == ($c.commit.author.email | lc)
                         and .by_name == .name
                         and .by_email == .email) | not)
  | .sha[0:12]'
