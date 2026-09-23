#!/bin/bash
# iter025: TWO arms x 15 tasks x 5 seeds at 64k = 150 runs. Asks whether iteration 024's REWARD result
# survives a gate that can decline: its econ trigger authorised 9 asks out of 9 because the break-even
# charged the cache-write and never the adjudication call. Configs are byte-identical to iterations 023
# and 024 -- the code changed, the configuration did not, which is what keeps B-A attributable.
#
# ONE BINARY, TWO CONFIGS. Any code change before all ten arm-seeds finish invalidates the whole run.
# See docs/experiments/loca/iter025/PREREGISTRATION.md for the frozen inputs and the reading table.
set -uo pipefail
H="$HOME/cg-loca"; BIN="$HOME/cg-bin/cg-i025-proxy-v03"; PORT=6870
EXPECT=fc42520260d70e834bc2adac94372974
# THE BINARY IS PART OF THE PREREGISTRATION, and this is checked BEFORE EVERY PASS rather than once at
# launch. A rebuild landing on this path mid-run -- another session, a stray `go build -o`, a fix applied
# in good faith at hour three -- would leave some passes on code X and others on code Y while every
# counter stayed healthy, cost_source still read `component` and the arithmetic still produced a number.
# That is the failure with no symptom, and checking once at the start does not catch it: the rebuild
# happens after the check. Per-pass costs one sha256sum of 41MB against a pass that runs for ~40 minutes.
check_binary() {
  local got; got=$(sha256sum "$BIN" | cut -c1-32)
  [ "$got" = "$EXPECT" ] && return 0
  echo "BINARY MISMATCH before $1: $got != $EXPECT (preregistered)"
  echo "REFUSING. Passes already completed ran a DIFFERENT binary from this one -- do not compare them."
  echo "Rebuild v03 from commit 5d58575, or re-preregister with the new SHA and discard finished passes."
  exit 1
}
check_binary "launch"
# AMENDMENT 1: arm A at seed 1 ONLY. Seeds 2-5 pair arm B against iteration 024's recorded arm A, whose
# reuse is argued in the preregistration -- the gateway difference is billing rather than routing, and
# iteration 024's arm A recorded no extraction activity for the accounting fixes to have invalidated.
# Seed 1's arm A is the DRIFT CHECK against `i024As1`, and its pass/fail criterion is fixed in advance.
for S in 1 2 3 4 5; do
  ARMS="B-merged"
  [ "$S" = 1 ] && ARMS="A-baseline B-merged"
  for ARM in $ARMS; do
    TAG="i025${ARM%%-*}s$S"
    if [ -f "$H/st-i022-$TAG.json" ]; then echo "SKIP $TAG"; continue; fi
    echo "===== SEED $S ARM $ARM ($TAG) ====="; date -u
    check_binary "$TAG"
    bash "$H/stage022.sh" "$TAG" "$BIN" "$H/cfg-iter023-$ARM.yaml" "$PORT" "$H/task-configs/i024-64k-s$S.json" 64
    sleep 15
  done
done
echo "ALL_PASSES_DONE"
