#!/usr/bin/env bash
# local-tests_test.sh: fixture tests for local-tests.sh (--dry-run --reuse) and for the API-error
# contract of check-ready.sh. Run it in a git checkout. It needs no network and posts nothing.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(git -C "$here" rev-parse --show-toplevel)
cd "$root"
H=$(git rev-parse HEAD)
d=$(mktemp -d); trap 'rm -rf "$d"' EXIT
fails=0
# A fixture list of known laptop failures, so the cases do not depend on the real list.
printf '# fixture\nTestNoHooksKeepsTheBinaryDefaults\n' > "$d/known.txt"
export CG_KNOWN_FAILURES_FILE="$d/known.txt"
steps=$(grep -oE '^[[:space:]]*step [a-z0-9-]+' scripts/release/part-a.sh | awk '{print $2}')
mk() { { echo "commit $1"; for s in $steps; do echo "ok      $s"; done; } > "$d/summary.txt"; }
# expect <name> <want-rc> <want-text> : run local-tests.sh on the fixture in $d
expect() {
  local out rc
  out=$(scripts/dev/local-tests.sh --dry-run --reuse "$d" 1 2>&1); rc=$?
  if [ "$rc" = "$2" ] && grep -qF -- "$3" <<<"$out"; then echo "ok   $1"; else echo "FAIL $1 (rc=$rc, want rc=$2 and '$3')"; fails=1; fi
}
mk "$H"; echo "DONE rc=0" >> "$d/summary.txt"; expect pass 0 "result=pass"
mk "$H"; sed -i.bak 's/^ok      race-dash/flaky   race-dash (x)/' "$d/summary.txt"; echo "DONE rc=0" >> "$d/summary.txt"; expect flaky 0 "flaky: passed"
mk "$H"; sed -i.bak 's/^ok      race-rest/FAILED  race-rest (twice)/' "$d/summary.txt"; echo "DONE rc=1" >> "$d/summary.txt"
printf -- '--- FAIL: TestNoHooksKeepsTheBinaryDefaults (0s)\nFAIL\n' > "$d/race-rest.log"; expect known-failure 0 "result=pass"
# The rerun decides: 3 tests fail on the 1st run, only a known one on the rerun (#1331 at d4cd32ee).
mk "$H"; sed -i.bak 's/^ok      race-rest/FAILED  race-rest (twice)/' "$d/summary.txt"; echo "DONE rc=1" >> "$d/summary.txt"
printf -- '--- FAIL: TestHooksRunInOrderAndTakeEffect (0s)\n--- FAIL: TestNoHooksKeepsTheBinaryDefaults (0s)\n--- FAIL: TestHostedModeRefusesCaptureEnv (0s)\nFAIL\n' > "$d/race-rest.log"
printf -- '--- FAIL: TestNoHooksKeepsTheBinaryDefaults (0s)\nFAIL\n' > "$d/race-rest.rerun.log"
expect rerun-decides 0 "result=pass"; expect rerun-flaky-note 0 "flaky on the 1st run, passed on the rerun: TestHooksRunInOrderAndTakeEffect"
# A real failure in the rerun still fails.
printf -- '--- FAIL: TestReal (0s)\nFAIL\n' > "$d/race-rest.rerun.log"; expect rerun-real-failure 1 "result=fail"
rm -f "$d/race-rest.rerun.log" "$d/race-rest.log"
mk "$H"; sed -i.bak 's/^ok      race-proxy/FAILED  race-proxy (twice)/' "$d/summary.txt"; echo "DONE rc=1" >> "$d/summary.txt"
printf -- '--- FAIL: TestReal (0s)\nFAIL\n' > "$d/race-proxy.log"; expect real-failure 1 "result=fail"
printf 'commit %s\nok      vet\nok      build-purego\nok      build-record\nDONE rc=143\n' "$H" > "$d/summary.txt"; expect stopped-rc143 1 "result=fail"
mk deadbeef; echo "DONE rc=0" >> "$d/summary.txt"; expect sha-mismatch 2 "Nothing posted"

# The timing-tests line in the comment comes from the summary part-a.sh wrote.
mk "$H"; { sed -n 1p "$d/summary.txt"; echo "timing-tests 0"; sed 1d "$d/summary.txt"; } > "$d/s2" && mv "$d/s2" "$d/summary.txt"; echo "DONE rc=0" >> "$d/summary.txt"; expect timing-skipped 0 "timing tests: skipped (integrator runs them)"
mk "$H"; { sed -n 1p "$d/summary.txt"; echo "timing-tests 1"; sed 1d "$d/summary.txt"; } > "$d/s2" && mv "$d/s2" "$d/summary.txt"; echo "DONE rc=0" >> "$d/summary.txt"; expect timing-run 0 "timing tests: run"

# The node-timing step: skipped when the timing tests are off, a normal row when they are on.
mk "$H"; sed -i.bak 's/^ok      node-timing.*/skip    node-timing (CG_TIMING_TESTS=0: the timing tests are off)/' "$d/summary.txt"; echo "DONE rc=0" >> "$d/summary.txt"; expect node-timing-skipped 0 "| node-timing | skipped"
mk "$H"; sed -i.bak 's/^ok      node-timing/FAILED  node-timing (twice)/' "$d/summary.txt"; echo "DONE rc=1" >> "$d/summary.txt"
printf -- 'not ok 1 - a timing test\n' > "$d/node-timing.log"; expect node-timing-failed 1 "result=fail"
rm -f "$d/node-timing.log"
# A summary with no node-timing line is a run that stopped short.
mk "$H"; sed -i.bak '/node-timing/d' "$d/summary.txt"; echo "DONE rc=0" >> "$d/summary.txt"; expect node-timing-missing 1 "node-timing | **FAIL**: no result line"

# timing-tests-needed.sh: decide from a diff, in a scratch repo.
r=$(mktemp -d); trap 'rm -rf "$d" "$r"' EXIT
(
  cd "$r" && git init -q . && git config user.email t@t && git config user.name t
  printf 'package p\nfunc TestA(t *T) { testutil.RequireTiming(t) }\n' > a_test.go
  printf 'package p\nfunc TestB(t *T) {}\n' > b_test.go
  printf 'doc\n' > README.md
  git add . && git commit -qm base
  git tag base
  printf 'doc2\n' > README.md; git commit -qam docs; git tag docs
  printf 'package p\nfunc TestB(t *T) { x() }\n' > b_test.go; git commit -qam plain-test; git tag plain
  printf 'package p\nfunc TestA(t *T) { testutil.RequireTiming(t); y() }\n func TestC(t *T) {}\n' > a_test.go; git commit -qam timing-file; git tag timingfile
  printf 'package p\nfunc TestB(t *T) { x() }\nfunc TestD(t *T) { testutil.RequireTiming(t) }\n' > b_test.go; git commit -qam new-timing; git tag newtiming
) >/dev/null 2>&1
needs() { (cd "$r" && "$root/scripts/dev/timing-tests-needed.sh" "$1" "$2"); }
for c in "base docs skip" "docs plain skip" "plain timingfile run" "timingfile newtiming run"; do
  set -- $c
  got=$(needs "$1" "$2")
  if [ "$got" = "$3" ]; then echo "ok   timing-needed $1..$2 = $3"; else echo "FAIL timing-needed $1..$2 = $got, want $3"; fails=1; fi
done

# check-ready.sh: a failing gh must exit 2, not print NOT READY.
mkdir "$d/bin"; printf '#!/bin/sh\nexit 1\n' > "$d/bin/gh"; chmod +x "$d/bin/gh"
out=$(PATH="$d/bin:$PATH" scripts/release/check-ready.sh 1 2>&1); rc=$?
if [ "$rc" = 2 ] && ! grep -q "NOT READY" <<<"$out"; then echo "ok   check-ready-gh-error"; else echo "FAIL check-ready-gh-error (rc=$rc)"; fails=1; fi
# A gh that works for every call except the comments must exit 2, and must not say "no local-tests
# comment". Every other call succeeds, so a later failure cannot hide a swallowed comments error.
cat > "$d/bin/gh" <<'GH'
#!/bin/sh
case "$*" in
  "pr view"*) echo '{"state":"OPEN","isDraft":false,"mergeable":"MERGEABLE","headRefOid":"abc123","title":"t"}' ;;
  *issues/*/comments*) exit 1 ;;
  *reviews*) echo '<!-- offload-review reviewer=1 round=1 sha=abc123 backend=x verdict=approve -->'; echo '<!-- offload-review reviewer=2 round=1 sha=abc123 backend=x verdict=approve -->' ;;
  *check-runs*) printf 'completed\tsuccess\tpurego\tgithub-actions\n' ;;
  *pulls/*/commits*) exit 0 ;;
  *) exit 1 ;;
esac
GH
out=$(PATH="$d/bin:$PATH" scripts/release/check-ready.sh 1 2>&1); rc=$?
if [ "$rc" = 2 ] && grep -q "could not read the comments" <<<"$out" && ! grep -q "no local-tests comment" <<<"$out"; then echo "ok   check-ready-comments-error"; else echo "FAIL check-ready-comments-error (rc=$rc)"; echo "$out" | tail -3; fails=1; fi
# The interim waiver: "Local tests: waived (owner rule)" in the PR body or in a comment.
# Every other call is fine, so only the waiver decides the local-tests point.
mkgh() { # $1 = PR body, $2 = comments text, $3 = "fail" to make the comments call fail
  printf '%s' "$1" > "$d/body.txt"; printf '%s' "$2" > "$d/comments.txt"
  cat > "$d/bin/gh" <<GH
#!/bin/sh
case "\$*" in
  "pr view"*) jq -n --rawfile b "$d/body.txt" '{state:"OPEN",isDraft:false,mergeable:"MERGEABLE",headRefOid:"abc123",title:"t",body:\$b}' ;;
  *issues/*/comments*) [ "${3:-}" = fail ] && exit 1; cat "$d/comments.txt" ;;
  *reviews*) echo '<!-- offload-review reviewer=1 round=1 sha=abc123 backend=x verdict=approve -->'; echo '<!-- offload-review reviewer=2 round=1 sha=abc123 backend=x verdict=approve -->' ;;
  *check-runs*) printf 'completed\tsuccess\tpurego\tgithub-actions\ncompleted\tsuccess\tCodeQL\tgithub-advanced-security\n' ;;
  *pulls/*/commits*) exit 0 ;;
  *) exit 1 ;;
esac
GH
}
run() { out=$(PATH="$d/bin:$PATH" scripts/release/check-ready.sh 1 2>&1); rc=$?; }
mkgh $'text\nlocal tests: WAIVED (owner rule)\n' ""; run
if [ "$rc" = 0 ] && grep -q "local tests: waived (owner rule)" <<<"$out"; then echo "ok   waiver-in-body"; else echo "FAIL waiver-in-body (rc=$rc)"; echo "$out" | tail -3; fails=1; fi
mkgh "plain body" $'hello\nLocal tests: waived (owner rule)\n'; run
if [ "$rc" = 0 ] && grep -q "local tests: waived (owner rule)" <<<"$out"; then echo "ok   waiver-in-comment"; else echo "FAIL waiver-in-comment (rc=$rc)"; echo "$out" | tail -3; fails=1; fi
mkgh "plain body" "no waiver here"; run
if [ "$rc" = 1 ] && grep -q "no local-tests comment" <<<"$out"; then echo "ok   no-waiver-not-ready"; else echo "FAIL no-waiver-not-ready (rc=$rc)"; fails=1; fi
mkgh $'Local tests: waived (owner rule)\n' "" fail; run
if [ "$rc" = 2 ] && grep -q "could not read the comments" <<<"$out"; then echo "ok   waiver-comments-error"; else echo "FAIL waiver-comments-error (rc=$rc)"; fails=1; fi
# A marker at the current head decides, also with a waiver present. An older head does not.
mkgh $'Local tests: waived (owner rule)\n' $'<!-- local-tests sha=abc123 result=fail -->\n'; run
if [ "$rc" = 1 ] && grep -q "result=fail" <<<"$out" && ! grep -q "waived" <<<"$out"; then echo "ok   waiver-fail-marker-at-head"; else echo "FAIL waiver-fail-marker-at-head (rc=$rc)"; fails=1; fi
mkgh $'Local tests: waived (owner rule)\n' $'<!-- local-tests sha=def456 result=fail -->\n'; run
if [ "$rc" = 0 ] && grep -q "waived" <<<"$out"; then echo "ok   waiver-beats-old-marker"; else echo "FAIL waiver-beats-old-marker (rc=$rc)"; fails=1; fi
exit "$fails"
