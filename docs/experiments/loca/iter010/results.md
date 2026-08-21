# Iteration 010 — `coref` vs lossless at 32k: the first live reward measurement

**Date:** 2026-08-21 · **Pre-registered:** `ecf4103`, amended `4eecbdc` (n) and `535de68` (ITT), all
committed **before** the arms ran. **Cost:** $191.03 ($93.23 + $97.80), against $340 approved.
**n = 75 runs per arm** (15 tasks × 5 seeds), paired on `(task, seed)`, two-sided.

## Headline: no measurable reward effect, in either direction — and the bound is too wide to call it safe

| | `format` (lossless) | `format`+`coref` |
|---|---|---|
| solved, per-protocol (n=69 pairs) | 33 | 35 |
| solved, intent-to-treat (n=75) | 33 | **38** |
| HTML-400 errors | **6** | **0** |
| tokens removed | 24.2% (6.98M) | **25.1%** (6.95M) |
| `coref` acted | — | **86 / 1266 requests (6.8%)**, 511,721 tokens |
| cost | $93.23 | **$97.80** |

**Reward, as pre-registered on the task-clustered end:**

| reading | harm | gain | p | harm bound |
|---|---|---|---|---|
| per-protocol, pair level (n=69) | 7 | 9 | 0.804 | ≤18% |
| **per-protocol, task-clustered (n=15)** | **4** | **4** | **1.000** | **≤51%** |
| intent-to-treat, pair level (n=75) | 7 | 12 | 0.359 | — |
| **intent-to-treat, task-clustered (n=15)** | **4** | **5** | **1.000** | **≤51%** |

**The pre-registered ≤10% harm bound was not achieved, and could not have been.** That figure assumed
*zero* harm events. Four tasks were net-harmed, so the bound is ≤51% — effectively no constraint. This
is worth stating plainly: **the experiment was bought at a size that only pays off under a clean
result, and the result was not clean.**

## The most informative number is the churn, not the totals

**16 of 69 pairs (23%) flipped outcome**, nearly evenly split (7 harm / 9 gain). `coref` is not
quietly inert here — it changes a quarter of outcomes — but the changes have **no consistent
direction**. Per-task, four tasks net-worse and four net-better.

That pattern is more consistent with `coref` perturbing trajectories than with it systematically
helping or hurting. Two consequences:

1. **A directional claim needs far more independent tasks than LOCA has.** At ~23% discordance split
   near 50/50, detecting a real direction needs hundreds of pairs; LOCA offers **15 independent
   tasks**, verified against its env registry — the shipped roster *is* the whole universe.
2. **Churn is itself a cost.** An agent whose outcome flips on a quarter of runs is less predictable,
   even when the average is unchanged. No prior measurement here could see that, because none paired
   individual runs.

## `coref` removed the transport failures — 6 → 0

The arm with *more* compaction had *zero* HTML-400s against the baseline's six, matching the direction
seen at 64k (10 vs 4). The mechanism is plausible: `coref` shrinks the largest requests below whatever
fails at size. This is why intent-to-treat matters — under ITT `coref` solves **38 vs 33**, and five of
those five extra solves are runs where the baseline's request failed outright.

**But this is a benefit against a rig defect, not against the provider.** The underlying 400 is still
unattributed and may be a CG bug (see the pre-registration's localisation section: identical runs
scored 1 error without the proxy and 6 with it). If the defect is fixed, this benefit disappears. **It
must not be quoted as a reason to run `coref`.**

## Yield: `coref` adds about one point on top of lossless

`format` alone removes 24.2%; adding `coref` reaches 25.1%. `coref`'s own contribution is **511,721
tokens over 1,266 requests**, acting on **6.8%** of them — consistent with its economic gate, and
close to the 4.2% seen at 64k.

**Removing more tokens did not cost less.** The `coref` arm cost **$97.80 against $93.23** — 4.9%
*more* while removing 0.9pp more. Two candidate explanations, not separated here: cache-write costs
from invalidating prefixes, and trajectory divergence (a changed context changes what the agent does,
so per-arm cost is not a controlled comparison). This is the same "removing tokens is not saving
money" result seen on replay, now on live traffic.

## Provisional status, per the pre-registration's own rule

The pre-registration says **≥3 errors of any kind is a rig failure, not a result**. Arm 1 had 6. So
**these conclusions are provisional** and stand only if the transport defect is independent of what is
being compared. Two facts argue it is not fully independent: errors are treatment-dependent (6 vs 0),
and they concentrate in the baseline. Both readings are therefore reported, and the ITT figures are
the ones that survive the confound.

## What this settles and what it does not

**Settled:** `coref` at 32k over 15 tasks × 5 seeds produces **no detectable reward difference** in
either direction, adds **~1pp** of token removal over lossless, and does **not** reduce cost.

**Not settled:** whether the churn reflects a real effect too small for 15 tasks; whether the cost
increase is cache-write economics or trajectory divergence; and whether `coref` helps where the
baseline is genuinely lossy — which is exactly what
[iteration 011](iter011/PREREGISTRATION.md), already running, tests against `summarize`.
