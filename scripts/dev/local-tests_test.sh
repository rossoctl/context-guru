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
  # A Python and a TypeScript suite with the timing switch, and a Python file without it.
  printf 'TIMING = os.environ.get("CG_TIMING_TESTS") == "1"\ndef test_a(): pass\n' > c.py
  printf 'const timing = process.env.CG_TIMING_TESTS === "1"\ntest("a", () => {})\n' > d.test.ts
  printf 'def f(): pass\n' > e.py
  git add . && git commit -qm suites; git tag suites
  printf 'TIMING = os.environ.get("CG_TIMING_TESTS") == "1"\ndef test_a(): pass\ndef test_b(): pass\n' > c.py; git commit -qam py-change; git tag pychange
  printf 'const timing = process.env.CG_TIMING_TESTS === "1"\ntest("a", () => {})\ntest("b", () => {})\n' > d.test.ts; git commit -qam ts-change; git tag tschange
  printf 'def f(): return 1\n' > e.py; git commit -qam py-plain; git tag pyplain
) >/dev/null 2>&1
# A large file and a large diff: the detector must not lose to SIGPIPE (#1378). 20 runs each.
(
  cd "$r" || exit 1
  { echo 'TIMING = os.environ.get("CG_TIMING_TESTS") == "1"'; for i in $(seq 1 20000); do echo "def test_$i(): pass"; done; } > big.py
  git add big.py && git commit -qm big; git tag big
  echo 'def test_more(): pass' >> big.py; git commit -qam big-change; git tag bigchange
  { echo '# CG_TIMING_TESTS'; for i in $(seq 1 20000); do echo "line $i"; done; } > big.txt
  git add big.txt && git commit -qm bigdiff; git tag bigdiff
) >/dev/null 2>&1
needs() { (cd "$r" && "$root/scripts/dev/timing-tests-needed.sh" "$1" "$2"); }
for c in "base docs skip" "docs plain skip" "plain timingfile run" "timingfile newtiming run" "newtiming suites run" "suites pychange run" "pychange tschange run" "tschange pyplain skip"; do
  set -- $c
  got=$(needs "$1" "$2")
  if [ "$got" = "$3" ]; then echo "ok   timing-needed $1..$2 = $3"; else echo "FAIL timing-needed $1..$2 = $got, want $3"; fails=1; fi
done
for c in "big bigchange" "bigchange bigdiff"; do
  set -- $c; n=0
  for _ in $(seq 1 20); do [ "$(needs "$1" "$2")" = run ] && n=$((n+1)); done
  if [ "$n" = 20 ]; then echo "ok   timing-needed $1..$2 = run, 20 of 20"; else echo "FAIL timing-needed $1..$2 = run only $n of 20 (SIGPIPE race)"; fails=1; fi
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
mkgh() { # $1 = PR body, $2 = comments text, $3 = "fail" to make the comments call fail; files in $d/files.txt
  [ -e "$d/files.txt" ] || : > "$d/files.txt"
  printf '%s' "$1" > "$d/body.txt"; printf '%s' "$2" > "$d/comments.txt"
  cat > "$d/bin/gh" <<GH
#!/bin/sh
case "\$*" in
  "pr view"*) jq -n --rawfile b "$d/body.txt" '{state:"OPEN",isDraft:false,mergeable:"MERGEABLE",headRefOid:"abc123",title:"t",body:\$b}' ;;
  *issues/*/comments*) [ "${3:-}" = fail ] && exit 1; cat "$d/comments.txt" ;;
  *reviews*) echo '<!-- offload-review reviewer=1 round=1 sha=abc123 backend=x verdict=approve -->'; echo '<!-- offload-review reviewer=2 round=1 sha=abc123 backend=x verdict=approve -->' ;;
  *check-runs*) printf 'completed\tsuccess\tpurego\tgithub-actions\ncompleted\tsuccess\tCodeQL\tgithub-advanced-security\n' ;;
  *pulls/*/files*) cat "$d/files.txt" ;;
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

# ---- check-ready.sh: the tier-2 marker, scope=none, the old marker and its cut-off ----------------
chk() { # <name> <want-rc> <want-text>
  if [ "$rc" = "$2" ] && grep -qF -- "$3" <<<"$out"; then echo "ok   $1"; else echo "FAIL $1 (rc=$rc, want rc=$2 and '$3')"; echo "$out" | tail -3; fails=1; fi
}
printf 'README.md\ndocs/a.md\n' > "$d/files.txt"
mkgh "b" $'<!-- local-tests sha=abc123 result=pass tier=2 scope=changed -->\n'; run
chk tier2-marker-ready 0 "tier 2, scope changed"
mkgh "b" $'<!-- local-tests sha=abc123 result=pass tier=2 scope=full -->\n'; run
chk tier2-full-ready 0 "tier 2, scope full"
mkgh "b" $'<!-- local-tests sha=abc123 result=pass tier=2 scope=none -->\n'; run
chk tier2-scope-none-ready 0 "scope none"
mkgh "b" $'<!-- local-tests sha=abc123 result=fail tier=2 scope=changed -->\n'; run
chk tier2-marker-fail 1 "result=fail"
mkgh "b" $'<!-- local-tests sha=def456 result=pass tier=2 scope=changed -->\n'; run
chk tier2-marker-old-head 1 "not the head"
mkgh "b" $'<!-- local-tests sha=abc123 result=pass tier=3 scope=changed -->\n'; run
chk tier3-marker-refused 1 "tier=2 is needed"
# scope=none on a PR that changes a Go file, go.mod, a testdata file or a scripts/ file: refused.
for f in proxy/a.go go.mod x/testdata/in.json scripts/dev/a.sh; do
  printf 'README.md\n%s\n' "$f" > "$d/files.txt"
  mkgh "b" $'<!-- local-tests sha=abc123 result=pass tier=2 scope=none -->\n'; run
  chk "scope-none-refused-on-$(tr '/' '_' <<<"$f")" 1 "says scope=none"
done
# A renamed file counts under both names.
printf 'docs/b.md\nold/a.go\n' > "$d/files.txt"
mkgh "b" $'<!-- local-tests sha=abc123 result=pass tier=2 scope=none -->\n'; run
chk scope-none-refused-on-rename 1 "says scope=none"
: > "$d/files.txt"
# The old marker (no tier): valid up to the cut-off date, refused after it.
cutoff=$(sed -n 's/^OLD_MARKER_VALID_UNTIL=//p' scripts/release/check-ready.sh)
mkgh "b" $'<!-- local-tests sha=abc123 result=pass -->\n'
out=$(CG_TODAY=$cutoff PATH="$d/bin:$PATH" scripts/release/check-ready.sh 1 2>&1); rc=$?
chk old-marker-on-cutoff-day 0 "old marker"
out=$(CG_TODAY=2026-11-26 PATH="$d/bin:$PATH" scripts/release/check-ready.sh 1 2>&1); rc=$?
chk old-marker-before-cutoff 0 "old marker"
out=$(CG_TODAY=2099-01-01 PATH="$d/bin:$PATH" scripts/release/check-ready.sh 1 2>&1); rc=$?
chk old-marker-after-cutoff 1 "stopped counting after $cutoff"
# The waiver stays valid after the cut-off, next to a tier-2 marker for an older head.
mkgh $'Local tests: waived (owner rule)\n' $'<!-- local-tests sha=def456 result=pass tier=2 scope=changed -->\n'
out=$(CG_TODAY=2099-01-01 PATH="$d/bin:$PATH" scripts/release/check-ready.sh 1 2>&1); rc=$?
chk waiver-after-cutoff 0 "local tests: waived (owner rule)"

# ---- local-tests.sh reads a tier-2 summary: the marker, the race packages, the CPU lines -----------
mk2() { # <scope> <steps...>: a tier-2 summary at HEAD
  local sc=$1; shift
  { echo "commit $H"; echo "timing-tests 0"; echo "scope $sc"; echo "steps $*"; echo "race-packages ./a ./b"
    for s in "$@"; do echo "ok      $s"; done; echo "cpu vet user=1.5s sys=0.5s maxrss=100MB"; echo "DONE rc=0"; } > "$d/summary.txt"
}
mk2 changed vet test-norace race-changed; expect tier2-marker 0 "<!-- local-tests sha=$H result=pass tier=2 scope=changed -->"
expect tier2-race-line 0 "race packages: ./a ./b"
expect tier2-cpu-line 0 "| vet | 1.5s | 0.5s | 100MB |"
mk2 none vet depguard; expect tier2-scope-none 0 "scope=none -->"
mk2 changed vet test-norace race-changed; sed -i.bak '/^ok      race-changed/d' "$d/summary.txt"; expect tier2-step-missing 1 "race-changed | **FAIL**: no result line"
mk2 changed vet race-changed; sed -i.bak 's/^ok      race-changed/FAILED  race-changed (twice)/' "$d/summary.txt"
printf -- '--- FAIL: TestReal (0s)\nFAIL\nFAIL\tx/a\t1s\n' > "$d/race-changed.log"; expect tier2-real-failure 1 "result=fail"
rm -f "$d/race-changed.log"

# ---- tier2-steps.sh: scope, packages and commands, from a diff in a scratch repo (--plan) -----------
t=$(mktemp -d); trap 'rm -rf "$d" "$r" "$t"' EXIT
T2=$root/scripts/dev/tier2-steps.sh
mkdir -p "$root/.tier2-scratch-guard" && rmdir "$root/.tier2-scratch-guard"
(
  cd "$t" && git init -q . && git config user.email t@t && git config user.name t
  mkdir -p config leaf pkg/a pkg/a/testdata dash/ui bob-plugin proxy internal/record scripts opencode-plugin/test tools
  mkdir -p scripts/release && cp "$root/scripts/release/part-a.sh" scripts/release/part-a.sh
  for p in config leaf pkg/a dash bob-plugin proxy internal/record; do printf 'package x\n' > "$p/x.go"; done
  printf '//go:build cg_record\npackage proxy\n' > proxy/rec.go
  printf 'package dash\nimport "embed"\n//go:embed ui\nvar f embed.FS\n' > dash/ui.go
  printf 'x\n' > dash/ui/app.js; printf 'x\n' > pkg/a/testdata/in.json
  printf 'package main\n//go:build ignore\n' > tools/gen.go
  printf 'doc\n' > README.md; printf 'module m\n' > go.mod; printf 's\n' > go.sum
  printf 't\n' > opencode-plugin/test/a.test.mjs; printf 'p\n' > opencode-plugin/index.mjs
  printf 'n\n' > dash/n.test.mjs
  git add . && git commit -qm base && git tag base
  printf 'doc2\n' > README.md; git commit -qam docs; git tag docs
  printf 'package x\n//c\n' > pkg/a/x.go; printf 'package x\n//c\n' > config/x.go; printf 'package x\n//c\n' > leaf/x.go; git commit -qam gochange; git tag gochange
  printf 'x2\n' > dash/ui/app.js; git commit -qam embed; git tag embed
  printf 'j\n' > pkg/a/testdata/in.json; git commit -qam testdata; git tag testdata
  printf 'sh\n' > scripts/a.sh; git add -A; git commit -qm script; git tag script
  printf 'module m2\n' > go.mod; git commit -qam gomod; git tag gomod
  printf 'package proxy\n//c\n' > proxy/rec.go; git commit -qam tagged; git tag tagged
  printf 'package x\n//c\n' > proxy/x.go; git commit -qam untagged; git tag untagged
  printf 'package x\n//c\n' > internal/record/x.go; printf 'package x\n//c\n' > bob-plugin/x.go; git commit -qam rec-bob; git tag recbob
  git rm -q leaf/x.go; git commit -qm delpkg; git tag delpkg
  printf 'n2\n' > dash/n.test.mjs; git commit -qam node1; git tag node1
  printf 'p2\n' > opencode-plugin/index.mjs; git commit -qam node2; git tag node2
) >/dev/null 2>&1
plan() { (cd "$t" && CG_TIMING_TESTS=0 bash "$T2" --plan --base "$1" --head "$2" 2>&1); }
pchk() { # <name> <base> <head> <must-have...> : every text must be in the plan; a text starting with ! must not be
  local name=$1 b=$2 h=$3 pl x ok=1; shift 3; pl=$(plan "$b" "$h")
  for x in "$@"; do
    if [ "${x:0:1}" = '!' ]; then grep -qF -- "${x:1}" <<<"$pl" && ok=0
    else grep -qF -- "$x" <<<"$pl" || ok=0; fi
  done
  if [ "$ok" = 1 ]; then echo "ok   $name"; else echo "FAIL $name"; echo "$pl" | head -20; fails=1; fi
}
pchk scope-none base docs "scope: none" "step vet:" "step depguard:" "!test-norace" "!race-changed" "race packages: "$'\n'
pchk scope-changed docs gochange "scope: changed" "race packages: ./config ./leaf ./pkg/a" "step race-changed:" "step test-norace:" \
  "-race -timeout 25m -p 2 -skip TestBobReleasePythonSuite ./config ./leaf ./pkg/a"
# Non-race: tier-1 packages and bob-plugin out; the others in. Timing tests off.
pchk norace-set docs gochange "step test-norace: env CGO_ENABLED=0 CG_TIMING_TESTS=0 go test -timeout 30m -p 2" " ./dash" " ./internal/record" " ./pkg/a" " ./proxy" "!test-norace: env CGO_ENABLED=0 CG_TIMING_TESTS=0 go test -timeout 30m -p 2 -skip TestBobReleasePythonSuite ./bob-plugin"
if plan docs gochange | grep '^step test-norace:' | grep -qE ' \./(config|components|apply|store)( |$)| \./proxy( |$)| \./bob-plugin( |$)'; then
  echo "FAIL norace-excludes-tier1-and-bob"; fails=1; else echo "ok   norace-excludes-tier1-and-bob"; fi
pchk scope-full script gomod "scope: full" "run: scripts/release/part-a.sh" "!step test-norace"
pchk go-mod-in-subdir-is-not-none base docs "scope: none"
pchk embedded-file-maps-to-package gochange embed "scope: changed" "race packages: ./dash"
pchk testdata-maps-to-package embed testdata "scope: changed" "race packages: ./pkg/a"
pchk script-file-is-changed testdata script "scope: changed" "race packages: "$'\n' "!race-changed"
pchk record-off-for-untagged-change tagged untagged "race packages: ./proxy" "!race-record"
pchk record-on-for-tagged-file gomod tagged "scope: changed" "race packages: ./proxy" "step race-record:" "-tags cg_record" "./proxy"
pchk record-on-for-internal-record untagged recbob "step race-record:" "-tags cg_record ./internal/record" "race packages: ./bob-plugin ./internal/record"
pchk deleted-package-skipped delpkg delpkg "scope: none"
pchk deleted-file-package-skipped recbob delpkg "scope: changed" "race packages: "$'\n' "!./leaf"
pchk node-test-file delpkg node1 "scope: none" "step node-changed: env CG_TIMING_TESTS=0 node --test dash/n.test.mjs"
pchk node-opencode-all node1 node2 "step node-changed:" "opencode-plugin/test/a.test.mjs"
pchk node-timing-skipped base docs "skip node-timing (CG_TIMING_TESTS=0)"
# The env limits in the plan.
pchk load-limits base docs "GOFLAGS=-p=2 GOMAXPROCS=4"
# classify: file names on stdin.
cl() { (cd "$t" && printf '%s\n' "$@" | bash "$T2" --classify); }
for c in "none README.md docs/x.md" "changed a/b.go" "changed dash/ui/app.js" "changed scripts/x.sh" "changed p/testdata/y" "full go.mod" "full README.md go.sum"; do
  set -- $c; w=$1; shift; g=$(cl "$@")
  if [ "$g" = "$w" ]; then echo "ok   classify $*"; else echo "FAIL classify $* = $g, want $w"; fails=1; fi
done
# The tier-1 package list in tier2-steps.sh must equal the list the CI job "purego" tests.
ci=$(grep -oE 'go test [^|]*\./config/\.\.\.[^|]*$' .github/workflows/ci.yaml | grep -oE '\./[a-z]+/\.\.\.' | sed -E 's|\./([a-z]+)/\.\.\.|\1|' | sort | tr '\n' ' ')
me=$(sed -n "s/^TIER1_RE='\^\\\\.\/(\(.*\))(\/|\$)'/\1/p" scripts/dev/tier2-steps.sh | tr '|' '\n' | sort | tr '\n' ' ')
if [ -n "$ci" ] && [ "$ci" = "$me" ]; then echo "ok   tier1-list-matches-ci ($ci)"; else echo "FAIL tier1-list-matches-ci (ci='$ci' script='$me')"; fails=1; fi

# ---- tier2-steps.sh run mode, with a fake go: the rerun of the failed packages only -----------------
fg=$d/fakego.sh
cat > "$fg" <<'FAKE'
#!/bin/sh
# A fake go. FAKE_MODE: flaky (a failure on the first run only), panic, always, ok.
echo "$*" >> "$FAKE_LOG"
[ "$1" = test ] || exit 0
case "$*" in *-race*) tag=race ;; *) tag=norace ;; esac
[ "$tag" = norace ] || exit 0
case "$*" in
  *" -run ^("*)  [ "$FAKE_MODE" = always ] && { echo "--- FAIL: TestSlow (0.00s)"; echo FAIL; printf 'FAIL\texample/leaf\t0.1s\n'; exit 1; }
                 echo "ok  	example/leaf	0.1s"; exit 0 ;;
  *" example/leaf")  echo "ok  	example/leaf	0.1s"; exit 0 ;;
  *"-p 2 -skip TestBobReleasePythonSuite ./"*" ./leaf"*)
    if [ "$FAKE_MODE" = ok ]; then echo "ok  	example/leaf	0.1s"; exit 0; fi
    echo "ok  	example/dash	0.1s"
    if [ "$FAKE_MODE" = panic ]; then echo "--- FAIL: TestA (0.00s)"; echo "panic: boom"; echo FAIL; printf 'FAIL\texample/leaf\t0.1s\n'; exit 1; fi
    echo "--- FAIL: TestSlow (0.00s)"; echo "--- FAIL: TestSlow/sub (0.00s)"; echo "--- FAIL: TestOther (0.00s)"; echo FAIL; printf 'FAIL\texample/leaf\t0.1s\n'; exit 1 ;;
esac
exit 0
FAKE
chmod +x "$fg"
runfake() { # <mode>: run tier2-steps.sh in the scratch repo with the fake go; sets $o (out dir)
  o=$d/out-$1; rm -rf "$o"; : > "$d/fake.log"
  (cd "$t" && FAKE_MODE=$1 FAKE_LOG="$d/fake.log" GO="$fg" CG_TIMING_TESTS=0 bash "$T2" --base docs --head gochange "$o" >/dev/null 2>&1); rc=$?
}
runfake flaky
if [ "$rc" = 0 ] && grep -q '^flaky   test-norace' "$o/summary.txt" && grep -q '^DONE rc=0' "$o/summary.txt" \
   && grep -qF -- '-run ^(TestSlow|TestOther)$ example/leaf' "$d/fake.log" && ! grep -E -- '-run .* example/dash' "$d/fake.log" >/dev/null; then
  echo "ok   rerun-only-failed-package-and-tests"; else echo "FAIL rerun-only-failed-package-and-tests (rc=$rc)"; cat "$o/summary.txt"; cat "$d/fake.log"; fails=1; fi
# The rerun log holds the rerun only. It has no "dash" line.
if ! grep -q 'example/dash' "$o/test-norace.rerun.log"; then echo "ok   rerun-log-only-failed"; else echo "FAIL rerun-log-only-failed"; fails=1; fi
runfake panic
if grep -qE -- ' example/leaf$' "$d/fake.log" && ! grep -qF -- '-run ^(' "$d/fake.log" && grep -q '^flaky   test-norace' "$o/summary.txt"; then
  echo "ok   rerun-whole-package-after-panic"; else echo "FAIL rerun-whole-package-after-panic"; cat "$d/fake.log"; fails=1; fi
runfake always
if [ "$rc" = 1 ] && grep -q '^FAILED  test-norace' "$o/summary.txt" && grep -q '^DONE rc=1' "$o/summary.txt"; then echo "ok   failed-twice"; else echo "FAIL failed-twice (rc=$rc)"; cat "$o/summary.txt"; fails=1; fi
runfake ok
if [ "$rc" = 0 ] && grep -q '^ok      vet' "$o/summary.txt" && grep -q '^scope changed' "$o/summary.txt" && grep -q '^steps vet .*test-norace race-changed' "$o/summary.txt" \
   && grep -q '^race-packages ./config ./leaf ./pkg/a' "$o/summary.txt"; then echo "ok   run-summary-format"; else echo "FAIL run-summary-format"; cat "$o/summary.txt"; fails=1; fi
if [ -x /usr/bin/time ]; then
  if grep -qE '^cpu vet user=[0-9.]+s sys=[0-9.]+s maxrss=[0-9]+MB$' "$o/summary.txt"; then echo "ok   cpu-line"; else echo "FAIL cpu-line"; cat "$o/summary.txt"; fails=1; fi
fi
# The summary of a run feeds local-tests.sh (end to end, no network, nothing posted).
sed -i.bak "1s/.*/commit $H/" "$o/summary.txt"
out=$(scripts/dev/local-tests.sh --dry-run --reuse "$o" 1 2>&1); rc=$?
if [ "$rc" = 0 ] && grep -qF "tier=2 scope=changed -->" <<<"$out"; then echo "ok   tier2-run-feeds-local-tests"; else echo "FAIL tier2-run-feeds-local-tests (rc=$rc)"; echo "$out" | head; fails=1; fi
exit "$fails"
