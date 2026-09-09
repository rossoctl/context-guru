#!/bin/bash
# iteration 026: TWO arms x 15 tasks x 5 seeds at 64k = 150 runs, INTERLEAVED A then B within each seed.
#
# Interleaving does two jobs. Drift over the run lands on both arms equally, and every completed seed pair
# is a full 15-task PAIRED comparison -- directional, never significant -- so the run can be read at five
# checkpoints instead of only at the end. Running one arm to completion first would give nothing
# comparative until the second arm started.
#
# Stopping at a checkpoint is permitted for FUTILITY or MECHANISM FAILURE only, never to claim a positive
# result early. See docs/experiments/loca/iter026/PREREGISTRATION.md for the checkpoint table.
set -uo pipefail
H="$HOME/cg-loca"; BIN="$HOME/cg-bin/cg-i026-proxy-v10"; PORT=6870
EXPECT=84d20e23b651e22f6df8b87086ddbd37
# THE BINARY IS PART OF THE PREREGISTRATION, checked before EVERY pass rather than once at launch. A
# rebuild landing on this path mid-run would make B-A a comparison of two different programs while every
# counter stayed healthy, cost_source still read `component` and the arithmetic still produced a
# publishable number. Checking once cannot catch that: the rebuild happens after the check.
check_binary() {
  local got; got=$(sha256sum "$BIN" | cut -c1-32)
  [ "$got" = "$EXPECT" ] && return 0
  echo "BINARY MISMATCH before $1: $got != $EXPECT (preregistered)"
  echo "REFUSING. Passes already completed ran a DIFFERENT binary -- do not compare them."
  exit 1
}
check_binary "launch"
for S in 1 2 3 4 5; do
  for ARM in A-baseline B-merged; do
    TAG="i026${ARM%%-*}s$S"
    if [ -f "$H/st-i022-$TAG.json" ]; then echo "SKIP $TAG"; continue; fi
    echo "===== SEED $S ARM $ARM ($TAG) ====="; date -u
    check_binary "$TAG"
    bash "$H/stage022.sh" "$TAG" "$BIN" "$H/cfg-iter026-$ARM.yaml" "$PORT" \
      "$H/task-configs/i024-64k-s$S.json" 64
    sleep 15
  done
  echo "===== CHECKPOINT: seed $S pair complete ====="
done
echo "ALL_PASSES_DONE"
