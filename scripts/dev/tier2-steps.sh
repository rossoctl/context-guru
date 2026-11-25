#!/usr/bin/env bash
# tier2-steps.sh [--plan] [--base <ref>] [<out-dir>]
# tier2-steps.sh --classify          (read file names on stdin, print the scope)
#
# Tier 2 of the test plan (CLAUDE.md, "Test tiers"): the lean test run of one PR, on a laptop.
# scripts/dev/local-tests.sh calls it. It replaces the full run of scripts/release/part-a.sh
# (which is now tier 3 only). The rules, from the owner-approved proposal (option E, no slot lock):
#   1. The static steps always: vet, build-purego, build-record, vet-record, depguard.
#   2. A scope, from the diff against the merge base of HEAD and origin/main:
#        none     no .go file, go.mod, go.sum, testdata file, embedded file or scripts/ file.
#        full     go.mod or go.sum changed. Run the whole of part-a.sh.
#        changed  anything else.
#   3. Non-race tests, once, for the packages the tier-1 CI job "purego" does not run
#      (TIER1_RE below must match the "go test" line of that job in .github/workflows/ci.yaml).
#      bob-plugin (on hold, #1221) runs only when the PR changes it.
#   4. "-race" on the changed packages only. proxy and its neighbours run with "-tags cg_record"
#      too, only when the PR changes a file with that build tag, or internal/record.
#   5. Node tests: the changed *.test.mjs files, and all of opencode-plugin/test when the PR changes
#      opencode-plugin. The step node-timing runs as in part-a.sh (only with CG_TIMING_TESTS=1).
#   6. A failed Go test step reruns only the failed packages (and only the failed tests, with -run).
#   7. Load limits: GOFLAGS gets -p=2, GOMAXPROCS is 4. Each step runs under /usr/bin/time, and
#      summary.txt gets one "cpu <step> user= sys= maxrss=" line per step.
#
# Output: <out-dir>/<step>.log, and <out-dir>/summary.txt in the format of part-a.sh:
#   commit <sha> / timing-tests <0|1> / scope <s> / steps <names> / race-packages <list>
#   ok|flaky|FAILED|skip <step> ... / cpu <step> ... / DONE rc=<n> (also on a stop: rc=143).
#
# --plan          print the scope and the commands that would run. Run nothing. Needs no Go.
# --base <ref>    the base of the diff (default: merge base of HEAD and origin/main; or CG_TIER2_BASE).
#                 If no base is found, the scope is "full": a check that cannot decide does not skip.
# --head <ref>    the head of the diff and the tree to read (default HEAD). For --plan on an old PR.
# --classify      read changed file names on stdin, print none|changed|full. check-ready.sh uses it.
#
# CG_TIMING_TESTS: as part-a.sh (default 1 there). local-tests.sh sets it. Go test steps of the
# non-race step always run with 0.
# CG_PART_A: the part-a.sh to call for scope full (for the tests).
# GO: the go binary (default: go on PATH, else /usr/local/go/bin/go).
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(git rev-parse --show-toplevel) || { echo "ERROR: not in a git checkout" >&2; exit 2; }
cd "$root" || exit 2

# Packages that the CI job "purego" already tests without -race (ci.yaml): config, components,
# apply, proxy, store. A drift test in local-tests_test.sh keeps this in step with the workflow.
TIER1_RE='^\./(config|components|apply|proxy|store)(/|$)'
# -race time limits per package. They match scripts/release/part-a.sh (race-dash 40m, race-plugin 30m,
# race-record 40m): on a loaded laptop dash took 1507-1514 s and hit 25m twice (#1381). The limit
# is per test binary, so dash and the plugin run in their own "go test" call. Every other package
# gets the default. If part-a.sh changes a limit, change it here too.
RACE_TIMEOUT_DEFAULT=25m
RACE_TIMEOUT_DASH=40m
RACE_TIMEOUT_PLUGIN=30m
RACE_TIMEOUT_RECORD=40m
# Packages whose tests change with the build tag cg_record (the old race-record step).
RECORD_RE='^\./(proxy|proxyapp|internal/record|tenant)(/|$)'

plan=0; base=${CG_TIER2_BASE:-}; head=HEAD; mode=run; out=
while [ $# -gt 0 ]; do
  case $1 in
    --plan) plan=1; shift ;;
    --base) base=${2:?--base needs a ref}; shift 2 ;;
    --head) head=${2:?--head needs a ref}; shift 2 ;;
    --classify) mode=classify; shift ;;
    -*) echo "ERROR: unknown option $1" >&2; exit 2 ;;
    *) out=$1; shift ;;
  esac
done

# ---- scope ----------------------------------------------------------------------------------
# The embedded files: every //go:embed pattern, as a path from the repo root.
EMB=()
while IFS= read -r line; do
  f=${line%%:*}; rest=${line#*//go:embed}; d=$(dirname "$f")
  for pat in $rest; do
    pat=${pat#all:}; pat=${pat//\"/}
    [ "$d" = . ] && EMB+=("$pat") || EMB+=("$d/$pat")
  done
done < <(git grep -n -H '^//go:embed ' "$head" -- '*.go' | sed -E 's/^[^:]+:([^:]+):[0-9]+:/\1:/')

is_embedded() { local f=$1 e; for e in "${EMB[@]+"${EMB[@]}"}"; do
  # shellcheck disable=SC2053 # $e is a glob on purpose
  [[ $f == $e || $f == "$e"/* ]] && return 0; done; return 1; }

# classify: file names on stdin. Prints none, changed or full.
classify() {
  local s=none f
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    case $f in
      go.mod|go.sum|*/go.mod|*/go.sum) echo full; return ;;
      *.go|testdata/*|*/testdata/*|scripts/*) s=changed ;;
      *) is_embedded "$f" && s=changed ;;
    esac
  done
  echo "$s"
}
if [ "$mode" = classify ]; then classify; exit 0; fi

# ---- the diff ---------------------------------------------------------------------------------
[ -n "$base" ] || base=$(git merge-base "$head" origin/main 2>/dev/null) || base=
files=
if [ -n "$base" ]; then files=$(git diff --name-only --no-renames "$base" "$head") || base=; fi
if [ -z "$base" ]; then scope=full; else scope=$(classify <<<"$files"); fi

# Package dirs that hold a Go file at HEAD (like "go list ./...": no testdata, no _dir, no .dir).
GODIRS=$(git ls-tree -r --name-only "$head" | grep '\.go$' | awk -F/ '{ d="."; ok=1
    for (i=1;i<NF;i++) { if ($i=="testdata" || $i ~ /^[_.]/) ok=0; d = (d=="." ? $i : d "/" $i) }
    if (ok) print d }' | sort -u)
has_go() { grep -qxF -- "$1" <<<"$GODIRS"; }
pkgarg() { if [ "$1" = . ]; then echo .; else echo "./$1"; fi; }

# Changed packages: a .go file maps to its dir (gone when the package is deleted). A testdata or
# embedded file maps to the nearest dir above it that holds Go files.
race=()
if [ "$scope" = changed ]; then
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    d=; walk=0
    case $f in
      *.go) d=$(dirname "$f") ;;
      testdata/*) d=. ; walk=1 ;;
      */testdata/*) d=${f%%/testdata/*}; walk=1 ;;
      *) if is_embedded "$f"; then d=$(dirname "$f"); walk=1; else continue; fi ;;
    esac
    if [ "$walk" = 1 ]; then while [ "$d" != . ] && ! has_go "$d"; do d=$(dirname "$d"); done; fi
    has_go "$d" && race+=("$(pkgarg "$d")")
  done <<<"$files"
  if [ "${#race[@]}" -gt 0 ]; then
    sorted=(); while IFS= read -r x; do sorted+=("$x"); done < <(printf '%s\n' "${race[@]}" | sort -u); race=("${sorted[@]}")
  fi
fi

# Split the race set: dash and the plugin each get their own step and limit.
racedash=(); raceplug=(); racerest=()
for p in "${race[@]+"${race[@]}"}"; do
  case $p in
    ./dash) racedash+=("$p") ;;
    ./context-guru-plugin) raceplug+=("$p") ;;
    *) racerest+=("$p") ;;
  esac
done

# The cg_record variant: a changed file (now or at the base) with a //go:build line that names
# cg_record, or any file of internal/record.
record=0; recpk=()
if [ "$scope" = changed ]; then
  while IFS= read -r f; do
    case $f in
      internal/record/*) record=1 ;;
      *.go) if { git show "$head:$f"; git show "$base:$f"; } 2>/dev/null | grep -qE '^//go:build.*cg_record'; then record=1; fi ;;
    esac
  done <<<"$files"
  for p in "${race[@]+"${race[@]}"}"; do grep -qE "$RECORD_RE" <<<"$p" && recpk+=("$p"); done
  [ "${#recpk[@]}" -gt 0 ] || record=0
fi

# The non-race packages: all except the tier-1 set. bob-plugin only when it changed.
norace=()
if [ "$scope" != none ] && [ "$scope" != full ]; then
  while IFS= read -r d; do
    p=$(pkgarg "$d")
    grep -qE "$TIER1_RE" <<<"$p" && continue
    if grep -qE '^\./bob-plugin(/|$)' <<<"$p"; then
      printf '%s\n' "${race[@]+"${race[@]}"}" | grep -qE '^\./bob-plugin(/|$)' || continue
    fi
    norace+=("$p")
  done <<<"$GODIRS"
fi

# Node tests.
nodefiles=()
if [ "$scope" != full ]; then
  allop=0
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    git cat-file -e "$head:$f" 2>/dev/null || continue
    case $f in
      opencode-plugin/*) allop=1 ;;
      *.test.mjs) nodefiles+=("$f") ;;
    esac
  done <<<"$files"
  if [ "$allop" = 1 ] || [ "${#nodefiles[@]}" -gt 0 ]; then
    [ "$allop" = 0 ] || nodefiles+=($(git ls-tree -r --name-only "$head" opencode-plugin/test/ | grep '\.test\.mjs$'))
    sorted=(); while IFS= read -r x; do sorted+=("$x"); done < <(printf '%s\n' "${nodefiles[@]}" | sort -u); nodefiles=("${sorted[@]}")
  fi
fi

GO=${GO:-$(command -v go || echo /usr/local/go/bin/go)}
GOFLAGS_ORIG=${GOFLAGS-}; GOMAXPROCS_ORIG=${GOMAXPROCS-}
export GOFLAGS="${GOFLAGS:+$GOFLAGS }-p=2" GOMAXPROCS=4
export CG_TIMING_TESTS=${CG_TIMING_TESTS-1}
SKIPBOB=(-skip TestBobReleasePythonSuite)

# The step names, in run order. For scope full they are the steps of part-a.sh.
names=()
if [ "$scope" = full ]; then
  while IFS= read -r s; do names+=("$s"); done < <(grep -oE '^[[:space:]]*step [a-z0-9-]+' scripts/release/part-a.sh | awk '{print $2}')
else
  names=(vet build-purego build-record vet-record depguard)
  [ "$scope" = none ] || { [ "${#norace[@]}" -eq 0 ] || names+=(test-norace); [ "${#racerest[@]}" -eq 0 ] || names+=(race-changed); [ "${#racedash[@]}" -eq 0 ] || names+=(race-dash); [ "${#raceplug[@]}" -eq 0 ] || names+=(race-plugin); [ "$record" = 0 ] || names+=(race-record); }
  [ "${#nodefiles[@]}" -eq 0 ] || names+=(node-changed)
  names+=(node-timing)
fi

# ---- plan -------------------------------------------------------------------------------------
# Each step: a prefix command P, then packages K (Go test steps), or the whole command in P.
declare -a P K
define_step() { # sets P and K for a step name
  K=()
  case $1 in
    vet)          P=("$GO" vet ./...) ;;
    build-purego) P=(env CGO_ENABLED=0 "$GO" build ./...) ;;
    build-record) P=(env CGO_ENABLED=0 "$GO" build -tags cg_record ./...) ;;
    vet-record)   P=("$GO" vet -tags cg_record ./...) ;;
    depguard)     P=(env CGO_ENABLED=0 "$GO" test -run TestDefaultBuildLinksNoOpenTelemetryGRPCOrProtobuf -count=1 ./proxyapp/) ;;
    test-norace)  P=(env CGO_ENABLED=0 CG_TIMING_TESTS=0 "$GO" test -timeout 30m -p 2 "${SKIPBOB[@]}"); K=("${norace[@]}") ;;
    race-changed) P=(env CGO_ENABLED=1 "$GO" test -race -timeout "$RACE_TIMEOUT_DEFAULT" -p 2 "${SKIPBOB[@]}"); K=("${racerest[@]}") ;;
    race-dash)    P=(env CGO_ENABLED=1 "$GO" test -race -timeout "$RACE_TIMEOUT_DASH" -p 2); K=("${racedash[@]}") ;;
    race-plugin)  P=(env CGO_ENABLED=1 "$GO" test -race -timeout "$RACE_TIMEOUT_PLUGIN" -p 2); K=("${raceplug[@]}") ;;
    # Part A runs this with -p 1; tier 2 uses -p 2 (the load limit) and the same 40m limit.
    race-record)  P=(env CGO_ENABLED=1 "$GO" test -race -timeout "$RACE_TIMEOUT_RECORD" -p 2 -tags cg_record); K=("${recpk[@]}") ;;
    node-changed) P=(env CG_TIMING_TESTS=0 node --test "${nodefiles[@]}") ;;
    node-timing)  P=(env CG_TIMING_TESTS=1 node --test "opencode-plugin/test/*.test.mjs") ;;
  esac
}
join() { local IFS=' '; echo "$*"; }

if [ "$plan" = 1 ]; then
  echo "base: ${base:-none}"
  echo "scope: $scope"
  echo "env: GOFLAGS=$GOFLAGS GOMAXPROCS=$GOMAXPROCS"
  echo "race packages: $(join "${race[@]+"${race[@]}"}")"
  if [ "$scope" = full ]; then
    echo "run: scripts/release/part-a.sh (all steps: $(join "${names[@]}"))"
  else
    for n in "${names[@]}"; do
      define_step "$n"
      if [ "$n" = node-timing ] && [ "$CG_TIMING_TESTS" != 1 ]; then echo "skip $n (CG_TIMING_TESTS=$CG_TIMING_TESTS)"; continue; fi
      line=$(join "${P[@]}" "${K[@]+"${K[@]}"}"); echo "step $n: ${line//$GO/go}"
    done
  fi
  exit 0
fi

# ---- run --------------------------------------------------------------------------------------
out=${out:-$PWD/.tier2}
mkdir -p "$out"
sum=$out/summary.txt
: > "$sum"
rc=0; child=
trap 'echo "DONE rc=$rc" >> "$sum"' EXIT
trap 'rc=143; [ -n "$child" ] && kill -TERM "$child" 2>/dev/null; exit 143' HUP INT TERM
{
  echo "commit $(git rev-parse --verify -q HEAD 2>/dev/null || echo unknown)"
  echo "timing-tests $CG_TIMING_TESTS"
  echo "scope $scope"
  echo "steps $(join "${names[@]}")"
  echo "race-packages $(join "${race[@]+"${race[@]}"}")"
} >> "$sum"

# The time wrapper: BSD time on macOS (-l), GNU time on Linux (-v). Without it, no cpu line.
timebin=/usr/bin/time; timeflag=-v; [ "$(uname -s)" = Darwin ] && timeflag=-l
[ -x "$timebin" ] || timebin=

# trun <log> <timefile> <cmd...>: run in the background and wait, so a stop signal acts at once.
trun() {
  local log=$1 tf=$2; shift 2
  if [ -n "$timebin" ]; then "$timebin" "$timeflag" -o "$tf" "$@" > "$log" 2>&1 &
  else "$@" > "$log" 2>&1 & fi
  child=$!; wait "$child"; local r=$?; child=; return "$r"
}

# cpu_line <step>: add up the user and system time of the step's runs, and the biggest memory.
cpu_line() {
  local f=() t
  for t in "$out/$1.time" "$out/$1".rerun*.time; do [ -s "$t" ] && f+=("$t"); done
  [ "${#f[@]}" -gt 0 ] || return 0
  awk -v n="$1" '
    / real +[0-9.]+ user +[0-9.]+ sys/ || / user .* sys/ { u+=$3; s+=$5 }
    /User time \(seconds\)/ { u+=$NF }
    /System time \(seconds\)/ { s+=$NF }
    /maximum resident set size/ && $1 ~ /^[0-9]+$/ { v=$1/1048576; if (v>m) m=v }
    /Maximum resident set size \(kbytes\)/ { v=$NF/1024; if (v>m) m=v }
    END { printf "cpu %s user=%.1fs sys=%.1fs maxrss=%dMB\n", n, u, s, m }' "${f[@]}" >> "$sum"
}

# failed_pkgs <log>: print "<pkg><TAB><Test1|Test2>" per failed package. A "*" means rerun the whole
# package: it has no failed test name, or it panicked or timed out (the later tests never ran).
failed_pkgs() {
  awk '
    /^--- FAIL: / { n=$3; sub(/\/.*/,"",n); if (!(n in seen)) { seen[n]=1; names = names (names==""?"":"|") n } }
    /^panic: / || /test timed out/ { whole=1 }
    /^FAIL[ \t]+[^ \t]+/ { print $2 "\t" ((whole || names=="") ? "*" : names); names=""; whole=0; delete seen }' "$1"
}

# step <name>: run the command of define_step. A failed Go test step reruns only the failed packages.
step() {
  local name=$1 log=$out/$1.log
  define_step "$name"
  if trun "$log" "$out/$name.time" "${P[@]}" "${K[@]+"${K[@]}"}"; then echo "ok      $name" >> "$sum"; cpu_line "$name"; return; fi
  local rr=$out/$name.rerun.log r=0 reran=0 nrr=0
  : > "$rr"
  if [ "${#K[@]}" -gt 0 ]; then
    local pk tests part
    while IFS=$'\t' read -r pk tests; do
      [ -n "$pk" ] || continue
      if [ "$tests" = "*" ]; then set -- "${P[@]}" "$pk"; else set -- "${P[@]}" -run "^($tests)\$" "$pk"; fi
      part=$out/$name.rerun.part; reran=1
      nrr=$((nrr+1))
      trun "$part" "$out/$name.rerun$nrr.time" "$@" || r=1
      cat "$part" >> "$rr"; rm -f "$part"
    done < <(failed_pkgs "$log")
    # A failure with no "FAIL <pkg>" line (a crash before any package ran) reruns the whole step.
    if [ "$reran" = 0 ]; then trun "$rr" "$out/$name.rerun0.time" "${P[@]}" "${K[@]}" || r=1; fi
  else
    trun "$rr" "$out/$name.rerun0.time" "${P[@]}" || r=1
  fi
  cpu_line "$name"
  if [ "$r" = 0 ]; then echo "flaky   $name (passed on the 2nd run; see $name.log)" >> "$sum"; return; fi
  echo "FAILED  $name (twice; see $name.log, $name.rerun.log)" >> "$sum"
  rc=1
}

if [ "$scope" = full ]; then
  # Today's full suite. part-a.sh writes its own summary: run it in a sub-directory, then merge.
  mkdir -p "$out/part-a"
  # The load limits are for tier 2 only: Part A runs with the caller's own GOFLAGS and GOMAXPROCS.
  env -u GOMAXPROCS ${GOMAXPROCS_ORIG:+GOMAXPROCS=$GOMAXPROCS_ORIG} GOFLAGS="$GOFLAGS_ORIG" CG_TIMING_TESTS=$CG_TIMING_TESTS "${CG_PART_A:-$here/../release/part-a.sh}" "$out/part-a" || true
  grep -vE '^(commit|timing-tests|DONE) ' "$out/part-a/summary.txt" >> "$sum"
  prc=$(sed -n 's/^DONE rc=//p' "$out/part-a/summary.txt" | tail -1)
  rc=${prc:-1}
  find "$out/part-a" -maxdepth 1 -type f ! -name summary.txt -exec mv {} "$out"/ \;
  exit "$rc"
fi

for n in "${names[@]}"; do
  if [ "$n" = node-timing ]; then
    if [ "$CG_TIMING_TESTS" = 1 ]; then step node-timing
    else echo "skip    node-timing (CG_TIMING_TESTS=$CG_TIMING_TESTS: the timing tests are off)" >> "$sum"; fi
    continue
  fi
  step "$n"
done
echo "skipped TestBobReleasePythonSuite (Bob plugin on hold, #1221)" >> "$sum"
exit "$rc"
