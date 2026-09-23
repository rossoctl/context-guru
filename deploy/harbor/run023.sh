#!/bin/bash
# iter023 Stage 0: three arms x 15 tasks (seed 42) at 64k, ONE binary, three configs.
#
# Two settings differ from iteration 022, both corrections rather than tuning:
#   - extract_llm is UNPINNED, which fixes #120 (its pinned floor sat below the 6,250-token break-even
#     its own preset comment derives) and #134's side effect (pinning either the floor or the trigger
#     sets `explicit`, and shouldFire() then returns true unconditionally, so the warm path fired every
#     request). Unpinned, the derived pressure trigger governs and pressureFloor derives the floor.
#   - the sweep floor drops to 100, reaching a batch of ~11.6 against the cap of 12, which is what makes
#     iteration 021's coverage question (61% at batch 12) directly testable. Safe now that
#     selectAffordableDrops prices a drop on DEPTH relative to the batch rather than on size.
#
# Interleaved by arm across three passes of five tasks, same reasoning as iteration 022: sequential by arm
# would put arm C hours after arm A, loading any drift onto whichever ran last, and interleaving also
# yields a complete paired comparison on five tasks after pass 1.
set -uo pipefail
H="$HOME/cg-loca"
BIN="$HOME/cg-bin/cg-i023-proxy-v01"
PORT=6860
for P in 1 2 3; do
  for ARM in A-baseline B-merged C-coref; do
    TAG="i023${ARM%%-*}p$P"
    if [ -f "$H/st-$TAG.json" ]; then echo "SKIP $TAG (already have stats)"; continue; fi
    echo "===== PASS $P ARM $ARM ($TAG) ====="; date -u
    bash "$H/stage022.sh" "$TAG" "$BIN" "$H/cfg-iter023-$ARM.yaml" "$PORT" "$H/task-configs/i022-64k-p$P.json" 64
    sleep 15
  done
done
echo "ALL_PASSES_DONE"
