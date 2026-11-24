#!/usr/bin/env bash
# part-a.sh [<out-dir>]: run the integration check's Part A (RELEASING.md, step 5) in this
# checkout, on a machine with Go and a C compiler (cgo is needed for -race).
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
trap 'rc=143; [ -n "$child" ] && kill -TERM "$child" 2>/dev/null; exit 143' HUP INT TERM
GO=${GO:-$(command -v go || echo /usr/local/go/bin/go)}
echo "commit $(git rev-parse --verify -q HEAD 2>/dev/null || echo unknown)" >> "$sum"
export CG_TIMING_TESTS=${CG_TIMING_TESTS-1}
echo "timing-tests $CG_TIMING_TESTS" >> "$sum"

# run <log> <cmd...>: run the command in the background, wait for it, and return its exit code.
run() {
  local log=$1; shift
  "$@" > "$log" 2>&1 &
  child=$!
  wait "$child"
  local r=$?
  child=
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
