#!/usr/bin/env bash
# part-a.sh [<out-dir>]: run the integration check's Part A (RELEASING.md, step 5) in this
# checkout, on a machine with Go and a C compiler (cgo is needed for -race).
#
# Each step writes <out-dir>/<step>.log. The summary is <out-dir>/summary.txt, and its last line
# is "DONE rc=<n>" on every exit except SIGKILL: also when a step fails or the script is stopped
# (a stop gives rc=143), so a poller can wait for that line. rc 0: every step passed. A failed step is run once more; the summary says
# "flaky" when the second run passed, and "FAILED" when both failed.
#
# The whole run takes about 40 minutes. The dash race tests alone need up to 25 minutes on a
# loaded machine, which is why they get -timeout 25m. Run it detached, and wait for the DONE line.
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

rest=$("$GO" list ./... | grep -vE '/(proxy|dash|context-guru-plugin)(/|$)')

step vet           "$GO" vet ./...
step build-purego  env CGO_ENABLED=0 "$GO" build ./...
step build-record  env CGO_ENABLED=0 "$GO" build -tags cg_record ./...
step vet-record    "$GO" vet -tags cg_record ./...
# The default build must not link otel, grpc or protobuf; only -tags cg_record may. CGO_ENABLED=0,
# as CI runs it: the pure-Go build is the one that ships.
step depguard      env CGO_ENABLED=0 "$GO" test -run TestDefaultBuildLinksNoOpenTelemetryGRPCOrProtobuf -count=1 ./cmd/context-guru-proxy/
step race-proxy    env CGO_ENABLED=1 "$GO" test -race -timeout 20m ./proxy/...
step race-dash     env CGO_ENABLED=1 "$GO" test -race -timeout 25m ./dash/...
step race-plugin   env CGO_ENABLED=1 "$GO" test -race -timeout 10m ./context-guru-plugin
# shellcheck disable=SC2086 # $rest is a list of packages
step race-rest     env CGO_ENABLED=1 "$GO" test -race -timeout 20m $rest
step race-record   env CGO_ENABLED=1 "$GO" test -race -tags cg_record -p 1 ./proxy/... ./internal/record/... ./tenant/... ./cmd/context-guru-proxy/...
exit "$rc"
