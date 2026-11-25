#!/usr/bin/env bash
# approval-still-valid_test.sh: tests for the LISTS ONLY case of approval-still-valid.sh. It builds a
# temp repo with a bare origin, and a stub gh that serves the PR head. It needs git and jq. No network.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
script=$here/approval-still-valid.sh
d=$(mktemp -d); trap 'rm -rf "$d"' EXIT
fails=0
L1=scripts/release/pending-timing-fixes.txt
L2=internal/timingguard/baseline.txt

cat > "$d/gh" <<'STUB'
#!/usr/bin/env bash
printf '{"headRefOid":"%s","baseRefName":"main"}\n' "$(cat "$(dirname "$0")/head")"
STUB
chmod +x "$d/gh"
export PATH="$d:$PATH"
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t
# no user hooks in the temp repo
export GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=core.hooksPath GIT_CONFIG_VALUE_0=/dev/null

git init -q --bare "$d/origin.git"
git init -q -b main "$d/w"
cd "$d/w" || exit 1
git remote add origin "$d/origin.git"
mkdir -p scripts/release internal/timingguard
printf 'a1\na2\na3\na4\n' > $L1
printf 'b1\nb2\n' > $L2
printf 'package x\n' > code.go
git add -A && git commit -qm base && git push -q origin main
M0=$(git rev-parse HEAD)
# main moves: it removes a3 from list 1
printf 'a1\na2\na4\n' > $L1
git commit -qam "main: fix a3" && git push -q origin main
M1=$(git rev-parse HEAD)
# the PR, from M0: it removes a2 and b1, and adds code
git checkout -q -b pr "$M0"
printf 'a1\na3\na4\n' > $L1
printf 'b2\n' > $L2
printf 'package x\nvar y = 1\n' > code.go
git commit -qam "pr: fix a2, b1" && APPROVED=$(git rev-parse HEAD)

# merged <list1> <list2> <code>: merge M1 into the PR, then force the 3 files to the given content.
merged() {
  git checkout -q -B pr "$APPROVED"
  git merge -q --no-commit "$M1" >/dev/null 2>&1 || true
  printf "$1" > $L1; printf "$2" > $L2; printf "$3" > code.go
  git add -A && git commit -qm "merge main" && git rev-parse HEAD
}
# check <name> <expected rc> <expected text> <head>
check() {
  git push -q -f origin "$4:refs/pull/1/head"
  echo "$4" > "$d/head"
  out=$("$script" 1 "$APPROVED" 2>&1); rc=$?
  if [ "$rc" = "$2" ] && grep -q "$3" <<<"$out"; then echo "PASS $1"
  else echo "FAIL $1: rc=$rc, want $2 and '$3'"; echo "$out"; fails=$((fails + 1)); fi
}

CODE='package x\nvar y = 1\n'
check "(a) merge resolves only list conflicts" 0 "LISTS ONLY" "$(merged 'a1\na4\n' 'b2\n' "$CODE")"
check "(b) an extra list line is added" 1 "CHANGED" "$(merged 'a1\na4\na5\n' 'b2\n' "$CODE")"
check "(c) a removed line is restored" 1 "CHANGED" "$(merged 'a1\na2\na4\n' 'b2\n' "$CODE")"
check "(d) a code change" 1 "CHANGED" "$(merged 'a1\na4\n' 'b2\n' 'package x\nvar y = 2\n')"
[ "$fails" = 0 ] && echo "all passed" || { echo "$fails failed"; exit 1; }
