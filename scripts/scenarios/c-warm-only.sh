#!/bin/bash
# SCENARIO C — WARM ONLY AFTER THE SUMMARY. The rate the review found wrong.
#
# The defect that inverted the panel's sign: the credit on a turn whose cache HIT was priced at the
# cache-WRITE rate, 12.5x too high, because the async stash key first reaches rep.CacheKeys on a
# credited replay turn and MarkUnique calls the whole removal "new content".
#
# So this arm fires once and then takes ONLY warm turns. Every credit it produces must land in
# read_credit_usd at the cache-READ rate, cold_credit_usd must be exactly zero, and the panel's
# figure must equal sum(saved_gross) x read rate — not the sum of the stored saved_usd, which is
# what it used to be.
set -u
. "$(dirname "$0")/lib.sh"

N=warmonly
PORT=4213

# ⚠️ THIS ARM NO LONGER FORCES A COLD TURN, AND THAT IS THE POINT OF THE CHANGE.
#
# It used to sleep GAP=380s to open a `pre_expiry_or_cold` gate, because a warm turn could not fire.
# Two things followed from that, and both are now gone:
#
#   - the gap was 380 rather than 310 because the strict cold test required a 60s clock-skew margin
#     PAST nominal expiry. 310s looked right — the provider HAD dropped the entry, the turn billed a
#     full 183,660-token rewrite with zero cache read — and the gate still refused, because the
#     arithmetic read "-10s remaining", inside the allowance. That conservatism cost real firing
#     opportunities: a session returning between 300s and 360s of idle found the entry gone AND the
#     gate shut.
#   - the forced fire was the arm's t0, so every later turn was warm by construction.
#
# The cold-gated cache states were withdrawn and the default is `any`, so the fire now happens on a
# WARM turn as soon as the transcript passes 0.5 of C. Keeping the sleep would invert the arm's own
# invariant: t0 would move earlier, the 380s gap would land AFTER t0, and the ttl_expiry turn it
# produces would put money in cold_credit_usd — which this arm asserts is exactly zero. So the sleep
# is removed rather than retuned.
#
# ⚠️ AND REMOVING IT MOVED t0 EARLIER, WHICH IS THE COST OF THE CHANGE. With the gate holding, t0 was
# pinned to the turn after the sleep and the eight tiny w-turns followed it by construction. Now t0
# lands on whichever g-turn first crosses 0.5 of C — and the remaining g-turns each read a file IN
# FULL, so they can consume a narrow span before the warm block starts. Two things answer that: the
# panel is now told the arm's real fill (see scen_panel), which makes the span 1.00 - 0.5 rather than
# the 0.10 it defaulted to; and the arrangement is ASSERTED below rather than assumed.
#
# resummarize_tokens is 200000 in lib.sh, so the checkpoint is replayed rather than re-summarized on
# every later turn. That is what keeps t0 unique without a gate holding it back.

echo "=== SCENARIO C: warm turns only after the summary (the read rate) ==="
scen_build
scen_start  "$N" "$PORT" 0.5
scen_home   "$N" "$PORT"
scen_work   "$N"

scen_turn "$N" g01 fresh    "Read a.go and list every exported function with a one-line summary."
scen_turn "$N" g02 continue "Read b.go in full. Summarise how the economic gate skips a candidate."
scen_turn "$N" g03 continue "Read c.go in full. Explain how the conversation partition is chosen."
scen_turn "$N" g04 continue "Read d.go in full. Explain the episode span axis."
scen_turn "$N" g05 continue "Read e.go in full. List every place usage is attributed to a request."
scen_turn "$N" g06 continue "Read f.go in full. Explain how the boundary is computed."
scen_turn "$N" g07 continue "Read g.go in full. Explain baselineDeltaUSD and repeatRate."

# No sleep: the fire happens on a warm turn at 0.5 of C. This turn is simply the next warm one, kept
# because the read-rate assertion needs turns after t0 to sum over — but note that t0 is now BEFORE
# this line rather than on it, so how many of the turns below fall inside the span is a property of
# the run, not of the script. The guard below asserts it instead of trusting it.
scen_turn "$N" f01 continue "In one sentence, what is the most important invariant in a.go?"

# Warm throughout: every turn within seconds of the last.
for k in 01 02 03 04 05 06 07 08; do
  scen_turn "$N" "w$k" continue "In one sentence, name one thing h.go decides."
done

echo
echo "=== SCENARIO C: all rows ==="
scen_tail "$N" 60
scen_panel "$N" "$PORT" "" 0.5
# A tiny span too, so the same rows also produce a CLOSED episode: the settled total is the figure a
# reader trusts, and an arm that only ever reports an open one cannot check it.
scen_panel "$N" "$PORT" 0.002 0.5

echo
echo "=== SCENARIO C: the read rate, hand-derived ==="
SCEN_DB="$SCEN_ROOT/$N/dash.db" SCEN_PANEL="$SCEN_ROOT/$N/panel.json" python3 - <<'PY'
import os, sqlite3, json
c = sqlite3.connect("file:%s?mode=ro" % os.environ["SCEN_DB"], uri=True)
rows = list(c.execute("""
  SELECT r.id, r.cache_miss_reason, COALESCE(c.saved_gross,0), COALESCE(c.saved_usd,0),
         COALESCE(c.events,''), r.cg_llm_cost_usd
  FROM requests r LEFT JOIN request_components c
    ON c.request_id=r.id AND c.component='summarize'
  WHERE r.keepalive=0 ORDER BY r.ts"""))
t0 = next((i for i, r in enumerate(rows)
           if '"summary_started"' in r[4] or '"fresh_summary"' in r[4]), None)
if t0 is None:
    print("NO SUMMARY WAS COMMISSIONED — precondition not met."); raise SystemExit
after = rows[t0+1:]
hits  = [r for r in after if r[1] == 'hit']
colds = [r for r in after if r[1] == 'ttl_expiry']
print("turns after t0   %d  (hit=%d, ttl_expiry=%d)" % (len(after), len(hits), len(colds)))
print("sum(saved_gross) on hit turns   %d" % sum(r[2] for r in hits))
print("sum(saved_usd)   on hit turns   %.8f   <-- what the panel USED to report" % sum(r[3] for r in hits))
print()
# READ THE EPISODE, NOT THE GROUP: group credit fields accumulate over CLOSED episodes only, so on an
# open run every bucket reads 0.00000000 and looks broken when the figures are in the episode.
pan = json.load(open(os.environ["SCEN_PANEL"]))
eps = pan.get("episodes", [])
e = eps[0] if eps else {}
print("EPISODE state=%s turns=%s new_content=%s" % (
    e.get("state"), e.get("turns"), e.get("new_content_billed")))
print("  read_credit_usd  %.8f" % e.get("read_credit_usd", 0))
print("  cold_credit_usd  %.8f" % e.get("cold_credit_usd", 0))
print("  summarizer_cost  %.8f" % e.get("summarizer_cost_usd", 0))
print("  invalidation     %.8f" % e.get("invalidation_debit_usd", 0))
print("  net_usd          %.8f" % e.get("net_usd", 0))

# Scoped to the turns the panel counted, and stated as pass/fail rather than as prose a reader has to
# adjudicate — an arm whose verdict needs interpreting is an arm that will be read as passing.
turns = int((e.get("turns") or 0))
span = rows[t0:t0 + turns] if turns else []
sh = [r for r in span[1:] if r[1] == 'hit']

# THE ARRANGEMENT IS ASSERTED, NOT REASONED ABOUT. This arm's claim is about a block of WARM turns
# after t0, and t0 is wherever the fill gate first opened — which is not a fact this script controls.
#
# It used to be: the cache gate held every warm turn, so t0 was pinned to the turn after the 380s
# sleep, and the eight tiny w01..w08 turns followed it by construction. With `cache_state: any` the
# gate no longer holds anything, so t0 lands on whichever g-turn first crosses the fill — and the
# remaining g-turns are "read this file IN FULL", each of which can write more new tail than a narrow
# span is wide. If that happens the warm block falls OUTSIDE the population every verdict below is
# scoped to, and the arm reports on one or two large file reads instead.
#
# That is trap 3 in timely-compact-validation.md, which was written up as a property of the design
# before it was understood as a rig fault. It fails loudly here rather than quietly: the verdicts would
# go 0 == 0 and read as a pass on an empty population.
MIN_WARM = 3
if len(sh) < MIN_WARM:
    print()
    print("ARRANGEMENT FAILED — %d warm hit turns inside the span, want >= %d." % (len(sh), MIN_WARM))
    print("  The span closed before the warm block. t0 is turn %d of %d, the panel counted %d turns,"
          % (t0, len(rows), turns))
    print("  and this arm's verdicts are scoped to those. See trap 3 in")
    print("  docs/proposals/timely-compact-validation.md: bridging turns that read files in full can")
    print("  consume the whole span before the turns being measured begin.")
    print("  Fix the ARRANGEMENT (fewer/smaller file reads before the crossing, or a wider span via")
    print("  scen_panel's fill argument) — do not read the verdicts below.")
    raise SystemExit(1)
# And no single turn inside the span may dominate it, which is the same fault one step earlier: a span
# that technically contains the warm block but is 90% one file read is not measuring warm turns either.
# OVER span[1:], THE SAME POPULATION THE VERDICTS USE. Computing this over the whole span included
# t0, and t0 can legitimately be the largest row: isFreshSummary also matches `fresh_summary`, the
# SPLICING turn, which carries a large saved_gross — and its INFERRED fallback exists because rows
# without `summary_started` are a real shape. On such a run the guard would blame an arrangement fault
# that is not there and abort a paid run for it, which costs the same as a false pass and teaches the
# wrong lesson.
rest = span[1:]
biggest = max((r[2] for r in rest), default=0)
total = sum(r[2] for r in rest) or 1
if biggest > 0.6 * total:
    print()
    print("ARRANGEMENT SUSPECT — one turn is %.0f%% of the span's removed tokens." % (100.0 * biggest / total))
    print("  The population is dominated by a single turn, so the read-rate check below is really a")
    print("  check on that one turn. Same cause as trap 3; rearrange before believing the verdicts.")
    raise SystemExit(1)
gross, usdsum = sum(r[2] for r in sh), sum(r[3] for r in sh)
cg = sum(r[5] for r in span)
READ = float(os.environ.get("CG_SCEN_READ_RATE", "1e-7"))
print()
print("HAND, over exactly the %d turns the panel counted:" % len(span))
print("  sum(saved_gross) on hit turns in span   %d" % gross)
print("  x the cache-READ rate                   %.8f   <-- read_credit must equal this" % (gross * READ))
print("  sum(saved_usd)   on hit turns in span   %.8f   <-- and must NOT equal this" % usdsum)
print("  sum(cg_llm_cost) over the span          %.8f" % cg)
print()
rc = e.get("read_credit_usd", 0)
def verdict(ok):
    return "PASS" if ok else "FAIL"
print("  read credit at the READ rate      %s" % verdict(abs(rc - gross * READ) < 1e-9))
print("  read credit != stored saved_usd   %s" % verdict(abs(rc - usdsum) >= 1e-9))
print("  cold credit is zero               %s" % verdict(e.get("cold_credit_usd", 0) == 0))
print("  summarizer cost is charged        %s" % verdict(e.get("summarizer_cost_usd", 0) > 0))
PY
echo "=== SCENARIO C done $(date -u +%H:%M:%S) ==="
