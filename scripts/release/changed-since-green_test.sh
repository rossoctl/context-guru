#!/usr/bin/env bash
# changed-since-green_test.sh: fixture tests for changed-since-green.sh. A stub gh serves fixture
# JSON from a temp dir, so the test needs no network. It needs jq.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
script=$here/changed-since-green.sh
d=$(mktemp -d); trap 'rm -rf "$d"' EXIT
fails=0

# The stub gh. "run list --event E" prints $d/list.E.json through the --jq filter. "run view ID"
# prints $d/run.ID.json through the filter. STUB_FAIL=1 makes every call fail.
cat > "$d/gh" <<'STUB'
#!/usr/bin/env bash
[ "${STUB_FAIL:-}" = 1 ] && { echo "stub gh: API error" >&2; exit 1; }
d=$(dirname "$0")
args=("$@"); ev=; jqf=.
for ((i = 0; i < ${#args[@]}; i++)); do
  [ "${args[i]}" = --event ] && ev=${args[i+1]}
  [ "${args[i]}" = --jq ] && jqf=${args[i+1]}
done
if [ "$1 $2" = "run list" ]; then
  f=$d/list.$ev.json; [ -f "$f" ] || f=$d/empty.json
  jq -r "$jqf" "$f"
elif [ "$1 $2" = "run view" ]; then
  jq -r "$jqf" "$d/run.$3.json"
fi
STUB
chmod +x "$d/gh"
echo '[]' > "$d/empty.json"

# list <event> <id:branch:createdAt>...   (listed in the order given; the script sorts by date)
list() {
  local ev=$1; shift
  printf '%s\n' "$@" | jq -R 'split(":") | {databaseId: (.[0]|tonumber), headBranch: .[1], createdAt: .[2]}' | jq -s . > "$d/list.$ev.json"
}
# run <id> <sha> <createdAt> <build-test conclusion> [<other fast job conclusion>]
run() {
  jq -n --arg sha "$2" --arg at "$3" --arg bt "$4" --arg lb "${5:-success}" \
    '{headSha: $sha, createdAt: $at, jobs: [{name: "build-test", conclusion: $bt}, {name: "lint-build", conclusion: $lb}]}' > "$d/run.$1.json"
}
reset() { rm -f "$d"/list.*.json "$d"/run.*.json; }
# expect <name> <want-rc> <want-line> <event> <sha>
expect() {
  local out rc
  out=$(GH="$d/gh" "$script" "$4" "$5" r/r 2>/dev/null); rc=$?
  if [ "$rc" = "$2" ] && { [ -z "$3" ] || grep -qx -- "$3" <<<"$out"; }; then echo "ok   $1"; else echo "FAIL $1 (rc=$rc, out='$out', want rc=$2 and '$3')"; fails=1; fi
}

reset; list schedule 1:main:2026-11-25T01:00:00Z; run 1 aaa 2026-11-25T01:00:00Z success
expect "sha matches the last green run: run=false" 0 run=false schedule aaa
expect "main moved: run=true" 0 run=true schedule bbb
expect "workflow_dispatch always runs" 0 run=true workflow_dispatch aaa
expect "a tag push always runs" 0 run=true push aaa

reset
expect "no green run: run=true" 0 run=true schedule aaa

reset; list schedule 1:main:2026-11-25T01:00:00Z; run 1 aaa 2026-11-25T01:00:00Z skipped
expect "a run that skipped build-test is not green: run=true" 0 run=true schedule aaa

reset; list schedule 1:main:2026-11-25T01:00:00Z; run 1 aaa 2026-11-25T01:00:00Z success failure
expect "build-test green but a fast job red counts as green: run=false" 0 run=false schedule aaa

# 25 skipped runs (newer) before the one green run: the scan must go past 20.
reset; rows=(); for i in $(seq 2 26); do
  rows+=("$i:main:2026-11-26T$(printf '%02d' "$i"):00:00Z"); run "$i" aaa "2026-11-26T$(printf '%02d' "$i"):00:00Z" skipped
done
rows+=("1:main:2026-11-25T01:00:00Z"); run 1 aaa 2026-11-25T01:00:00Z success
list schedule "${rows[@]}"
expect "more than 20 skipped runs before the green run: run=false" 0 run=false schedule aaa

# A green dispatch newer than an older green schedule run decides.
reset; list schedule 1:main:2026-11-25T01:00:00Z; run 1 aaa 2026-11-25T01:00:00Z success
list workflow_dispatch 2:main:2026-11-25T05:00:00Z; run 2 bbb 2026-11-25T05:00:00Z success
expect "the newer green dispatch decides: run=false at bbb" 0 run=false schedule bbb
expect "the newer green dispatch decides: run=true at aaa" 0 run=true schedule aaa

# A green v* tag run at main's sha counts. A push run on another branch does not.
reset; list push 3:v0.7.0:2026-11-25T06:00:00Z 4:feat-x:2026-11-25T07:00:00Z
run 3 ccc 2026-11-25T06:00:00Z success; run 4 ddd 2026-11-25T07:00:00Z success
expect "a green tag run at main's sha: run=false" 0 run=false schedule ccc
expect "a push run on a feature branch does not count: run=true" 0 run=true schedule ddd

# A gh failure must stop the script with a non-zero exit code. It must never print run=false.
reset; list schedule 1:main:2026-11-25T01:00:00Z; run 1 aaa 2026-11-25T01:00:00Z success
out=$(STUB_FAIL=1 GH="$d/gh" "$script" schedule aaa r/r 2>/dev/null); rc=$?
if [ "$rc" != 0 ] && ! grep -q 'run=false' <<<"$out"; then echo "ok   a gh failure: rc!=0"; else echo "FAIL a gh failure (rc=$rc, out='$out')"; fails=1; fi

exit "$fails"
