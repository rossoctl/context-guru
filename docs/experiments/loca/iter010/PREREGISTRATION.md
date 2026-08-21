# Iteration 010 — pre-registration (written BEFORE the run)

**Status at time of writing: nothing has been run.** This file exists because
[iteration 007](../iter007/results.md) declared its margin *after* seeing results, and so bought a
≤26% harm bound it could not use. The convention in [the index](../../README.md) is to commit the
design *and the reading of each outcome* before the numbers exist.

**Approved budget: ~$340.** Approved margin: **bound harm at ≤10%.**

## Question

Does adding `coref` to a lossless deterministic pipeline change task reward?

Reward is the axis **nothing in this repo has measured**. The selection experiment
([iter009](../iter009/results.md)) settled decision quality on captured traffic and says so in its own
warning box; [measurement-limits §1](../../../results/measurement-limits.md) prices reward and explains
why 64k could not deliver it. [Iteration 008](../iter008/results.md) established 32k as the only band
with headroom in both directions (53% base solve rate vs 25% at 64k).

## Arms

| arm | pipeline | role |
|---|---|---|
| `s2-format` | `[format]` | **lossless** baseline — reformatting only, removes no information |
| `s2-coref` | `[format, coref]` | adds the co-reference cut under test |

`format` is the baseline rather than passthrough deliberately: the question is what `coref` adds over
lossless, not over nothing. Same binary, same config, same task set, same seeds, same order.

## Design

- **Band:** 32k (upstream `final_32k_set_config.json` task set).
- **n = 30 pairs**: the **15 tasks × the first 2 seeds** of each. Requires defeating LOCA's
  `group_by_seed` (default `True`, not exposed as a CLI flag), which otherwise collapses the set to 15.
- **Paired** on `(task, seed)`; identical pairs in both arms.
- **Two-sided**, per the argument in [measurement-limits §1](../../../results/measurement-limits.md):
  where the baseline is itself lossy, selective removal can *raise* reward, so a one-sided harm bound
  would be unable to register a gain even if one occurred.
- **Path:** LOCA → fixed `repair_shim.py` → cg-proxy → benchmark gateway
  (`ANTHROPIC_BASE_URL=$ANTHROPIC_BENCHMARK_BASE_URL`, `ANTHROPIC_CUSTOM_HEADERS=` cleared).

## Endpoints, declared now

**Primary — non-inferiority on reward.** Harm event = a pair where `s2-format` solved and `s2-coref`
did not. Report the upper 95% (Clopper–Pearson) bound on the harm rate.

**The bound is bracketed, and both ends are stated up front**, because the 30 pairs are 2 seeds of 15
tasks and are therefore correlated, not independent:

| assumption | effective n | bound if 0 harm events |
|---|---|---|
| pairs independent (optimistic) | 30 | ≤ 10% |
| task is the unit (conservative) | 15 | ≤ 18% |

The truth is between. **The conservative figure is the one to quote in any claim**; the optimistic one
is reported only for comparability with the cost table in measurement-limits §1. Analysis clusters by
task — seeds averaged within task — with the pair-level count shown alongside.

**Secondary — superiority.** McNemar exact on discordant pairs. Given ~1 discordant pair in 10 at 64k,
this is expected to be non-significant at n=30 and is reported for its point estimate and direction,
**not** as a test we are powered to pass.

**Tertiary — savings.** `coref` acted-count and tokens removed from CG's own `/stats` counters. This
needs no significance test (thousands of per-request observations) and is reported as the yield triple:
eligible / acted / refused-for-economics.

## How each outcome will be read

| outcome | reading | consequence |
|---|---|---|
| 0–1 harm events, no gain | non-inferior within a wide bound; `coref`'s savings are ~free on reward | ship-able at this band; extend to n=45 only if a tighter bound is wanted |
| 0 harm events **and** ≥3 gains one-way | the two-sided argument is supported — the lossy baseline is being beaten | strong result; extend to n=45 to firm it up |
| ≥4 harm events | real degradation | `coref` needs its gates tightened before any reward claim |
| ≥3 errors of any kind | rig failure, not a result | fix and re-run; do not interpret |

**Stopping rule:** arms run to completion. No interim peeking at reward to decide whether to continue —
interim looks inflate false positives, and iteration 007 was stopped on *rig* grounds, which is a
different and legitimate reason.

## Pre-declared threats

- **Correlated seeds** — handled by the bracket above; the conservative end is the quotable one.
- **20% of pairs may be unusable** if any task errors; n falls accordingly and the bound widens. Report
  actual n, never planned n.
- **Tier-3 blindness does not apply here.** Reward is blind to nothing, which is the entire reason for
  running it. This experiment is not subject to iter009's ground-truth limitation.
- **`cut_unreferenced` carries a 21–24% false-drop floor** ([iter009](../iter009/results.md)). If the
  primary shows no harm despite that, the most likely explanation is `expand` recovery or the agent
  not needing the dropped content — both worth stating rather than claiming the floor is wrong.
