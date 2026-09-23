#!/bin/bash
# iteration 027: does the reward result survive a gate that is correctly denominated AND priced for the
# benefit the run actually delivered? TWO arms x 15 tasks x 5 seeds at 64k = 150 runs, INTERLEAVED A then
# B within each seed so an early read is a comparison rather than half a comparison.
#
# STAGED ON PURPOSE, because the budget does not fund the full design and pretending otherwise is how a
# run gets abandoned halfway with nothing readable. See PREREGISTRATION.md section "Staging and budget":
#
#   STEP 0  the mechanism probe, arm B, 2 tasks, ~$8. Answers ONLY "does it fire, and was the window
#           real". Iteration 026's answer to the first was zero, and iterations 008-024's answer to the
#           second was no. Neither needs a full pass to establish, and both invalidate everything after.
#   STEP 1  seed 1, arm A then arm B, ~$100. Directional on the endpoints. NOT significant.
#   STEP 2  seeds 2-5. Requires new budget and an explicit go-ahead.
#
# ONE BINARY, TWO CONFIGS. Any code change before the passes being compared finish invalidates them, which
# is why the hash is checked BEFORE EVERY PASS and not once at launch — a rebuild landing on this path at
# hour three leaves some passes on code X and others on code Y with every counter still healthy.
set -uo pipefail
H="$HOME/cg-loca"; BIN="$HOME/cg-bin/cg-i027-proxy-v04"; PORT=6870
EXPECT=6a09c9e993a31e9387c2a330945fd234
STEP="${1:-probe}"

check_binary() {
  local got; got=$(sha256sum "$BIN" | cut -c1-32)
  [ "$got" = "$EXPECT" ] && return 0
  echo "BINARY MISMATCH before $1: $got != $EXPECT (preregistered)"
  echo "REFUSING. Passes already completed ran a DIFFERENT binary -- do not compare them."
  exit 1
}

# EVERY PASS IS CHECKED FOR THE WINDOW, twice: stage022.sh refuses to start if the model-window document
# is unreachable, and prints a WINDOW_MISMATCH banner afterwards if the RESOLVED window was not the band.
# This function is the third look, at the artifact, because that is what a reader of the results has.
verify_window() {
  local tag="$1" unres
  unres=$(grep -o '"model_info_unresolved":[0-9]*' "$H/st-i022-$tag.json" 2>/dev/null | grep -o '[0-9]*$')
  if [ -n "$unres" ] && [ "$unres" != "0" ]; then
    echo "!!!!! $tag: model_info_unresolved=$unres -- this pass's thresholds used the wrong window."
    echo "!!!!! DISCARD IT. This is the failure that voided iteration 024."
    return 1
  fi
  echo "  $tag: model_info_unresolved=0"
}

run_pass() {
  local tag="$1" arm="$2" taskcfg="$3"
  if [ -f "$H/st-i022-$tag.json" ]; then echo "SKIP $tag (already done)"; return 0; fi
  echo "===== $tag arm $arm ====="; date -u
  check_binary "$tag"
  bash "$H/stage022.sh" "$tag" "$BIN" "$H/cfg-iter027-$arm.yaml" "$PORT" "$taskcfg" 64
  verify_window "$tag"
  sleep 15
}

case "$STEP" in
probe)
  # ARM B ONLY, and mechanism only. A probe cannot say anything about reward and this one is not asked to.
  echo "STEP 0: mechanism probe. Reads, in this order: model_info_unresolved, the resolved ctx_window,"
  echo "        whether any ask happened at all, and which term is refusing on cg.sweep.econ."
  run_pass "i027pf1" "B-merged" "$H/task-configs/i027-probe.json"
  echo "--- what the probe has to answer before step 1 is worth paying for ---"
  grep -c '"msg":"cg.sweep.ask"' "$H/i022log-i027pf1.jsonl" 2>/dev/null | sed 's/^/  asks made: /'
  grep -o '"ctx_window":[0-9]*' "$H/i022log-i027pf1.jsonl" 2>/dev/null | sort | uniq -c
  python3 - <<'PY'
import json, glob, collections
rows = []
for f in glob.glob("/home/vpcuser/cg-loca/i022log-i027pf1.jsonl"):
    for l in open(f, errors="replace"):
        if '"cg.sweep.econ"' not in l:
            continue
        try: rows.append(json.loads(l))
        except Exception: pass
if not rows:
    print("  NO ECON DECISIONS -- the trigger was never even evaluated. Read the gates before spending.")
else:
    fired = sum(1 for d in rows if d.get("decision"))
    print("  econ decisions=%d fired=%d premium=%s" % (len(rows), fired, rows[0].get("premium")))
    h = sorted(d.get("haveTurns", 0) for d in rows)
    n = sorted(d.get("needTurns", 0) for d in rows)
    print("  haveTurns min=%d p50=%d max=%d   needTurns min=%d p50=%d max=%d"
          % (h[0], h[len(h)//2], h[-1], n[0], n[len(n)//2], n[-1]))
    print("  askDeclined=%d  (the rest of the declines are the rewrite)"
          % sum(1 for d in rows if d.get("askDeclined")))
PY
  ;;
seed1)
  echo "STEP 1: seed 1, both arms, interleaved. Directional only -- 15 clusters at one seed."
  run_pass "i027As1" "A-baseline" "$H/task-configs/i024-64k-s1.json"
  run_pass "i027Bs1" "B-merged"   "$H/task-configs/i024-64k-s1.json"
  echo "===== CHECKPOINT: seed 1 pair complete. STOP HERE unless the reading table says continue. ====="
  ;;
rest)
  echo "STEP 2: seeds 2-5. Only with an explicit go-ahead and new budget."
  for S in 2 3 4 5; do
    run_pass "i027As$S" "A-baseline" "$H/task-configs/i024-64k-s$S.json"
    run_pass "i027Bs$S" "B-merged"   "$H/task-configs/i024-64k-s$S.json"
    echo "===== CHECKPOINT: seed $S pair complete ====="
  done
  ;;
*) echo "usage: run027.sh {probe|seed1|rest}"; exit 2 ;;
esac
echo "STEP_DONE $STEP"
