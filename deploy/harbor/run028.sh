#!/bin/bash
# iteration 028: does iteration 024's reward result appear when the band is actually 64k?
#
# ONE SEED PER INVOCATION, AND THEN IT STOPS. There is no loop over seeds in this file, deliberately and
# at the operator's instruction: each seed is read out and the decision to continue is made by a person.
# A script that ran all five would spend the whole budget before the first number was looked at, which is
# how iteration 026 spent a full run to discover a zero.
#
#   ./run028.sh preflight    the four conditions from PREREGISTRATION.md section 2. No reward pass, ~$8.
#   ./run028.sh thprobe 2048     does BOUNDING thinking stop the truncation, and at what cost to
#                            accuracy? Two tasks, 4096 output budget, ~$5. This is the live question.
#   ./run028.sh mtprobe 8192     does raising LOCA's output budget stop the harness killing episodes?
#                            Two tasks, baseline config, ~$10. Reward is NOT measured.
#   ./run028.sh base 1 [model]   the BASELINE pass of seed 1 only, on an optional agent model. No ask
#                            spend; with the default model this IS seed 1's baseline and `seed 1`
#                            will reuse it rather than pay for it twice.
#   ./run028.sh seed 1       baseline then arm A for seed 1, then STOP.
#   ./run028.sh readout      the cumulative mean difference and the futility check, over whatever exists.
#
# BASELINE FIRST WITHIN EACH SEED, so that if a seed is interrupted halfway what survives is the arm that
# costs nothing to repeat rather than the arm that carries the ask spend.
#
# ONE BINARY, BOTH CONFIGS, HASH CHECKED BEFORE EVERY PASS. Not once at launch: a rebuild landing on this
# path between seed 2 and seed 3 leaves some passes on code X and others on code Y with every counter
# still healthy, and the comparison is silently void. The expected hash must be filled in from the built
# binary BEFORE the first pass and not edited afterwards.
set -uo pipefail
H="$HOME/cg-loca"
BIN="${CG_I028_BIN:-$HOME/cg-bin/cg-i028-proxy}"
PORT="${CG_I028_PORT:-6872}"
# PINNED HERE, not left to the environment, so the preregistration is auditable in the repo rather than
# in someone's shell history. Built with
# `CGO_ENABLED=1 go build -o ~/cg-bin/cg-i028-proxy ./cmd/context-guru-proxy`.
#
# DO NOT EDIT once a REWARD pass has completed: those passes used this binary, and changing the pin to
# match a rebuild compares two different programs while every counter still looks healthy.
#
# CHANGED ONCE, before any reward pass, and the record matters. b9a69b15e8644bb479bead7e537cf7bf ran the
# 128k pre-flight's predecessor at 64k and is superseded by the horizon credit in prefixRewriteNet
# (prefix_econ.go). No seed had been run under the old hash, so nothing is being invalidated -- only the
# two pre-flight attempts, both of which are kept on disk with their reasons.
EXPECT="${CG_I028_SHA:-bc3b53b5320e898490c333e61c28bae9}"
# 128k, NOT the 64k this iteration started with. Iteration 028's own pre-flight is the reason: at a 64k
# declared band the agent's requests EXCEED the window -- 73,550 tokens measured, pressures of 1.15 and
# 3.62 -- and `turnsRemainingAfter` returns 0 whenever reqAfter >= window. That zeroes three terms at
# once: the econ trigger's `have`, the horizon credit, and selectAffordableDrops' S*T. Observed
# consequence: 339 of 349 candidates declined by the inventory floor, then 8 of the surviving 9 drops
# pruned, then 105 tokens removed across 64 requests.
#
# At 128k the same 73,550-token request sits at 0.57 pressure with a real horizon. This is also, finally,
# a mechanical account of iteration 024: its window resolved to 1,000,000, so that request sat at 7% and
# every term was healthy. Its result was not bought by a permissive gate but by a window large enough for
# the arithmetic to be non-degenerate.
#
# The band sets the declared window AND LOCA's clearing threshold together (stage022.sh), and
# model-window-128k.json is served on :6980 -- verified HTTP 200 before this was changed.
BAND=128
STEP="${1:-readout}"

die() { echo "REFUSING: $*" >&2; exit 1; }

check_binary() {
  [ -x "$BIN" ] || die "no binary at $BIN"
  [ -n "$EXPECT" ] || die "CG_I028_SHA is unset. Pin the binary hash before the first pass, or a rebuild
  mid-run will void the comparison without changing a single counter."
  local got; got=$(sha256sum "$BIN" | cut -c1-32)
  [ "$got" = "$EXPECT" ] || die "binary is $got, preregistered $EXPECT.
  Passes already completed ran a DIFFERENT binary. Do not compare them."
}

# CONDITION 1 of the pre-flight, applied to every pass and not only the probe. This is the check whose
# absence voided iterations 008-024: the window resolved to 1,000,000 and every threshold in the config
# was measured against the wrong number, silently.
verify_window() {
  local tag="$1" unres
  unres=$(grep -o '"model_info_unresolved":[0-9]*' "$H/st-i022-$tag.json" 2>/dev/null | grep -o '[0-9]*$')
  [ -n "$unres" ] || { echo "  !! $tag: no model_info_unresolved in the stats -- cannot verify the band"; return 1; }
  [ "$unres" = "0" ] || { echo "  !! $tag: model_info_unresolved=$unres -- thresholds used the wrong window. DISCARD."; return 1; }
  echo "  $tag: model_info_unresolved=0"
}

# CONDITION 4: the baseline must not act, and arm A must ask. Either failure means the arms are not what
# the configs claim, and a null result would be indistinguishable from two identical arms -- exactly the
# thing iteration 026 could not tell apart.
verify_arm() {
  local tag="$1" arm="$2"
  python3 - "$H/st-i022-$tag.json" "$arm" <<'PY'
import json,sys
d=json.load(open(sys.argv[1])); arm=sys.argv[2]
c=(d.get("components") or {}).get("extract_llm_sweep") or {}
e=c.get("events") or {}
acted=c.get("acted") or 0
asks=e.get("sweep_prefix_cache_read_ok",0)+e.get("sweep_adjudicated",0)
print("  %s: acted=%d offered=%d adjudicated=%d"%(arm,acted,e.get("sweep_offered",0),e.get("sweep_adjudicated",0)))
if arm=="baseline" and acted!=0:
    sys.exit("  !! BASELINE ACTED (%d). The arms are not what the configs claim; nothing is comparable."%acted)
if arm=="A" and e.get("sweep_adjudicated",0)<5:
    sys.exit("  !! ARM A produced %d verdicts (<5). This pass is INVALID, not informative. Re-run it;\n"
             "  !! do not pool it and do not read it as futility."%e.get("sweep_adjudicated",0))
PY
}

# RESUMABILITY CUTS BOTH WAYS. Skipping a tag whose stats exist is what lets an interrupted run continue
# without repeating a paid pass -- and it is also how a pass that must be REDONE gets silently skipped.
# Measured: the first pre-flight attempt died on a rotated gateway credential (HTTP 401 on every request),
# left a stats file behind, and the retry would have reported "SKIP" and moved on with a pass that
# adjudicated nothing. Move the artifact aside rather than deleting it -- `mv st-i022-<tag>.json{,.bad}` --
# so the reason it was rejected stays on disk next to the run it belongs to.
run_pass() {
  local tag="$1" cfg="$2" arm="$3" taskcfg="$4"
  if [ -f "$H/st-i022-$tag.json" ]; then
    echo "SKIP $tag (stats already on disk)."
    echo "  If that pass was INVALID -- wrong window, baseline acted, <5 verdicts, accuracy 0.000, or an"
    echo "  auth failure -- move it aside first or this run silently reuses it:"
    echo "    mv $H/st-i022-$tag.json $H/st-i022-$tag.json.bad"
    return 0
  fi
  echo "===== $tag  arm=$arm ====="; date -u
  check_binary
  bash "$H/stage022.sh" "$tag" "$BIN" "$H/$cfg" "$PORT" "$taskcfg" "$BAND" || die "$tag failed to run"
  verify_window "$tag" || die "$tag: window check failed"
  verify_arm "$tag" "$arm" || die "$tag: arm check failed"
  sleep 15
}

case "$STEP" in
preflight)
  # Arm A only, two tasks. Answers ONLY: is the band real, and does it fire. Neither needs a full pass,
  # and both invalidate everything after them.
  echo "PRE-FLIGHT: band real? fires at all? baseline inert? Reward is NOT measured here."
  run_pass "i028pf" "cfg-iter028-A.yaml" "A" "$H/task-configs/i028-preflight.json"
  echo
  echo "Pre-flight passed. Nothing about reward is known. Next: ./run028.sh seed 1"
  ;;
thprobe)
  # THE BOUNDED-THINKING PROBE, and the one the 8192 probe argued for. Same two environments, same 4096
  # output budget as every earlier pass, with thinking BOUNDED instead of left adaptive.
  #
  # It answers the only thing the gateway measurements could not: whether sonnet's edge depends on the
  # unbounded thinking. Sonnet solves 5 of 12 live environments where haiku solves 1, and haiku never
  # thinks at all -- so bounding thinking might cost the difference, or might cost nothing. The three
  # repetitions on the gateway say the OUTPUT stops running away (capped 3/3 -> 0/3); they say nothing
  # about task success, which needs the rig.
  #
  # READ: truncations should fall from 16 (at 8192) and from 5-9 (at 4096 unbounded) toward zero. If they
  # do AND an environment comes off zero, this is the rig setting for the whole iteration. If truncations
  # fall but both stay at zero, the setting is still right -- it removes a large noise source and 7x of
  # cost -- but it is not the reason these two never score, and that has to be said rather than assumed.
  #
  # This probe cannot spiral the way the 8192 one did ($19 against a $10 estimate): bounded thinking
  # produces fewer truncations, so fewer broken tool calls and fewer retries.
  TB="${2:-2048}"
  TAG="i028-tb$TB"
  export LOCA_THINKING_BUDGET="$TB"
  run_pass "$TAG" "cfg-iter028-baseline.yaml" "baseline" "$H/task-configs/i028-mtprobe.json"
  echo
  echo "===== $TAG: did bounding thinking stop the truncation? ====="
  echo -n "  LOCA config banner -- max tokens: "
  grep -oE "Max tokens +[0-9]+" "$H/i022loca-$TAG.log" | head -1
  echo -n "  LOCA config banner -- thinking:  "
  grep -A2 -E "Extended Thinking|Enabled" "$H/i022loca-$TAG.log" | grep -oE "(Enabled|Budget tokens) +\S+" | head -2 | tr '\n' ' '
  echo
  echo -n "  stop_reason max_tokens responses: "
  grep -c "stop_reason: max_tokens" "$H/i022loca-$TAG.log" || true
  echo -n "  tool validation failures:         "
  grep -cE "validation error for call|Missing required argument" "$H/i022loca-$TAG.log" || true
  echo -n "  responses carrying thinking:      "
  grep -c "Response contains .* thinking block" "$H/i022loca-$TAG.log" || true
  python3 "$H/perenv028.py" "$TAG"
  echo
  echo "COMPARE AGAINST: unbounded at 4096 -- CanvasArrangeExam 0.000 (1 trunc), NhlB2b 0.000 (5 trunc),"
  echo "                 \$2.02 and \$2.49; unbounded at 8192 -- both 0.000, 16 truncations, \$19.19 total."
  ;;
mtprobe)
  # THE OUTPUT-BUDGET PROBE. Answers three things and nothing about reward:
  #   1. does the raised `--max-tokens` actually reach the request, or is it clamped somewhere,
  #   2. do the harness's truncation KILLS stop, and
  #   3. does an environment that has never scored on sonnet score once it is allowed to finish.
  #
  # Two environments, both of which were truncation-killed in BOTH seed-1 sonnet arms.
  # CanvasArrangeExam is the informative one: it is on the never-scores list, and haiku -- which killed no
  # episodes at all -- solved it outright at 35/35 courses matched, so it is demonstrably winnable and its
  # zero on sonnet is the budget rather than the task. NhlB2bAnalysis is a live environment that carries
  # real power, so the probe is not read only off a task we had already written off.
  #
  # BASELINE CONFIG, so the sweep is inert: this measures the RIG, and an ask would only add a variable
  # and a cost to a question that has nothing to do with selection.
  #
  # A PROBE THAT SEES NO RESPONSE OVER 4096 TOKENS IS INCONCLUSIVE, NOT A PASS. Either the flag never
  # reached the request or nothing wanted the extra budget, and those need opposite responses -- so the
  # check below reads the request bodies rather than trusting that the flag was honoured.
  MT="${2:-8192}"
  TAG="i028-mt$MT"
  export LOCA_MAX_TOKENS="$MT"
  run_pass "$TAG" "cfg-iter028-baseline.yaml" "baseline" "$H/task-configs/i028-mtprobe.json"
  echo
  echo "===== $TAG: did the budget reach the request, and did the kills stop? ====="
  # LOCA'S OWN CONFIG BANNER is the authoritative record that the flag took effect -- it prints
  # "Max tokens <n>" before the first task. NOT the capture hop's body dump: i022body-*.json holds a
  # RESPONSE (an SSE fragment), so grepping it for max_tokens finds output-token counts at best and
  # nothing at worst, and a blank line there would read as "the flag was ignored" when it was honoured.
  echo -n "  LOCA config banner says: "
  grep -oE "Max tokens +[0-9]+" "$H/i022loca-$TAG.log" | head -1 || echo "(not found -- cannot confirm the budget)"
  echo -n "  responses over 4096 output tokens: "
  grep -oE "output: [0-9]+" "$H/i022loca-$TAG.log" | grep -oE "[0-9]+" |
    awk -v n=0 '{if ($1>4096) n++} END {print n}'
  echo -n "  responses at exactly $MT (the new wall): "
  grep -c "output: $MT" "$H/i022loca-$TAG.log" || true
  echo -n "  stop_reason max_tokens responses: "
  grep -c "stop_reason: max_tokens" "$H/i022loca-$TAG.log" || true
  python3 "$H/perenv028.py" "$TAG"
  echo
  echo "READ IT AS: kills gone AND an environment off zero => raise the budget for the whole iteration"
  echo "            and re-run seed 1 whole, both arms. Kills merely fewer => the wall moved, not away;"
  echo "            decide on a higher value. No response over 4096 => INCONCLUSIVE, the flag did not"
  echo "            take effect or nothing needed it. Do not pool this pass with a reward seed either way."
  ;;
base)
  # BASELINE ONLY, for a question that is not about reward: can the AGENT solve these tasks at all on a
  # given model. The baseline config leaves the sweep inert, so this pass carries no ask spend and is not
  # an arm of anything -- it is a capability measurement.
  #
  # WHY IT EXISTS AS ITS OWN STEP. The question "would a cheaper agent model still finish these tasks"
  # cannot be answered from a reward comparison: if the agent solves nothing, both arms score zero and
  # the run reads as futility rather than as an unusable agent. Iteration 026 spent a full run to learn
  # a zero it could not attribute.
  #
  # THE TAG ENCODES A NON-DEFAULT MODEL. A haiku pass written to the plain `i028-sN-base` tag would be
  # SKIPPED and silently adopted by `./run028.sh seed N` as arm A's baseline, comparing a haiku baseline
  # against a sonnet arm A with every counter healthy. The default model keeps the plain tag precisely
  # because that pass IS seed N's baseline and must be reused rather than paid for twice.
  S="${2:?usage: run028.sh base N [model]}"
  M="${3:-aws/claude-sonnet-5}"
  TC="$H/task-configs/i024-64k-s$S.json"
  [ -f "$TC" ] || die "no task config $TC -- seed $S must reuse iteration 024's own instances"
  TAG="i028-s$S-base"
  case "$M" in
    aws/claude-sonnet-5) ;;
    *) TAG="$TAG-$(printf %s "$M" | tr -c 'A-Za-z0-9' '-')" ;;
  esac
  # THE THINKING BUDGET IS PART OF THE TAG, for the same reason the model is. A bounded-thinking pass
  # written to the plain tag would be SKIPPED by run_pass and adopted by `seed N` as arm A's baseline,
  # comparing a bounded baseline against an unbounded arm A -- and the unbounded pilot it must be PAIRED
  # against would have been overwritten by the very pass that needs it as a reference.
  [ -n "${LOCA_THINKING_BUDGET:-}" ] && TAG="$TAG-tb$LOCA_THINKING_BUDGET"
  export LOCA_MODEL="$M"
  run_pass "$TAG" "cfg-iter028-baseline.yaml" "baseline" "$TC"
  echo
  echo "===== $TAG: per-environment accuracy (model $M) ====="
  # THE READ IS THE COUNT OF NON-DEGENERATE ENVIRONMENTS, not the mean. Three of these fifteen never
  # solve on any arm or model; power lives entirely in the other twelve, and a weaker agent buys nothing
  # by being cheaper if it pushes more of them to a constant zero.
  python3 "$H/perenv028.py" "$TAG"
  ;;
seed)
  S="${2:?usage: run028.sh seed N}"
  TC="$H/task-configs/i024-64k-s$S.json"
  [ -f "$TC" ] || die "no task config $TC -- seed $S must reuse iteration 024's own instances"
  run_pass "i028-s$S-base" "cfg-iter028-baseline.yaml" "baseline" "$TC"
  run_pass "i028-s$S-A"    "cfg-iter028-A.yaml"        "A"        "$TC"
  echo
  bash "$0" readout
  echo
  echo "=============================================================================="
  echo "SEED $S COMPLETE. STOPPING, BY DESIGN."
  echo "Do not run the next seed until a person has read the numbers above and said so."
  echo "=============================================================================="
  ;;
readout)
  python3 "$H/readout028.py"
  ;;
*) die "unknown step '$STEP' (preflight | mtprobe [budget] | thprobe [budget] | base N [model] | seed N | readout)" ;;
esac
