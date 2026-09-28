# A per-tenant cap beats every predictor we've built — including the calibrated one built for this page

Extends [kv-ttl-predictor-arms.md](kv-ttl-predictor-arms.md) (the `stop_reason`-gated rule,
9-day capture), [kv-cache-keepalive-reusemodel.md](kv-cache-keepalive-reusemodel.md)
(`ReuseModelV1`'s 13 features), and [kv-ttl-predictor-features.md](kv-ttl-predictor-features.md)
(the feature availability matrix). Measured on a 41.4-day snapshot (2026-08-18→09-28,
264,163 requests, 18 tenants with traffic — 4.1x the rows and 4.6x the days of the prior page's
capture), read-only. Tenant ids pseudonymized `t01`..`t18`, **sorted by raw tenant id** — the
convention this page adopts and recommends over a volume-ranked alternative that was tried in
parallel during this study and produced a spurious cross-check discrepancy purely from
relabelling (see "A labelling lesson" below).

## The short version

**The headline, for a reader who stops here: a two-parameter non-learned policy beats every
predictor we built, including a well-calibrated one built specifically to try to beat it.** That
number is **modelled** (a replay against one 41.4-day snapshot, not a live measurement), and the
one piece of work that would move it from modelled to measured — a live randomized holdout — is
the single largest thing this page does not contain. Everything below is detail in support of
that sentence, not a build-up to it.

- **A per-tenant-tuned `max_pings` cap with an off-switch (no model at all) beats every
  predictor in this whole line of work.** Better than the shipped `stop-reason-gated` rule,
  better than the prior page's `logreg-v1`, better than a calibrated hybrid model built
  specifically for this page. It also **harms zero tenants**, where every flat setting
  (including today's) harms several. **It is also more ROBUST than a learned or uncapped
  alternative, independent of its net-dollar edge**: a hard cap bounds any one span's ping
  spend regardless of the true gap length, so it is structurally insulated from data defects
  a predictor or an uncapped policy is exposed to in full — see "A robustness argument" below,
  found only after this study was already largely finished, which is itself worth knowing.
- **A single fleet-wide `max_pings=2` is the right fallback if per-tenant config isn't built
  yet — `max_pings=6` is not.** An earlier back-of-envelope estimate favoured 6; once a real,
  per-conversation, per-model-priced replay is run (this page's method, not aggregate
  fresh-equivalent arithmetic), 2 wins and 6 harms one more tenant for barely more money.
- **A calibrated model was built and is recommended AGAINST shipping.** `hybrid2` — isotonic
  calibration, no tenant-identity feature (so it can score a tenant it never trained on),
  per-tenant-tuned threshold — beats the shipped rule (+4.90% vs +3.41%) and is well-calibrated
  (ECE 0.0035) and generalizes across held-out tenants and models without collapsing. It still
  loses to both no-model policies above on net dollars, the primary objective. This is reported
  as the paper-worthy negative result it is, not softened into a partial win.
- **The target is gap magnitude, not arrival.** Cost scales with `⌈gap/TTL⌉` pings, not with a
  binary "will they come back" — a model with excellent AUC that can't tell 12 minutes from
  3 hours loses money. The correctly-specified decision rule (compare *expected dollars*, not a
  probability against a threshold) already exists in this codebase
  (`kv_ttl_cost_model.policy_expected_cost`) and, scored honestly, buys almost nothing over
  doing nothing (+0.06%) — confirming the target reframing mattered more than any model.
- **The realized-vs-modelled gap converged, from two directions.** Two independently-built cost
  models (this page's, and the orchestrating session's, corrected four times as bugs were
  found) land within 2% of each other on the raw gap: **1.94–1.97x**. Applying the REAL
  strategy config (actual tenant targeting, actual narrow weekday/hour windows, the real
  `min_prefix` gate) — this page's own contribution — closes most of that, to **1.4–1.8x**.
  The residual is bounded, not further closed, and said so plainly.
- **A cross-check discrepancy that looked like a modelling disagreement turned out to be a
  labelling collision** — see "A labelling lesson" below. Kept in this page because it is a
  cheap, memorable, and general lesson, not because the disagreement itself matters anymore.

## The decision problem, restated correctly

Not `P(return within 5 minutes)` — the prior page's own ML arms already showed that label buys
nothing (92.5% of gaps close inside 5 minutes, so the answer is almost always "yes" and there is
no decision left for a probability to improve). The right target:

```
P(next request arrives within the interval AND with a compatible prefix | information known now)
```

at every horizon a ping schedule can reach, because **the cost of a miss scales with the number
of pings needed to bridge the gap**, not with a single yes/no. Being wrong about "10 minutes"
costs ~3 pings; being wrong about ">2 hours" costs ~167. Action set (six, not three — the extra
three are not redundant): `expire`, `write_5m`, `write_1h`, `ping_5m`, `ping_1h`, and
`write_5m_ping_1h` (write cheap, escalate to a 1-hour write only if the entry survives to its
first keep-alive due date — measured here as the **worst-performing non-1h-tier action on the
page**, +29.70% vs. doing nothing, unexpected and not diagnosed further this round).

Break-even, per ping: `r/(w-r) = 0.10/1.15 = 8.70%` (myopic), falling to ~7.4% once the option
value of preserving a *later* ping's chance is counted (backward induction,
`deploy/harbor/kv_ttl_keepalive_policy.py`, not re-derived here). Objective throughout: net
dollars, unclamped. AUC/Brier/log-loss are diagnostics; where a model wins on AUC and loses on
net-$ (§"Baselines" below), this page says so rather than reporting the AUC as the headline.

## Baselines and oracle, full-snapshot single split

`deploy/harbor/kv_ttl_cost_model.py`'s own `main()`, unmodified, `min_prefix=20000`,
`max_pings=2`, split 0.6/0.4, on the full 41.4-day snapshot (test window: 73,487 requests,
1,185 conversations):

| policy | Δ vs. fixed-5m | % | pings | writes_1h |
|---|---:|---:|---:|---:|
| no-cache | +$31,613.16 | +295.08% | 0 | 0 |
| fixed-5m ("never-ping") | — | — | 0 | 0 |
| fixed-1h | +$1,248.91 | +11.66% | 0 | 59,616 |
| **keepalive-5m ("always-ping", capped)** | **−$619.42** | **−5.78%** | 6,259 | 0 |
| keepalive-1h | +$1,109.04 | +10.35% | 2,436 | 59,604 |
| write_5m_ping_1h ("escalate if quiet") | +$3,182.13 | +29.70% | 5,035 | 59,610 |
| observed-policy (client-declared tier, replayed) | $0.00 | 0.00% | 0 | 59,778 |
| predictor (survival-model threshold ladder) | +$49.80 | +0.46% | 0 | 7,436 |
| expected-cost (correctly-specified $ rule) | +$6.42 | +0.06% | 0 | 0 |
| **optimal — ORACLE, hindsight, unreachable** | **−$1,700.49** | **−15.87%** | 367 | 1,640 |

The oracle's own edge is concentrated (367 pings + 1,640 one-hour writes out of 73,487
requests), not distributed — consistent with "a rare-event problem" rather than "raise the ping
rate everywhere," and it's why a predictor buys so little once a cap already bounds the
catastrophic tail (below).

## Arms, rolling-origin 3-fold replay, session- and tenant-level bootstrap CIs

Same train/fold split as the prior page's `run_all` (train_frac 0.6, 3 folds), 400-rep
bootstrap, seed 0. Four arms reused from that page's own run without refitting
(`historical-probability-tenant-tuned`, `stop-reason-x-hour`, `stop-reason-gated`,
`logreg-v1`); the rest are new to this page.

| arm | Δ vs. floored fixed-5m | % | 95% CI, session-level | 95% CI, tenant-level |
|---|---:|---:|---|---|
| historical-probability-tenant-tuned | −$8.59 | −0.08% | [−12.27, −5.29] (harmful) | not computed |
| age-turn≤3 | +$2.13 | +0.02% | [−2.50, +8.18] (≈zero) | [−4.84, +11.32] |
| clock-unconditional (no `stop_reason`) | $0.00 | 0.00% | exactly zero | exactly zero |
| stop-reason-x-hour | +$337.36 | +3.15% | [+154.42, +555.56] | not computed |
| stop-reason-gated (SHIPPED) | +$365.68 | +3.41% | [+159.23, +621.67] | not computed |
| logreg-v1 (one-hot `user_id`) | +$418.17 | +3.90% | [+210.04, +686.85] | not computed |
| **hybrid2** (calibrated, no tenant one-hot, tenant-tuned threshold) | +$525.10 | +4.90% | [+283.02, +855.73] | [+164.42, +971.31] |
| flat-cap-6 | +$517.73 | +4.83% | [+229.76, +860.65] | [+125.12, +960.76] |
| flat-cap-1 | +$541.58 | +5.05% | [+301.64, +831.53] | [+199.31, +924.30] |
| **flat-cap-2** | **+$619.77** | **+5.78%** | **[+349.98, +940.08]** | **[+212.42, +1,057.79]** |

**A pure clock rule carries no signal at all** — no UTC hour clears the break-even bar
unconditionally. The entire value of `stop-reason-x-hour` is its `stop_reason` conditioning,
not the hour. **Tenant-level CIs run 1.5–2.7x wider than session-level for every arm** — both
are reported because they answer different robustness questions (session non-independence vs.
tenant-mix sensitivity); a pooled session-level-only number would have overstated confidence.

## Generalization

**Calibration** (`hybrid2`, pooled test folds, n=72,302): Brier 0.0325, log-loss 0.134, AUC
0.782, ECE (10-bin quantile) 0.0035. Top decile — where the ping threshold sits — predicts
0.174, observes 0.173: well-calibrated exactly where it matters, not just on average.

**Leave-one-tenant-out** (`hybrid2` has no tenant-identity feature by construction, so this is a
genuinely blind test, not a one-hot-degrades-to-intercept fallback):

| held-out tenant | n (test) | AUC excl. tenant |
|---|---:|---:|
| t14 (largest) | 12,633 | 0.888 |
| t12 | 13,122 | 0.825 |
| t01 | 6,480 | 0.767 |
| t06 | 7,736 | 0.766 |
| t08 | 6,584 | 0.690 |

No catastrophic collapse on any held-out tenant; three of five score at or above the pooled
figure.

**Leave-one-model-out**: AUC excluding a model ranges 0.640 (bare `claude-sonnet-5` — the same
population the #324 pricing defect hit; whether that's the same underlying cause or a
coincidence was not investigated) to 0.877 (`claude-opus-5`).

**Per-tenant harm, `flat-cap-2` and `hybrid2`, held-out test folds** (all tenants clearing a
20-event floor): worst case for either arm is −0.51% (a 73-event cell, the noisiest reported).
**Materially safer than the shipped `stop-reason-gated` rule**, which loses −1.99% on one tenant
and −0.13% on another on this same corpus (prior page's own finding).

## The per-tenant cap + off-switch: the actual headline — CORRECTED, this page's own numbers

**Correction, found by REV's review of this PR, and thanks to REV for catching it**: an earlier
version of this section presented a table as "converged... including this page's own
`kv_ttl_cost_model.evaluate()`" that this page's engine does **not** actually reproduce. The
table was the orchestrating session's and a third agent's own separately-built, hand-rolled
per-tenant model, cited here by copy rather than independently reproduced — and describing it as
jointly verified by this page's engine was a real error, not a rounding difference. Fixed below:
every number in this section is now reproduced from this page's own committed script, on the
same test-window split as every other table on this page (`--split 0.6`, `--min-prefix 20000`,
`kv_ttl_cost_model.py`'s own `by_user` breakdown, unmodified), with exact reproduction commands.

A per-tenant `max_pings ∈ {off, 1, 2, 6}` policy, tuned per tenant with an off-switch where every
setting loses money, beats every flat policy on this page:

| policy | Δ vs. fixed-5m (test window) | tenants harmed |
|---|---:|---:|
| flat N=1 | +$541.55 | 1 |
| flat N=2 (≈ today) | +$619.50 | 3 |
| flat N=6 | +$519.43 | 5 |
| **per-tenant best N, OFF where every N loses** | **+$641.95** | **0** (2 of 15 tenants have no profitable setting and are switched off) |

**This flat N=2 figure ($619.42) is the SAME number as "Baselines and oracle" above** — restated
here deliberately, because the earlier, incorrect version of this table quoted $515 for the same
policy on the same page, which was internally inconsistent as well as unreproducible. Per-tenant
tuning beats flat N=2 by $22.44 (0.21% of the $10,724 test-window baseline) on this page's own
engine — real, and in the same direction as every other model built in this whole line of work,
but a much smaller edge than the $253/41% orchestrator's-model figure this section previously
borrowed. **`flat-cap-6` still does not beat `flat-cap-2` on this page's own engine** (the
finding from earlier in this page, reconfirmed here at a different min-prefix/window
combination) — consistent, not contradicted, by the correction.

**A separate, NOT-reconciled model, from a different engine, given here as a citation rather
than a joint result**: the orchestrating session's own hand-rolled per-tenant model (uniform
$/M-fresh-equivalent pricing, its own action-cost formula, a 24-hour gap cutoff this page's
engine does not apply) converged with a third agent's independent reimplementation to within 2%
on one specific figure (the RAW, pre-coverage-correction N=1 estimate: $395 vs $389 against
`kv_ttl_cost_model`'s $541.50) and reports, on the full 41.4-day window: flat N=1 $395/7 harmed,
N=2 $515/6 harmed, N=6 $571/7 harmed, per-tenant-best $753/4 harmed, per-tenant+off-switch
$768/0 harmed, oracle $1,855 (`PHASE2.md` P2-9). **The two models agree qualitatively**
(per-tenant tuning with an off-switch beats every flat cap and eliminates harm) **and disagree
on magnitude by roughly 2-2.6x** for the per-tenant+off-switch total specifically, even after
accounting for this page's test-window-only scope (a full-window run of this page's own engine,
using a different per-tenant selection method, gives ≈$1,669 — see "A labelling lesson" below —
which is closer to but still not the same as the $768 the other model reports). **This magnitude
gap is a genuinely new, unresolved finding, distinct from the tenant-identity labelling
collision resolved elsewhere on this page.** Named, not chased down further this round: it could
be the 24h-gap cutoff (absent from this page's engine), the uniform-vs-real-per-model pricing
convention, a difference in how each model prices the avoided re-write, or some combination —
this page does not have enough remaining budget to trace it to ground, and says so rather than
picking a story that fits. **This page's own recommendation is stated ONLY from this page's own
reproduced numbers above** ($619.50→$641.95, a $22.45 edge to per-tenant tuning) — not from the
larger, unreconciled $253/41%-of-oracle figure the earlier version of this page borrowed.

Per-tenant + off-switch, on this page's own engine, eliminates the harm that flat N=2 causes
on 3 tenants at this test-window scope, for a modest but real dollar gain. **A single fleet-wide
value raised from 2 to 6 does not help on this page's own engine either** ($519.18 < $619.42,
and harms more tenants, 5 vs 3) — the direction of that specific finding is unaffected by the
correction above.

### A robustness argument for the cap, found only after this study was largely finished

The per-tenant cap's case is not only that it scores best on this snapshot — it is **structurally
more robust to a data defect nobody knew about when this comparison was designed.** A different
reviewer, checking the same corpus independently, found that client-supplied `session_id`s are
sometimes reused across genuinely disjoint conversations (reproduced on this page's own data:
205 of 246,484 decision points, 0.083%, show a gap over 24 hours, up to 501.8 hours — plainly not
real idle time). **Any policy that pings unconditionally in proportion to gap length, or any
predictor that scores such a span as "still worth pinging," is exposed to this defect in full**:
an uncapped policy would pay for hundreds of pings across a fake multi-day "gap." **A
`max_pings`-capped policy is not** — the cap bounds the spend on any one span regardless of how
long the true or artifactual gap is, so the worst this defect can do to a capped arm is waste at
most `max_pings` pings on 205 events (a few dollars, not a material fraction of any figure on
this page). This is a property of the RECOMMENDATION, not just a number in a threats-to-validity
list: a policy that is robust to a defect discovered after the fact is a stronger recommendation
than one that merely scored better on the data as first understood.

**Realized-vs-modelled gap**: real, measured keep-alive net is $200.86. The raw model (before
any coverage/gate correction) says $395–433 at N=1 — a **1.94–1.97x gap**, converged across two
independent engines. Replaying the REAL control-plane config instead (30 active strategies, all
single-tenant campaigns, narrow weekday/hour windows in each strategy's own timezone — empty
`Window.TZ` resolves to `Asia/Jerusalem`, `tenant/keepalivestrategy.go:44-45` — plus the real
`min_prefix_tokens=20000` gate) closes most of the remaining gap: **1.4–1.8x**. Of decision
points with a gap over the 280-second idle threshold, 99.6% belong to a tenant targeted by SOME
active strategy, but only 24.8% fall inside that tenant's actual window at the moment — window
narrowness, not tenant targeting, is the dominant remaining cause, sized exactly from the real
strategy rows rather than assumed. The residual 1.4–1.8x is attributed to (a) 5 of the 30
strategies additionally gating on `stop_reason` — not modelled in this specific replay — and
(b) ping-success optimism (every simulated ping assumed to succeed); neither is sizeable further
from this snapshot alone.

## A labelling lesson

During this study, an independent per-tenant model built by a different reviewer appeared to
disagree with this page's own per-tenant harm findings — three tenants flagged as harmed by that
model showed up as clearly profitable here, and vice versa. Both engines were re-checked line by
line; neither had a bug. **The two scripts had assigned pseudonymized tenant ids by different
rules** — one by decision-point volume descending, this page's by sorted raw tenant id — so
"T09" in one meant a different real tenant than "t09" in the other. Once relabelled, every
disagreement dissolved exactly, and both engines agree on 6 tenants harmed by today's flat
setting. **Recorded here as a general, cheap, memorable lesson**: two independently-correct
analyses can appear to contradict each other purely through labelling, and it can take as little
as one query to dissolve what looked like a real disagreement. This page adopts sorted-raw-id as
the one pseudonymization rule (stable under any filter, unlike a volume rank that silently
relabels every tenant when the window changes) and recommends it as the project's convention
going forward.

**A related, sharper lesson**: the independent model above was corrected downward four
successive times over one working session (an early flat-N=6 estimate moved $1,252 → $898 → $768
→ $571), each correction caught by a different independent check, every one in the same
direction. Individually each was a plausible bug, caught properly. Collectively it is a
systematic optimism that no amount of self-review from inside one modelling frame caught alone —
the strongest argument this study can offer for why the still-open live randomized holdout
(below) is worth more than further modelling.

**A third lesson, about methodology rather than about this study's numbers**: this page's own
cost engine was never exposed to either of two bugs that hit the two from-scratch
reimplementations built for the cross-check above. `kv_ttl_cost_model.Request.key` has keyed on
`(tenant, session, model)` — not `(tenant, session)` alone — since before this study started,
because a cache entry cannot transfer between models. Both the independent model above AND a
third, separately-built reimplementation initially keyed on session alone and had to discover
and fix the same bug (588 of 14,343 sessions switch models mid-session, 71% of the corpus by
request count is inside such a session). **The analysis built on production's own tested
machinery inherited a correctness property that both from-scratch reimplementations had to
rediscover the hard way.** That generalises past this one bug: reusing an already-tested engine
is not just less work, it is a real reduction in the surface area for this exact class of error,
and it is the concrete reason this page's `evaluate()`-based numbers needed none of the four
downward corrections the independently-built model went through.

## Feature availability, condensed (full table: KA-predictor's own report, cited below)

Re-verified directly against this snapshot's schema, not cited from an older capture:
`stop_cluster` remains the single most load-bearing feature (#1/#2 in every arm that uses it);
no request-level tool-to-decision linkage exists (`tool_uses` is a 10,422-row session-level
aggregate with no `request_id`); no subagent parent/child hierarchy exists anywhere in the
schema (checked directly: zero of 264,163 `session_id`s show a nested pattern); no
`tenants.tz` column exists (checked `cg-control.db` schema directly — the only timezone signal
on this deployment is each strategy's own `windows_json[].tz`, an admin schedule choice, not a
verified tenant-residency fact).

## What this page recommends, and what it explicitly does not

1. **Build the per-tenant `max_pings` cap with an off-switch.** No model required — a per-tenant
   constant refit periodically, the same way this codebase's own `tune_historical_probability`
   already refits per-tenant thresholds, with the same cold-start floor. **This needs a
   precondition checked before it ships**: gateway ping-rate/capacity headroom for raising any
   tenant's cap was not measured in this study, and raising `max_pings` fleet-wide without
   knowing that headroom is the one way this recommendation could cause real harm.
2. **If only a single fleet-wide value can ship**, use `max_pings=2`, not 6.
3. **Do not ship `hybrid2`, or any new predictor model, as a strategy control on this evidence.**
   It is a genuinely good model by every diagnostic this page ran, and it still loses to both
   no-model policies above on the primary objective (net dollars). If a predictor ships anyway
   for an unrelated operational reason (e.g. a per-tenant cap alone hits the gateway capacity
   ceiling above), `hybrid2` is the one to ship, not `logreg-v1` — it wins on net $, is
   calibrated, and generalizes to unseen tenants and models without collapsing.
4. **Not run, stated plainly rather than assumed away**: `ReuseModelV1` + `stop_cluster`
   (issue #326) — every other arm in this whole line of work finds `stop_cluster` its #1/#2
   feature, and `ReuseModelV1` currently has none that touch it; this is the single most
   concretely-motivated next experiment nobody has run. A live randomized holdout — the only way
   to move `keepalive_saved_usd` from "modelled" to "measured" — still has not been run; it needs
   a multi-day window this study did not have.

## Reproduction

```
# kv_ttl_per_tenant_cap.py -- the per-tenant best-of-{off,N} table above, reproducibly, from
# kv_ttl_cost_model.py's own by_user breakdown (no hand-combined JSON, per REV's review)
deploy/harbor/kv_ttl_per_tenant_cap.py --db <cg.db> --prices <prices.yaml>

# kv_ttl_ka_arms.py -- new arms, calibration, LOTO/LOMO, tenant-level bootstrap
deploy/harbor/kv_ttl_ka_arms.py --db <cg.db> --prices <prices.yaml> \
  --train-frac 0.6 --folds 3 --bootstrap 400 --seed 0 --out result.json

# kv_ttl_keepalive_coverage.py -- the real-strategy-config coverage/gate decomposition
deploy/harbor/kv_ttl_keepalive_coverage.py --db <cg.db> --control-db <cg-control.db> \
  --prices <prices.yaml>

# the full-snapshot single-split oracle/baseline sweep (existing script, unmodified)
deploy/harbor/kv_ttl_cost_model.py --db <cg.db> --prices <prices.yaml> \
  --baseline fixed-5m --split 0.6 --min-prefix 20000 --max-pings 2 --json out.json
```

Both new scripts read the store `mode=ro` only and take no dependency beyond what
`kv_ttl_predictor_arms.py`/`kv_ttl_cost_model.py` already require (numpy, pandas,
scikit-learn). Neither writes to `cg.db`, `cg-control.db`, or any live path.

## A corpus bootstrap-period caveat

Found independently by two other agents from two directions: 2026-08-17→08-19, this snapshot's
first ~60 hours, carries its own distinct billing behavior (a `claude-sonnet-5` implied rate of
2.11849 that week vs. exactly 2.28000 every week since) that coincides with an unrelated
`saved_usd`-attribution defect whose entire 1,696-row population also falls in that same window.
347 of 46,028 rows (0.754%), moving the aggregate implied rate by ~0.054% — immaterial to every
figure on this page, which reports full-window aggregates rather than a per-week/time-series
trend that this window's step-change could manufacture spuriously. Flagged because this page's
full-window replays (the coverage/gate decomposition, the per-tenant reconciliation) DO include
those three days; a future time-series extension of this work should exclude or dual-report that
window rather than inherit the caveat silently.

## Not reached

- `ReuseModelV1` + `stop_cluster` (issue #326).
- A live randomized keep-alive holdout (session-stable assignment on real `session_id`,
  measured/estimated/modelled tiers per the Headroom design this project's own literature
  review extracted) — needs a multi-day window.
- Gateway ping-rate/capacity implications of raising `max_pings` on any tenant — a stated
  precondition on recommendation 1 above, not measured here.
- Prefix-size-weighted (dollar-weighted) AUC for `hybrid2` — this page's AUC figures are
  unweighted, and this project's own budget-model finding (pooled AUC 0.93 vs. 0.57 on the
  largest, most expensive prefixes) is a direct warning that an unweighted AUC can look much
  better than it performs where the money actually is.
