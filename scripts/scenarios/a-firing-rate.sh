#!/bin/bash
# SCENARIO A — SHIPPED DEFAULTS, NATURAL TRAFFIC. Measures the FIRING RATE.
#
# This is the arm a review ran and the arm my own acceptance run never had: shipped defaults, a real
# Claude Code session, and NO injected idle gaps. The question is not "does the mechanism work" — the
# forced arms answer that — it is "how often does the gate open on traffic nobody arranged".
#
# ⚠️ WHAT THIS ARM MEASURED, AND WHY IT NO LONGER MEASURES THE SAME THING. When it was written the
# shipped cache_state was `pre_expiry_or_cold`, and this arm returned 0 fires in 53 turns with
# `cache_state_declined_warm` on 51. That result is what withdrew the cold-gated states: the default
# is now `any`, so the FILL gate is the only gate and a firing rate near zero now means the session
# never reached 0.9 of C, not that the cache stayed warm. cache_state is deliberately not written
# below, so this arm keeps tracking the shipped default rather than a value frozen into the script.
#
# It also records the one number that decides whether 0.9 is reachable at all on this client: where
# Claude Code runs its OWN compaction. If the client caps the transcript below 0.9 of the model
# window, the shipped gate cannot fire on this model no matter how long the session runs, and the
# firing rate is zero for a structural reason rather than a statistical one.
set -u
. "$(dirname "$0")/lib.sh"

N=shipA
PORT=4211

echo "=== SCENARIO A: shipped defaults, natural traffic (firing rate) ==="
scen_build
scen_start  "$N" "$PORT" 0.9
scen_home   "$N" "$PORT"
scen_work   "$N"

scen_turn "$N" t01 fresh    "Read a.go and list every exported function with a one-line summary."
for i in 02 03 04 05 06 07 08 09 10 11 12 13 14; do
  case $i in
    02) P="Read b.go in full. Summarise how the economic gate decides to skip a candidate.";;
    03) P="Read c.go in full. Explain how the conversation partition is chosen.";;
    04) P="Read d.go in full. Explain the episode span axis and why the two obvious axes fail.";;
    05) P="Read e.go in full. List every place usage is attributed to a request.";;
    06) P="Read f.go in full. Explain how the boundary is computed with and without a Tracker.";;
    07) P="Read g.go in full. Explain baselineDeltaUSD and repeatRate.";;
    08) P="Read h.go in full. Explain every conjunct of Fires and CacheAllows.";;
    09) P="Compare a.go and d.go: which facts does each one own, and where could they drift?";;
    10) P="Re-read c.go and g.go. List every hardcoded cache TTL and where each comes from.";;
    11) P="Re-read b.go and e.go. List every counter and where it is exported.";;
    12) P="Across every file you have read, list each distinct token 'ruler' and its unit.";;
    13) P="Re-read d.go and h.go and list every place a dollar figure could be priced twice.";;
    14) P="Summarise everything you have learned about this codebase in fifteen bullets.";;
  esac
  scen_turn "$N" "t$i" continue "$P"
done

echo
echo "=== SCENARIO A: all rows ==="
scen_tail "$N" 60
scen_panel "$N" "$PORT"

echo
echo "=== SCENARIO A: the firing-rate verdict ==="
SCEN_DB="$SCEN_ROOT/$N/dash.db" CG_SCEN_WINDOW="${CG_SCEN_WINDOW:-200000}" python3 - <<'PY'
import os, sqlite3, json
# The window is NOT a column on `requests` — it is resolved per request from the model id — so it is
# a parameter here. Set CG_SCEN_WINDOW for a model other than the 200,000-token default.
WIN = int(os.environ["CG_SCEN_WINDOW"])
c = sqlite3.connect("file:%s?mode=ro" % os.environ["SCEN_DB"], uri=True)
rows = list(c.execute("""
  SELECT r.id, r.fresh_input+r.cache_read+r.cache_write AS billed, r.cache_miss_reason,
         r.tokens_before, COALESCE(cp.events,''), COALESCE(cp.gates,''), r.model, r.ts
  FROM requests r LEFT JOIN request_components cp
    ON cp.request_id=r.id AND cp.component='summarize'
  WHERE r.keepalive=0 ORDER BY r.ts"""))
if not rows:
    print("NO ROWS — the run never reached the proxy"); raise SystemExit

peak = max(r[1] for r in rows)
print("model(s)                 %s" % sorted({r[6] for r in rows}))
print("turns                    %d" % len(rows))
print("window (parameter)       %d" % WIN)
print("peak billed input        %d" % peak)
print("peak fill                %.3f     <-- the gate needs 0.900 (= %d billed)" % (peak/WIN, int(0.9*WIN)))
print("turns at or over the fill gate   %d of %d" % (sum(1 for r in rows if r[1] >= 0.9*WIN), len(rows)))

v = {}
for r in rows:
    v[r[2]] = v.get(r[2], 0) + 1
print("cache verdicts           %s" % v)
fired = [r for r in rows if '"summary_started"' in r[4] or '"fresh_summary"' in r[4]]
print("TURNS THAT FIRED         %d of %d" % (len(fired), len(rows)))

g = {}
for r in rows:
    if not r[5]:
        continue
    try:
        for k, n in json.loads(r[5]).items():
            g[k] = g.get(k, 0) + n
    except Exception:
        pass
print("gates (summed)           %s" % g)

# WHICH GATE BINDS, which is the whole point of this arm. `below_request_trigger` means the fill gate
# closed; `cache_state_declined_warm` means the cache gate did. If the second dominates while the
# first is small, the fraction is reachable and the CACHE STATE is what the default is really gated on.
print()
print("the binding gate         fill=%d  cache=%d" % (
    g.get("below_request_trigger", 0), g.get("cache_state_declined_warm", 0)))

# AND WHY THE CACHE GATE CLOSES: pre_expiry needs >= (TTL - pre_expiry_seconds) of idle, i.e. 240s on
# a 5-minute entry. Continuous agent work does not produce gaps that long, and this is the measurement
# that says so rather than assuming it.
gaps = [(rows[i][7]-rows[i-1][7])/1000.0 for i in range(1, len(rows))]
if gaps:
    gs = sorted(gaps)
    print("inter-turn gaps (s)      min=%.1f median=%.1f max=%.1f" % (gs[0], gs[len(gs)//2], gs[-1]))
    print("  gaps >= 240s (the idle pre_expiry needs)   %d of %d" % (
        sum(1 for x in gaps if x >= 240), len(gaps)))

# THE CLIENT'S OWN CEILING. If Claude Code compacts below 0.9 of the window, the shipped gate cannot
# fire on this model however long the session runs — a structural zero rather than a statistical one.
drops = [(rows[i-1][0], rows[i-1][1]) for i in range(1, len(rows))
         if rows[i][3] and rows[i-1][3] and rows[i][3] < rows[i-1][3]*0.7]
if drops:
    hi = max(b for _, b in drops)
    print("CLIENT COMPACTED         yes, after row(s) %s" % [a for a, _ in drops])
    print("  client's own ceiling   %d billed = %.3f of the window (the gate needs 0.900)" % (hi, hi/WIN))
else:
    print("CLIENT COMPACTED         no — the session never reached its own ceiling in this run")

print()
print("--- every row ---")
for r in rows:
    ev = r[4].replace('"','').replace('{','').replace('}','')[:40]
    print("  #%-4d billed=%-7d %-13s before=%-7d fill=%.3f %s" % (r[0], r[1], r[2], r[3], r[1]/WIN, ev))
PY
echo "=== SCENARIO A done $(date -u +%H:%M:%S) ==="
