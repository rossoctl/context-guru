#!/bin/bash
# SCENARIO D — DOES `W - max_tokens` PREDICT WHERE THE CLIENT COMPACTS?
#
# The client's compaction point C is what the trigger's fill fraction is measured against, and today it
# comes from a per-model table with one measured entry. This arm tests the RULE that would let C be
# derived per request instead, with no table and no learning.
#
# THE OBSERVATION THAT SUGGESTS THE RULE. On a real Claude Code session on haiku, `max_tokens` was
# 32,000 on every single request, and:
#
#	W - max_tokens = 200,000 - 32,000 = 168,000
#
# which is the threshold the client's own context indicator reports. So the client appears to reserve a
# full response's worth of headroom in its own accounting. If that is the rule, C is derivable from two
# numbers already on every request -- and `max_tokens` is the quantity to watch when a deployment
# changes its output budget.
#
# WHY THIS ARM AND NOT A CONFIG KNOB. An earlier version of this arm set a settings key named
# `autoCompactThreshold` and looked for the compaction point to move. That key was INVENTED -- it is
# not in the client's settings and nothing was shown to read it -- so the arm tested nothing. If the
# client's threshold really is a reserve rule rather than a configured fraction, there may be no
# threshold knob at all, and the settable quantity is the output budget, which reaches us as
# `max_tokens`.
#
# THE TEST. Run the same growth on a SECOND model with the same window, and see whether the observed
# compaction point moves the way `W - max_tokens` predicts. One (W, max_tokens) pair cannot distinguish
# a rule from a coincidence; two can.
#
# INFORMATIVE EITHER WAY:
#   - max_tokens differs between the models AND the compaction point moves as predicted -> the rule
#     holds on two points, and C can be derived rather than tabulated.
#   - max_tokens differs and the point does NOT move as predicted -> the rule is refuted, and the
#     haiku match was a coincidence. Worth knowing before anyone builds on it.
#   - max_tokens is the SAME on both models -> no variation was available, the arm proves nothing, and
#     it says so rather than reporting a pass.
#
# THE THIRD OUTCOME IS WHAT HAPPENED, AND IT IS STRUCTURAL FOR THIS MODEL PAIR. Claude Code sent
# max_tokens = 32,000 on `claude-sonnet-4-5` — identical to haiku. So no pair of 200,000-window models
# can test the rule here: the predictor does not vary. Varying it needs a model where the client picks
# a different output cap, or control of max_tokens itself, and neither is available cheaply.
#
# Which is fine, because the rule is a SPECULATION that has already been retracted (see
# internal/compactionpoint: one data point, and evidence against the mechanism that would make it
# non-arbitrary). The primary source for C is direct observation in billed tokens and does not depend
# on it. Keep this arm for the day a model with a different output cap is on hand; do not spend money
# on it before then.
set -u
. "$(dirname "$0")/lib.sh"

N=maxtokrule
PORT=4214
# A second 200,000-window model, so the cost is comparable to arm A rather than a 1M session.
#
# PREFIXED, because a gateway routes by its own names: the unprefixed `claude-sonnet-4-5` 403s with
# "team not allowed to access model" on the gateway this was written against, while the same model
# under `aws/` is routable. Every request then returns 403, the session never grows, and the arm
# reports "the client did not compact" — which looks like a result and is a routing error.
MODEL2="${CG_SCEN_MODEL2:-aws/claude-sonnet-4-5}"
# Override the library's model for this whole arm, rather than prefixing every call.
MODEL="$MODEL2"

echo "=== SCENARIO D: does W - max_tokens predict the compaction point? (model=$MODEL2) ==="
scen_build
scen_start "$N" "$PORT" 0.9
scen_home "$N" "$PORT"
scen_work  "$N"

# The growth sequence, driven on the SECOND model. Same prompts as arm A so the transcript shape is
# comparable and any difference in the compaction point is the model's, not the workload's.
scen_turn "$N" g01 fresh "Read a.go and list every exported function with a one-line summary."
for i in 02 03 04 05 06 07 08 09 10 11 12; do
  case $i in
    02) P="Read b.go in full. Summarise how the economic gate skips a candidate.";;
    03) P="Read c.go in full. Explain how the conversation partition is chosen.";;
    04) P="Read d.go in full. Explain the episode span axis.";;
    05) P="Read e.go in full. List every place usage is attributed to a request.";;
    06) P="Read f.go in full. Explain how the boundary is computed.";;
    07) P="Read g.go in full. Explain baselineDeltaUSD and repeatRate.";;
    08) P="Read h.go in full. Explain every conjunct of Fires and CacheAllows.";;
    09) P="Compare a.go and d.go: which facts does each own?";;
    10) P="Re-read c.go and g.go. List every hardcoded cache TTL.";;
    11) P="Re-read b.go and e.go. List every counter and where it is exported.";;
    12) P="Summarise everything you have learned in fifteen bullets.";;
  esac
  scen_turn "$N" "t$i" continue "$P"
done

echo
echo "=== SCENARIO D: all rows ==="
scen_tail "$N" 60

echo
echo "=== SCENARIO D: the rule, tested ==="
SCEN_DB="$SCEN_ROOT/$N/dash.db" CG_SCEN_WINDOW="${CG_SCEN_WINDOW:-200000}" python3 - <<'PY'
import os, sqlite3
W = int(os.environ["CG_SCEN_WINDOW"])
c = sqlite3.connect("file:%s?mode=ro" % os.environ["SCEN_DB"], uri=True)
rows = list(c.execute("""
  SELECT id, fresh_input+cache_read+cache_write AS billed, tokens_before, bypassed, max_tokens,
         model, agent
  FROM requests WHERE keepalive=0 ORDER BY ts"""))
if not rows:
    print("NO ROWS"); raise SystemExit

mt = sorted({r[4] for r in rows})
print("model(s)        %s" % sorted({r[5] for r in rows}))
print("agent           %s" % sorted({r[6] for r in rows}))
print("max_tokens      %s" % mt)
print("window          %d" % W)
peak = max(r[1] for r in rows)
print("peak billed     %d = %.3f of the window" % (peak, peak / W))

if len(mt) == 1:
    pred = W - mt[0]
    print()
    print("PREDICTION  W - max_tokens = %d - %d = %d  (the client's own ruler)" % (W, mt[0], pred))

# Where the client compacted, by both markers.
flagged = [(r[0], r[1]) for r in rows if r[3]]
drops = [(rows[i-1][0], rows[i-1][1]) for i in range(1, len(rows))
         if rows[i][2] and rows[i-1][2] and rows[i][2] < rows[i-1][2] * 0.7]
print()
print("MARKER 1 (agent-compaction detector): %s" % (flagged or "none"))
print("MARKER 2 (tokens_before drop):        %s" % (drops or "none"))

obs = sorted({b for _, b in flagged} | {b for _, b in drops})
if not obs:
    print()
    print("THE CLIENT DID NOT COMPACT. Peak fill above says whether the session simply did not grow")
    print("far enough. Nothing about the rule is shown either way.")
    raise SystemExit

print()
print("OBSERVED C (billed): %s  => %s of the window"
      % (obs, ["%.3f" % (x / W) for x in obs]))
if len(mt) == 1:
    pred = W - mt[0]
    print()
    print("THE COMPARISON. The prediction is in the CLIENT's ruler and the observation is in BILLED")
    print("tokens, so they are not the same quantity -- the ratio between them is the thing to look at:")
    for o in obs:
        print("  observed %d / predicted %d = %.3f" % (o, pred, o / pred))
    print()
    print("On haiku that ratio was 199,184 / 168,000 = 1.186. A ratio near 1.186 here means the rule")
    print("holds across two models and C can be DERIVED from max_tokens plus one calibration, with no")
    print("per-model table. A ratio far from it means the haiku match was a coincidence and the rule")
    print("is refuted -- which is the more useful outcome, because someone would otherwise build on it.")
    print()
    print("AND IF max_tokens IS THE SAME ON BOTH MODELS the arm has no variation to test and shows")
    print("nothing; compare the max_tokens line above against arm A's 32,000.")
PY
echo "=== SCENARIO D done $(date -u +%H:%M:%S) ==="
