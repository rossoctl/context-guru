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

---

# AMENDMENT 1 — n raised from 30 to 75 (written before the run; nothing had completed)

**Trigger:** a verification check on the first 5 evals of arm 1, before the arm was allowed to spend.
The arm was stopped at 5 evals (~$0 recorded) and this amendment written before relaunching.

**What the check found — three of this experiment's premises were wrong.**

`state0`…`state4` in a LOCA output directory are **the 5 seeds**. Every run reported in iterations
007 and 008 read `state0` only, so every "n=15" was 15 observations out of **75 that were actually
executed and paid for**:

| run | as reported | corrected |
|---|---|---|
| 32k baseline (iter008) | n=15, 53%, $5.67/task | **n=75, 52.7%, $1.13/run** |
| 64k `s1-format` (iter007) | n=15, 25%, $7.59/task | **n=75, 33.3%, $1.52/run** |
| 64k `s1-coref` (iter007) | n=14 | **n=62** |

And the premise this experiment's sizing rested on — *"`group_by_seed` collapses 75 configs into 15
runs"* — **is false**. It groups for *reporting*; all 75 configs execute either way. iteration 008
proves it: unpatched, 75 configs, 75 evals. The `run_claude_api.py` patch was therefore unnecessary
and **has been reverted**, so this run uses the stock tool.

**Consequences for the plan as pre-registered above:**

1. **Cost was overstated ~5×.** The full 5-seed, 2-arm experiment costs **~$170**, not $850. The
   approved $340 covers it twice.
2. **n=30 would discard 60% of the data for no saving.** Raising to the full 75 configs per arm is
   strictly more information at less than the approved budget.
3. **Discordance is ~23%, not the ~10% assumed.** Re-pairing stage 1 across all seeds gives **48
   usable pairs, 11 discordant (7 favouring `coref`, 4 favouring `format`, McNemar p≈0.55)** — not
   the "1 discordant in 10, p=1.00" reported in iteration 007. Direction favours `coref`; the test is
   not significant and is not claimed to be.

**Amended design:** both arms on the full `final_32k_set_config.json` (75 configs = 15 tasks × 5
seeds). Everything else above — arms, band, endpoints, two-sidedness, stopping rule, threats — stands
unchanged.

**Amended bound:**

| assumption | effective n | bound if 0 harm events |
|---|---|---|
| pairs independent (optimistic) | 75 | ≤ 4% |
| **task is the unit (conservative, quotable)** | **15** | **≤ 18%** |

**The conservative end barely moves, and that is the honest headline.** It is driven by the number of
independent *tasks* (15), not the number of seeds. Extra seeds buy precision *within* a task, not more
independent tasks. So this amendment buys a better point estimate and a better-powered McNemar, **not
a materially tighter harm bound.** Tightening that end needs more distinct tasks, which LOCA's 32k set
does not have.

**Why this is an amendment and not a post-hoc choice:** it is committed before the run, the trigger
was a verification check rather than an outcome, no reward comparison from the amended design existed
when it was written, and it moves n in the direction that makes the pre-registered test *harder* to
pass by luck.
