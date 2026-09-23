#!/bin/bash
# iter022 Stage 0: three arms x 15 tasks (seed 42) at 64k -- see PREREGISTRATION.md Amendment 1, ONE binary, three configs.
#
# INTERLEAVED BY ARM across three passes of five tasks. Sequential-by-arm would put arm C two to three
# hours after arm A, so any drift in gateway latency or model behaviour over the night would load
# entirely onto whichever arm ran last. Round-robin spreads it across all three. It also produces a
# complete paired comparison on five tasks after pass 1, which is the earliest honest direction check.
#
# Never two arms at once: one port set, one proxy, sequential invocation. The stale-proxy bug
# invalidated two arms in an earlier iteration.
set -uo pipefail
H="$HOME/cg-loca"
BIN="$HOME/cg-bin/cg-i022-proxy-v01"
PORT=6860
for P in 1 2 3; do
  for ARM in A-housellm B-merged C-coref; do
    TAG="${ARM%%-*}p$P"
    if [ -f "$H/st-i022-$TAG.json" ]; then echo "SKIP $TAG (already have stats)"; continue; fi
    echo "===== PASS $P ARM $ARM  ($TAG) ====="; date -u
    bash "$H/stage022.sh" "$TAG" "$BIN" "$H/cfg-iter022-$ARM.yaml" "$PORT" "$H/task-configs/i022-64k-p$P.json" 64
    grep -E "Avg Cost" "$H/i022loca-$TAG.log" 2>/dev/null | tail -2
    sleep 15
  done
done
echo "ALL_PASSES_DONE"
