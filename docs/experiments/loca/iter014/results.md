# Iteration 014 — the MERGED design, measured: it mostly declines to act

**Date:** 2026-08-23 · **Pre-registered:** `fe4d8d0`, before the run · **Both arms on `cg-proxy-v8`**,
so the binary is not a difference between them · **128k band, n=75/arm** · **Cost: ~$460** for the pair.

## The result

| | separate components | **merged** |
|---|---|---|
| pipeline | `[format, coref, extract_llm, summarize]` | `[format, extract_llm(merged), summarize]` |
| solved (per-protocol) | 15 / 56 | 13 / 56 |
| **solved (intent-to-treat, n=75)** | **16** | **16** |
| errors | **15** | **8** |
| `extract_llm` acted | 223 (6.3%/req) | **1,827 (61.3%/req)** |
| `summarize` acted | 2,511 (70.8%/req) | **1,754 (58.8%/req)** |
| **CG model calls** | 2,700 | **2,030 (−25%)** |
| **CG model spend** | **$22.66** | **$9.50 (−58%)** |
| LOCA spend | $204.94 | $223.22 |
| total | **$227.60** | **$232.72** |

**Reward is indistinguishable.** Intent-to-treat: **16 vs 16 solved**, 6 harm / 6 gain, p=1.000.
Task-clustered per-protocol: 2 net-harmed, 0 net-gained, p=0.500, harm bound ≤39%. There is no reward
signal here in either direction, at a resolution too coarse to be worth much — and the error counts are
asymmetric (15 vs 8), so per-protocol exclusion is confounded and ITT is the reading that survives.

## What the gate counters say, and this is the real answer

The merged counters exist to separate "the model kept everything" from "the model was never asked" from
"the model returned junk". They are unambiguous:

| verdict | count | share |
|---|---|---|
| **`merged_keep`** | **1,824** | **88%** |
| `merged_drop` | 249 | 12% |
| `merged_trim` | **1** | 0.05% |
| `merged_trim_not_contained` | 4 | rejected as invented |
| `merged_unparseable` | 19 | 0.9% |
| `merged_call_failed` | 9 | 0.4% |

**Shown fifteen outputs with their co-reference evidence and told the true cost of a wrong removal, the
model keeps 88% of them and trims essentially never (1 of 2,074).** The pre-registered reading for this
outcome was written before the run: *"`merged_keep` dominates → the model declines to act on evidence;
consistent with iteration 009 and a negative answer."*

That is what happened. And it is the *opposite* failure from the one the per-output design showed —
there, a model shown one output at a time dropped nearly everything (6% live-kept). Shown many at once
and told removals are not recoverable, it becomes conservative instead. Both are the same underlying
fact from opposite sides: **the model is not discriminating between outputs; it is responding to how the
question is framed.**

The single trim in 2,074 decisions is worth its own note. Trimming — returning a subset of records
verbatim — is where the merged design's value was supposed to lie, since it is the judgement an exact
matcher cannot make. The model essentially never did it.

## Where merged genuinely wins

**Efficiency, which was the second pre-registered endpoint.** One call per request instead of up to
`llm_max_per_request`: **2,030 calls against 2,700 (−25%)** and **$9.50 of CG model spend against
$22.66 (−58%)**, for the same reward. It also deferred `summarize` more (58.8% vs 70.8% of requests)
and had **half the errors** (8 vs 15).

Total cost is $232.72 vs $227.60 — 2% apart, and [iteration 012](../iter012/results.md) established
that per-arm LOCA cost cannot price a component, so that difference should not be read as anything.
**The CG-spend comparison is the one that is attributable, because it is a direct count of this
component's own calls.**

## Yield, in unique tokens

| arm | component | acted | reported | **unique** | overcount |
|---|---|---|---|---|---|
| merged | `extract_llm` | 1,827 | 573,265,890 | **18,065,171** | **31.7×** |
| separate | `extract_llm` | 223 | 23,131,709 | 1,953,083 | 11.8× |
| separate | `coref` | 513 | 16,559,942 | 874,969 | 18.9× |

Merged `extract_llm` removes **9× more unique tokens** than separate `extract_llm` + `coref` combined
(18.1M vs 2.8M) — so the fold does more work per call, which is consistent with acting on 61.3% of
requests instead of 6.3%. **It also carries the largest overcount ratio measured anywhere in this work,
31.7×**, which is what happens when a component acts often and its rewrites are replayed on every later
turn. Anyone quoting its 573M "saved" would be overstating by a factor of thirty.

## Verdict

**The merged design is cheaper per decision and no worse on reward, but it does not do the thing it was
built to do.** It was meant to catch what the deterministic index structurally cannot — Tier-2/3 reuse
and the anchor-vs-payload call. Instead it keeps 88% and trims once in two thousand.

Combined with [iteration 009](../iter009/results.md) — where the free index beat every model arm at 95%
live-kept and 11% false-drop, and neither floor-symmetry nor a widened Tier-2 ground truth closed the
gap — the honest position is that **model-based selection has now failed in both directions**: reckless
when shown one output, inert when shown many. The remaining case for merged is purely economic (fewer
calls for the same outcome), not qualitative.

**Not settled:** whether a middle framing exists between "drops everything" and "keeps everything". The
two arms differ by ~26 points of prompt framing in iteration 009's measurements, so framing clearly
dominates the model's behaviour — which is itself an argument that the decision is not really being made
on the evidence.
