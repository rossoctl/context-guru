#!/usr/bin/env bash
# Gate A: can context-guru ship as a static binary with no C toolchain?
#
# Runs in a golang container, so it needs no local Go install. A named volume holds the
# module cache so the matrix downloads once rather than once per target.
#
#   1. default tags, CGO_ENABLED=0        -> must SUCCEED (the binary we want to ship)
#   2. -tags cg_skeleton, CGO_ENABLED=0   -> must FAIL (tree-sitter is the only cgo dep)
#   3. GOOS/GOARCH matrix, CGO_ENABLED=0  -> must SUCCEED for all four release targets
#   4. artifact size and linkage
#
# Results as of 2026-08-30 (go 1.26.4, module github.com/rossoctl/context-guru):
#   1 PASS · 2 PASS (fails with "build constraints exclude all Go files", the
#   cgo-disabled signature) · 3 PASS on all four · 4 30.5 MB stripped, "not a dynamic
#   executable".
#
# Note: `go mod download all` is flaky against proxy.golang.org from here and may report
# failures while the builds still succeed — `go build` fetches only what it needs.
set -uo pipefail
cd "$(dirname "$0")/.."

IMG="${IMG:-golang:1.26}"
VOL="${VOL:-cg-gomodcache}"
LOG="${LOG:-/tmp/gate-a.log}"
: >"$LOG"

run() { docker run --rm -v "$PWD":/src -v "$VOL":/go/pkg/mod -w /src \
        -e GOFLAGS=-buildvcs=false "$@"; }

result() { # name, expected(ok|fail), exit code
  local name="$1" want="$2" code="$3" verdict
  if [[ "$want" == ok ]]; then
    [[ "$code" -eq 0 ]] && verdict="PASS" || verdict="FAIL"
  else
    [[ "$code" -ne 0 ]] && verdict="PASS (failed as expected)" || verdict="UNEXPECTED SUCCESS"
  fi
  printf '%-38s exit=%-3s %s\n' "$name" "$code" "$verdict" | tee -a "$LOG"
}

echo "== prewarming module cache (best effort, up to 3 attempts) ==" | tee -a "$LOG"
for i in 1 2 3; do
  run -e CGO_ENABLED=0 "$IMG" go mod download all >>"$LOG" 2>&1 && { echo "  ok on attempt $i" | tee -a "$LOG"; break; }
  echo "  attempt $i failed (builds may still succeed)" | tee -a "$LOG"
done

echo | tee -a "$LOG"
echo "== 1. default tags, CGO_ENABLED=0 (want SUCCESS) ==" | tee -a "$LOG"
run -e CGO_ENABLED=0 "$IMG" go build -o /tmp/cg ./cmd/context-guru-proxy >>"$LOG" 2>&1
result "default tags / CGO=0" ok $?

echo | tee -a "$LOG"
echo "== 2. -tags cg_skeleton, CGO_ENABLED=0 (want FAILURE) ==" | tee -a "$LOG"
echo "-- fetching tree-sitter grammars first, so a network error cannot be mistaken" | tee -a "$LOG"
echo "-- for the cgo error we are actually testing for" | tee -a "$LOG"
grammars=no
for i in 1 2 3 4 5; do
  run -e CGO_ENABLED=0 "$IMG" go mod download github.com/alexaandru/go-sitter-forest/c >/dev/null 2>&1 \
    && { grammars=yes; echo "   fetched on attempt $i" | tee -a "$LOG"; break; }
  echo "   attempt $i failed" | tee -a "$LOG"
done
if [[ "$grammars" == yes ]]; then
  run -e CGO_ENABLED=0 "$IMG" go build -tags cg_skeleton -o /tmp/cg-skel ./cmd/context-guru-proxy >>"$LOG" 2>&1
  result "cg_skeleton / CGO=0" fail $?
  grep -m1 "build constraints exclude" "$LOG" >/dev/null \
    && echo "   reason: \"build constraints exclude all Go files\" = cgo disabled" | tee -a "$LOG"
else
  echo "cg_skeleton / CGO=0                    INCONCLUSIVE (could not fetch grammars)" | tee -a "$LOG"
fi

echo | tee -a "$LOG"
echo "== 3. cross-compile matrix, CGO_ENABLED=0 ==" | tee -a "$LOG"
for t in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  os="${t%/*}"; arch="${t#*/}"
  run -e CGO_ENABLED=0 -e GOOS="$os" -e GOARCH="$arch" "$IMG" \
    go build -o "/tmp/cg-$os-$arch" ./cmd/context-guru-proxy >>"$LOG" 2>&1
  result "$t" ok $?
done

echo | tee -a "$LOG"
echo "== 4. artifact size and linkage (linux/amd64) ==" | tee -a "$LOG"
# Inspected inside the container: Rancher Desktop does not share /tmp with the host.
run -e CGO_ENABLED=0 -e GOOS=linux -e GOARCH=amd64 "$IMG" sh -c '
  set -e
  go build -ldflags="-s -w" -o /tmp/cg ./cmd/context-guru-proxy
  ls -l /tmp/cg | awk "{printf \"stripped:   %.1f MB\n\", \$5/1048576}"
  go build -o /tmp/cg-plain ./cmd/context-guru-proxy
  ls -l /tmp/cg-plain | awk "{printf \"unstripped: %.1f MB\n\", \$5/1048576}"
  printf "linkage:    "; ldd /tmp/cg 2>&1 | head -1
' 2>&1 | tee -a "$LOG"

echo | tee -a "$LOG"
echo "full log: $LOG"
