# Iteration 012 — the fold: the headline savings number is inflated 3–8×, and components cannibalise each other

**Date:** 2026-08-21 · **Pre-registered:** `ded9636`, before the run · **Cost:** $89.52 total
($88.56 LOCA + $0.96 of CG's own model calls) · **n = 75**, baseline reused from
[iteration 010](../iter010/results.md).

## The result that matters: `savings_pct` is not a saving

| arm | `savings_pct` | **unique** tokens saved | cache writes | CG model $ | LOCA $ | total $ |
|---|---|---|---|---|---|---|
| `[format]` | 24.2% | **6,977,633** | 2,113,551 | – | 93.23 | **93.23** |
| `[format, coref]` | 25.1% | 6,567,928 | 3,118,809 | – | 97.80 | **97.80** |
| `[format, coref, extract_llm]` | **26.0%** | **5,259,955** | 3,040,796 | 0.96 | 88.56 | **89.52** |

**The ordering inverts.** By `savings_pct` the fold looks best (26.0% > 25.1% > 24.2%). By tokens
actually removed once each, it is **worst** — 5.26M against the lossless baseline's 6.98M.

Two mechanisms, both measured, neither previously visible:

### 1. Overcounting, by a factor of up to 8

CG's own `/stats` reports both figures, and they diverge sharply for the non-deterministic components:

| arm | component | acted | reported saved | **unique** | overcount |
|---|---|---|---|---|---|
| `+coref` | `coref` | 86 | 511,721 | **125,724** | **4.07×** |
| fold | `coref` | 56 | 601,621 | **71,612** | **8.40×** |
| fold | `extract_llm` | 63 | 625,702 | **223,279** | **2.80×** |
| all | `format` | 599–626 | 4.97–6.98M | same | 1.00× |

The same removed content is counted again on every later turn that replays the frozen rewrite.
`format`, which rewrites in place deterministically, has a ratio of exactly 1. **So every headline
`coref` yield figure in this log — including iteration 010's "511,721 tokens" — is inflated, and the
honest number is 4–8× smaller.** In the fold arm `coref`'s real contribution is **71,612 tokens of
23,775,292**: 0.3%.

### 2. The components cannibalise each other

`format`'s own unique saving **falls** as components are added ahead of it: 6,977,633 → 6,442,204 →
4,965,064. `coref` and `extract_llm` remove content `format` would otherwise have reformatted more
cheaply, so **total unique removal goes DOWN as the pipeline grows**. That is an architectural
result, not a tuning detail, and no measurement in this work could see it before, because
`savings_pct` sums per-component credit and therefore rewards double-counting.

## The apparent cost saving is NOT attributable to compaction

The fold's $89.52 against the baseline's $93.23 looked like the first money-saving configuration in
this work. **On inspection it is trajectory variance, and the pre-registered reading should not be
applied.** The token economics point the other way:

- the fold pays **927,245 more cache-write tokens** than the baseline (≈ +$2.32 at the 1.25× write rate);
- it removes **1,717,678 fewer unique tokens** (≈ +$0.34 at cache-read rate, +$3.44 at fresh);
- plus **$0.96** of its own model calls.

So on compaction-attributable terms the fold is break-even at best. The $3.71 difference is the
agent's own path — it also *solved more tasks* (36 vs 33), and a run that succeeds differs in length
from one that flails. [Iteration 010](../iter010/results.md) showed the same magnitude in the
**opposite** direction ($97.80 vs $93.23), which is the tell: **per-arm LOCA cost is dominated by
trajectory divergence and cannot price a component.** Only the token counters can.

## Reward: direction favours the fold, still not significant

| reading | harm | gain | p | harm bound |
|---|---|---|---|---|
| per-protocol, pair level (n=69) | 6 | 9 | 0.607 | ≤16% |
| **per-protocol, task-clustered (n=15)** | **3** | **6** | **0.508** | **≤44%** |
| intent-to-treat, pair level (n=75) | 6 | 12 | 0.238 | — |
| intent-to-treat, task-clustered | 3 | 6 | 0.508 | ≤44% |

3 harm / 6 gain is the most favourable direction measured so far, and under ITT the fold solves
**39 vs 33**. But p=0.508 at 15 independent tasks, and the bound is ≤44%. **This is not evidence the
fold helps reward; it is an absence of evidence that it hurts, at a resolution too coarse to be worth
much.** Churn is 15/69 = 22%, essentially unchanged from iteration 010's 23% — the fold does more
rewriting without flipping more outcomes.

Errors: **0 in the fold arm** against 6 in the baseline, consistent with both earlier arms.

## A pre-registered check that could not work

The pre-registration promised to validate the reused baseline by confirming its error count and solve
rate still matched iteration 010's. **That check is vacuous**: reusing the same run directory compares
the data to itself, so it matched trivially and could not detect the risk it was written for
(gateway drift between arms run 3.5 hours apart). Recorded as a design error in the pre-registration,
not as a passed check. A real check needs a fresh baseline, which is what the $93 was saved by
avoiding — so the saving bought an unverifiable comparison.

## What this settles

**Settled:** `savings_pct` overstates non-deterministic components by 3–8× and must not be quoted
again without its unique counterpart; adding components can *reduce* total unique removal; per-arm
LOCA cost cannot attribute cost to a component; `format` alone does ~95% of the real removal work.

**Not settled:** whether the fold's reward direction is real (needs more independent tasks than LOCA
has); and the deferral question, still blocked on the `apply` parallel-tool-result defect
(`2ec2445`).
