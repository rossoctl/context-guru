#!/bin/bash
# SCENARIO B — THREE COLD EVENTS INSIDE THE SPAN. The headline bucket, accumulating.
#
# The claim the panel's ColdCreditUSD bucket exists to measure: a cache expiry that happens AFTER a
# summary re-creates the COMPACTED prefix instead of the full one, and the difference is a prevented
# rewrite. My acceptance run observed exactly one such event. One event cannot distinguish "the
# credit is computed" from "the credit ACCUMULATES per event", and it is the accumulation that says
# the amortisation model is right.
#
# So: fire once, then force the cache cold three separate times inside the same 10% span, and check
# the bucket is three prevented rewrites rather than one.
#
# WHY THREE COLDS FIT INSIDE ONE SPAN, which is the property this arm exists to demonstrate:
#
# The span advances on cumulative NEW content. A cold event happens because of ELAPSED TIME. Those are
# independent, and they are anti-correlated in the helpful direction — a session idle enough for its
# cache entry to lapse is BY DEFINITION not accruing new content, so the span cannot be closing while
# a cold event becomes possible. A session can go cold having added 2% of the window.
#
# Two things had to be right for that to hold, and one of them is this script's own prompts:
#
#   - cache_write on a MISS is re-creation rather than new content, so a cold turn advances the span
#     by only its few tokens of fresh input. Under the axis this PR replaced (cumulative SPEND) the
#     first cold turn would have closed the span on its own.
#   - the turns BETWEEN t0 and the idle gaps must add almost nothing. The first version of this arm
#     asked them to read files, they wrote 20,095 tokens of new tail against a 20,000 span, and the
#     span closed before any gap. That produced a $0.00 cold credit and an incorrect conclusion that
#     a cold event "essentially cannot fall inside a 10% span".
#
# min_request_frac is 0.5, NOT the shipped 0.9, and Scenario A is the arm that measures why.
set -u
. "$(dirname "$0")/lib.sh"

N=cold3
PORT=4212
# THE GAP MUST EXCEED THE TTL WITH ROOM TO SPARE, and the reason changed while the number did not.
#
# It was 380 because the GATE demanded it: a strict cold test required a 60s clock-skew margin past
# nominal expiry before it would claim an entry was gone, so at 310s idle the arithmetic read "-10s
# remaining" — inside the allowance — and `pre_expiry_or_cold` declined even though the provider had
# already dropped the entry (that turn billed a full 183,660-token rewrite with zero cache read).
#
# That gate no longer exists; the cold-gated states were withdrawn and the default is `any`, so
# nothing here waits for the arithmetic's permission. 380 stays anyway, because what this arm needs is
# a genuinely COLD turn from the PROVIDER, and the provider's entry can outlive its nominal lifetime:
# a measured turn at 383s came back a PARTIAL hit (read=22,441 write=10,829). So a wait can only make
# a cold turn likely, never certain — which is why the checks below read the verdict they actually got
# instead of assuming one.
GAP=380   # > TTL (300), with margin for a provider entry that outlives its nominal lifetime

echo "=== SCENARIO B: three cold events inside one span ==="
scen_build
scen_start  "$N" "$PORT" 0.5
scen_home   "$N" "$PORT"
scen_work   "$N"

# --- Grow the transcript past 0.5 of haiku's 200k window.
scen_turn "$N" g01 fresh    "Read a.go and list every exported function with a one-line summary."
scen_turn "$N" g02 continue "Read b.go in full. Summarise how the economic gate decides to skip a candidate."
scen_turn "$N" g03 continue "Read c.go in full. Explain how the conversation partition is chosen."
scen_turn "$N" g04 continue "Read d.go in full. Explain the episode span axis."
scen_turn "$N" g05 continue "Read e.go in full. List every place usage is attributed to a request."
scen_turn "$N" g06 continue "Read f.go in full. Explain how the boundary is computed."
scen_turn "$N" g07 continue "Read g.go in full. Explain baselineDeltaUSD and repeatRate."

echo "--- fill before forcing the gate:"
scen_tail "$N" 3

# --- FIRE: one gap past the TTL, with the transcript full. This turn commissions the summary.
scen_sleep "$GAP" "past the TTL so the gate sees a cold cache; this turn fires"
scen_turn "$N" f01 continue "Reply with exactly: ok"

# --- The splice lands on the NEXT turn, because the summary is produced off the hot path.
#
# EVERY TURN FROM HERE ON MUST ADD ALMOST NO NEW CONTENT, and getting that wrong invalidated the
# first version of this arm. These prompts used to be "name one risk in d.go" and "name one thing
# c.go owns", which makes the agent READ those files: the two bridging turns wrote 3,809 and 16,278
# tokens of new tail, 20,095 against a 20,000 span, and the span closed before the first idle gap.
# The run then reported cold_credit_usd = $0.00 with three cold events sitting outside the episode,
# which reads exactly like the credit being broken.
#
# The span advances on NEW CONTENT and a cold event needs ELAPSED TIME, so an arm testing cold events
# must spend the former as slowly as possible. A trivial prompt adds a few tokens; a file read adds
# sixteen thousand.
scen_turn "$N" s01 continue "Reply with exactly: ok"
echo "--- after the summary landed (billed should have COLLAPSED):"
scen_tail "$N" 4

# --- THREE COLD EVENTS, each inside the span. Each must re-create the COMPACTED prefix.
for k in 1 2 3; do
  scen_sleep "$GAP" "cold event $k of 3, inside the span"
  scen_turn "$N" "c0$k" continue "Reply with exactly: ok"
done

echo
echo "=== SCENARIO B: all rows ==="
scen_tail "$N" 60
scen_panel "$N" "$PORT" "" 0.5

echo
echo "=== SCENARIO B: hand-derived against the panel ==="
SCEN_DB="$SCEN_ROOT/$N/dash.db" SCEN_PANEL="$SCEN_ROOT/$N/panel.json" python3 - <<'PY'
import os, sqlite3, json
c = sqlite3.connect("file:%s?mode=ro" % os.environ["SCEN_DB"], uri=True)
rows = list(c.execute("""
  SELECT r.id, r.fresh_input+r.cache_read+r.cache_write, r.cache_read, r.cache_write,
         r.cache_write_1h, r.cache_miss_reason, COALESCE(c.saved_gross,0),
         COALESCE(c.saved_usd,0), COALESCE(c.events,''), r.cg_llm_cost_usd, r.model
  FROM requests r LEFT JOIN request_components c
    ON c.request_id=r.id AND c.component='summarize'
  WHERE r.keepalive=0 ORDER BY r.ts"""))

# t0 = the turn that commissioned the summary.
t0 = next((i for i, r in enumerate(rows)
           if '"summary_started"' in r[8] or '"fresh_summary"' in r[8]), None)
if t0 is None:
    print("NO SUMMARY WAS COMMISSIONED — the scenario did not reach its own precondition.")
    raise SystemExit

print("t0 is row #%d (billed=%d write=%d %s)" % (rows[t0][0], rows[t0][1], rows[t0][3], rows[t0][5]))
after = rows[t0+1:]
colds = [r for r in after if r[5] == 'ttl_expiry']
hits  = [r for r in after if r[5] == 'hit']
print("turns after t0           %d   (ttl_expiry=%d, hit=%d)" % (len(after), len(colds), len(hits)))
print()
print("THE COLD EVENTS INSIDE THE SPAN")
for r in colds:
    print("  #%-4d wrote %-7d tokens of prefix, removed %-7d (our count)" % (r[0], r[3], r[6]))

# The counterfactual: what an UNCOMPACTED prefix would have written on each cold turn. The best
# evidence on hand is the last pre-summary full prefix, which t0 itself re-created.
full = rows[t0][3] or max((r[3] for r in rows[:t0+1]), default=0)
print()
print("counterfactual full prefix   %d tokens (t0's own re-creation)" % full)
if colds:
    comp = sum(r[3] for r in colds) / len(colds)
    print("actual compacted prefix      %.0f tokens (mean over the %d cold turns)" % (comp, len(colds)))
    print("prevented per cold event     %.0f tokens" % (full - comp))
    print("prevented over %d events      %.0f tokens" % (len(colds), len(colds)*(full-comp)))

# Rates, straight off the row's own model, as the panel prices them.
rate = list(c.execute("SELECT DISTINCT model FROM requests LIMIT 1"))
print()
print("PANEL vs HAND")
# READ THE EPISODE, NOT THE GROUP. A provenance group's credit fields accumulate over CLOSED episodes
# only — open ones are reported apart in open_net_usd, by design. Reading the group printed
# 0.00000000 for every bucket on a run whose episode was open, which reads exactly like "the credit
# is broken" when the credit is fine and sitting in the other field.
pan = json.load(open(os.environ["SCEN_PANEL"]))
for g in pan.get("by_provenance", []):
    print("  GROUP provenance=%s closed=%s open=%s settled_net=%.8f open_net=%.8f open_turns=%s" % (
        g.get("provenance"), g.get("closed"), g.get("open"), g.get("net_usd", 0),
        g.get("open_net_usd", 0), g.get("open_turns")))
eps = pan.get("episodes", [])
for e in eps[:3]:
    print("  EPISODE state=%s turns=%s new_content=%s" % (
        e.get("state"), e.get("turns"), e.get("new_content_billed")))
    print("    cold_credit_usd   %.8f" % e.get("cold_credit_usd", 0))
    print("    read_credit_usd   %.8f" % e.get("read_credit_usd", 0))
    print("    invalidation      %.8f" % e.get("invalidation_debit_usd", 0))
    print("    summarizer_cost   %.8f" % e.get("summarizer_cost_usd", 0))
    print("    net_usd           %.8f" % e.get("net_usd", 0))

# SCOPE THE HAND CHECK TO THE TURNS THE PANEL COUNTED, which is t0 plus (turns-1) rows. Summing over
# everything after t0 compares two different populations, and on this run it disagreed by three whole
# cold events — the span had closed before any of them.
turns = int((eps[0].get("turns") if eps else 0) or 0)
span = rows[t0:t0 + turns] if turns else []
sc = [r for r in span[1:] if r[5] == 'ttl_expiry']
sh = [r for r in span[1:] if r[5] == 'hit']
print()
print("  HAND, over exactly the %d turns the panel counted:" % len(span))
print("    sum(saved_gross) on ttl_expiry turns IN SPAN = %d   x the cache-WRITE rate" % sum(r[6] for r in sc))
print("    sum(saved_gross) on hit turns        IN SPAN = %d   x the cache-READ rate" % sum(r[6] for r in sh))
print("    (the panel's cold and read buckets must equal those two products)")
if turns and not sc:
    print()
    print("  NOTE: the span contains NO cold event, so cold_credit_usd is legitimately 0. That is a")
    print("  finding about the SPAN, not about the credit: at 10%% of the window the episode closes")
    print("  after two or three warm turns — tens of seconds — while a cache expiry needs minutes.")
    print("  Re-query with ?span= wide enough to contain the cold turns to see the credit accumulate.")
PY
echo "=== SCENARIO B done $(date -u +%H:%M:%S) ==="
