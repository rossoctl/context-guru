#!/usr/bin/env bash
# part-a.sh [<out-dir>]: run the integration check's Part A (RELEASING.md, step 5) in this
# checkout, on a machine with Go and a C compiler (cgo is needed for -race).
#
# This is tier 3 of the test plan: every step, all packages, with -race. The PR owner's checks
# (tier 2) are scripts/dev/local-tests.sh. The scheduled GitHub run is the other tier-3 half
# (.github/workflows/ci.yaml). See RELEASING.md, "Heavy CI on main".
#
# CPU cost: each run of a step adds a line "time    <step> user=<s>s sys=<s>s maxrss_kb=<n>" to the
# summary ("?" marks a value the script could not parse from the time output)
# (the step name has ".rerun" added for a second run). It comes from /usr/bin/time: -l on
# macOS, -v on Linux. With no usable /usr/bin/time the steps run as before, with no time line.
# local-tests.sh reads summary.txt, and only the lines that start with ok, flaky, FAILED, skip,
# commit, timing-tests and DONE, so the time lines do not change its result. check-ready.sh does
# not read summary.txt.
#
# Each step writes <out-dir>/<step>.log. The summary is <out-dir>/summary.txt, and its last line
# is "DONE rc=<n>" on every exit except SIGKILL: also when a step fails or the script is stopped
# (a stop gives rc=143), so a poller can wait for that line. rc 0: every step passed. A failed step is run once more; the summary says
# "flaky" when the second run passed, and "FAILED" when both failed.
#
# The whole run takes about 70 to 90 minutes on a laptop. The race steps have these limits: proxy 20m,
# dash 40m, plugin 30m, rest 20m, record 40m. Run it detached, and wait for the DONE line.
#
# CG_TIMING_TESTS: the timing tests (testutil.RequireTiming) run when it is 1. The default here is 1,
# because the integrator runs this script. scripts/dev/local-tests.sh sets it to 0 or 1 itself. The
# summary records the value in a "timing-tests" line.
#
# The step node-timing needs node 22 (the opencode-plugin workflow uses it too).
#
# GO: the go binary (default: go on PATH, else /usr/local/go/bin/go).
set -uo pipefail
out=${1:-$PWD/.part-a}
mkdir -p "$out"
sum=$out/summary.txt
: > "$sum"
rc=0
child=
trap 'echo "DONE rc=$rc" >> "$sum"' EXIT
# A stop must not read as a pass: without this, the EXIT trap writes the rc=0 it started with.
# Bash runs a trap only when the foreground command ends, so each step runs in the background and
# the script waits for it; the trap then stops the step at once instead of after it finishes.
# With the time wrapper the child is /usr/bin/time: stop its own child first. pkill -P <pid> signals
# only the children of that one known pid. Never change it to a name pattern (shared box).
trap 'rc=143; [ -n "$child" ] && { pkill -TERM -P "$child" 2>/dev/null; kill -TERM "$child" 2>/dev/null; }; exit 143' HUP INT TERM
GO=${GO:-$(command -v go || echo /usr/local/go/bin/go)}
echo "commit $(git rev-parse --verify -q HEAD 2>/dev/null || echo unknown)" >> "$sum"
export CG_TIMING_TESTS=${CG_TIMING_TESTS-1}
echo "timing-tests $CG_TIMING_TESTS" >> "$sum"

# Pick the time flavour: BSD time (macOS) takes -l, GNU time (Linux) takes -v. Probe it once.
timemode=
tprobe=$(mktemp "${TMPDIR:-/tmp}/part-a-time.XXXXXX")
if [ -x /usr/bin/time ]; then
  case $(uname -s) in
    Darwin) /usr/bin/time -l -o "$tprobe" true 2>/dev/null && grep -q 'maximum resident set size' "$tprobe" && timemode=bsd ;;
    *)      /usr/bin/time -v -o "$tprobe" true 2>/dev/null && grep -q 'Maximum resident set size' "$tprobe" && timemode=gnu ;;
  esac
fi
rm -f "$tprobe"

# timeline <name> <file>: add "time <name> user=.. sys=.. maxrss_kb=.." to the summary.
timeline() {
  local name=$1 f=$2 u sy rss
  case $timemode in
    bsd) u=$(awk '/ user /{print $3}' "$f") sy=$(awk '/ user /{print $5}' "$f")
         rss=$(awk '/maximum resident set size/{printf "%d", $1/1024}' "$f") ;; # macOS gives bytes
    gnu) u=$(awk -F': ' '/User time \(seconds\)/{print $2}' "$f") sy=$(awk -F': ' '/System time \(seconds\)/{print $2}' "$f")
         rss=$(awk -F': ' '/Maximum resident set size/{print $2}' "$f") ;;
  esac
  # A value that is not a number (a time format we do not know) shows as "?".
  for v in u sy rss; do [[ ${!v} =~ ^[0-9]+(\.[0-9]+)?$ ]] || printf -v "$v" '?'; done
  echo "time    $name user=${u:-?}s sys=${sy:-?}s maxrss_kb=${rss:-?}" >> "$sum"
}

# run <log> <cmd...>: run the command in the background, wait for it, and return its exit code.
# The step name is the log's base name without .log. With a time mode, the command runs under time.
run() {
  local log=$1; shift
  local tf= name
  name=$(basename "$log" .log)
  if [ -n "$timemode" ]; then
    tf=$(mktemp "${TMPDIR:-/tmp}/part-a-time.XXXXXX")
    case $timemode in
      bsd) /usr/bin/time -l -o "$tf" "$@" > "$log" 2>&1 & ;;
      gnu) /usr/bin/time -v -o "$tf" "$@" > "$log" 2>&1 & ;;
    esac
  else
    "$@" > "$log" 2>&1 &
  fi
  child=$!
  wait "$child"
  local r=$?
  child=
  if [ -n "$tf" ]; then timeline "$name" "$tf"; rm -f "$tf"; fi
  return "$r"
}

# step <name> <cmd...>: run the command, and run it once more if it fails.
step() {
  local name=$1; shift
  if run "$out/$name.log" "$@"; then echo "ok      $name" >> "$sum"; return; fi
  if run "$out/$name.rerun.log" "$@"; then echo "flaky   $name (passed on the 2nd run; see $name.log)" >> "$sum"; return; fi
  echo "FAILED  $name (twice; see $name.log, $name.rerun.log)" >> "$sum"
  rc=1
}

skipnote="skipped TestBobReleasePythonSuite in race-rest (Bob plugin on hold, #1221)"

rest=$("$GO" list ./... | grep -vE '/(proxy|dash|context-guru-plugin)(/|$)')

step vet           "$GO" vet ./...
step build-purego  env CGO_ENABLED=0 "$GO" build ./...
step build-record  env CGO_ENABLED=0 "$GO" build -tags cg_record ./...
step vet-record    "$GO" vet -tags cg_record ./...
# The default build must not link otel, grpc or protobuf; only -tags cg_record may. CGO_ENABLED=0,
# as CI runs it: the pure-Go build is the one that ships.
step depguard      env CGO_ENABLED=0 "$GO" test -run TestDefaultBuildLinksNoOpenTelemetryGRPCOrProtobuf -count=1 ./proxyapp/
step race-proxy    env CGO_ENABLED=1 "$GO" test -race -timeout 20m ./proxy/...
# 40m, like race-record: on the shared laptop (load 190-400) dash took 1507-1514 s and hit 25m twice.
step race-dash     env CGO_ENABLED=1 "$GO" test -race -timeout 40m ./dash/...
step race-plugin   env CGO_ENABLED=1 "$GO" test -race -timeout 30m ./context-guru-plugin
# shellcheck disable=SC2086 # $rest is a list of packages
# The Bob plugin is on hold (#1221): skip its install suite. Remove the skip when the owner resumes it.
step race-rest     env CGO_ENABLED=1 "$GO" test -race -timeout 20m -skip TestBobReleasePythonSuite $rest
step race-record   env CGO_ENABLED=1 "$GO" test -race -timeout 40m -tags cg_record -p 1 ./proxy/... ./internal/record/... ./tenant/... ./proxyapp/...
# The OpenCode plugin's node tests. Only the timing tests need this step: the opencode-plugin workflow
# runs the rest on every PR. With CG_TIMING_TESTS=0 the step is skipped, and the summary says so.
if [ "$CG_TIMING_TESTS" = 1 ]; then
  step node-timing env CG_TIMING_TESTS=1 node --test opencode-plugin/test/*.test.mjs
else
  echo "skip    node-timing (CG_TIMING_TESTS=$CG_TIMING_TESTS: the timing tests are off)" >> "$sum"
fi
echo "$skipnote" >> "$sum"
exit "$rc"
