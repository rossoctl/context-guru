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
got=$(sha256sum "$BIN" | cut -c1-32)
# THE BINARY IS PART OF THE PREREGISTRATION. A rebuild between arm-seeds is the one thing that would
# make B-A meaningless while every counter still looked healthy, so it is checked rather than trusted.
[ "$got" = "$EXPECT" ] || { echo "BINARY MISMATCH: $got != $EXPECT (preregistered) -- refusing"; exit 1; }
for S in 1 2 3 4 5; do
  for ARM in A-baseline B-merged; do
    TAG="i025${ARM%%-*}s$S"
    if [ -f "$H/st-i022-$TAG.json" ]; then echo "SKIP $TAG"; continue; fi
    echo "===== SEED $S ARM $ARM ($TAG) ====="; date -u
    bash "$H/stage022.sh" "$TAG" "$BIN" "$H/cfg-iter023-$ARM.yaml" "$PORT" "$H/task-configs/i024-64k-s$S.json" 64
    sleep 15
  done
done
echo "ALL_PASSES_DONE"
