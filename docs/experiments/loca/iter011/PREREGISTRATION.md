# Iteration 011 — pre-registration: does selective compaction DEFER summarization?

**Written before the run. Nothing had been launched when this was committed.**

This is the question the work started from: *"we want to test if it's worthwhile deferring the context
summarization, when using extract_llm+coref on full body and pay the write cache"*, and *"we have
summarizer as well, so we can use it before the reset if the context max is reached, to mimic Claude
Code behavior."*

It has never been measured on live, provider-validated traffic. [Iteration 002](../iter002/results.md)
showed the *mechanism* on replayed traffic; [iteration 005](../iter005/results.md) tried it live and
was blocked by three message-shape defects in `summarize`, each masking the next. Those are fixed
(`80e95d5`, `0971a32`, `2d6902d`) and guarded by `schema.ValidateShape` plus a test over all 11
presets. The binary in use (`/tmp/cg-coref/cg-proxy-v5`, 2026-08-21 08:54) loads
`pipeline="[format summarize]"` cleanly.

## Why deferral should matter at all — the argument being tested

A summary is **lossy in a way that is worse for co-reference than a trim**. A trim removes whole
messages, so what is gone is at least knowable; a summary paraphrases exact identifiers into prose,
silently corrupting the literal tokens Tier-1 matching depends on. So summarising *early* destroys
precisely what selective removal would have kept. If selective compaction can postpone or avoid a
summary, it should preserve accuracy **and** save money — the effect is two-sided, per
[measurement-limits §1](../../../results/measurement-limits.md).

## Arms

| arm | pipeline | role |
|---|---|---|
| `s3-sum` | `[format, summarize]` | **blunt baseline** — summarise when large, à la Claude Code `/compact` |
| `s3-coref-sum` | `[format, coref, summarize]` | selective removal first, then summarise if still needed |
| `s3-fold-sum` | `[format, coref, extract_llm, summarize]` | the **fold**: `extract_llm` full-body (`allow_cached_prefix: true`) + `coref` + summarise |

`summarize` config is **identical in all three arms** — only what precedes it differs. Any difference
in its firing rate is therefore attributable to upstream components, which is the whole design.

## The trigger, chosen from measured data rather than guessed

`trigger.min_request_tokens: 30000`, from the real request-size distribution at 32k (2,362 requests,
arm 1 of [iteration 010](../iter010/PREREGISTRATION.md)):

| percentile | request tokens |
|---|---|
| p50 | 14,154 |
| p75 | 33,603 |
| p90 | 43,728 |
| max | 110,163 |

**35% of requests exceed 30,000**, so the trigger fires mid-trajectory on most sessions without firing
constantly. 40,000 would reach only 12% of requests — too rare to measure deferral; 20,000 reaches 46%
and would fire almost immediately, leaving nothing to defer.

## Endpoints, declared now

**Primary — deferral, measured directly.** `summarize`'s firing count and model-call count per arm,
from CG's `/stats`. This is a count over thousands of requests, not a sampled estimate, so it needs no
significance test. **Deferral is established if `s3-coref-sum` and `s3-fold-sum` fire `summarize`
strictly less often than `s3-sum`.**

Reported as the yield triple — eligible / acted / refused — because a single number cannot separate
"nothing left to summarise" from "economically throttled".

**Secondary — reward.** Paired on `(task, seed)`, two-sided, via `deploy/harbor/reward_pairs.py`.
**Quoted on the task-clustered end**, as pre-registered in iteration 010: LOCA has exactly **15**
`*_s2l` environments — verified, the shipped roster is the entire task universe — so 75 runs are 5
seeds of 15 tasks and no seed count makes them independent. The conservative bound floor is ~≤18% and
**cannot be improved on this benchmark**.

**Tertiary — cost.** Total $/arm and tokens removed per component. Note the sign convention that keeps
tripping this work up: removing tokens is not the same as saving money once cache-writes are priced.

## How each outcome will be read

| outcome | reading |
|---|---|
| fewer `summarize` firings in B/C **and** no reward harm | **deferral works** — the founding claim is supported live for the first time |
| fewer firings but reward harm at task level | deferral trades accuracy for cost; the trade needs pricing, not celebration |
| no reduction in firings | selective removal does not shift the trigger at this band; the claim fails **here**, and the band is a stated limit, not a refutation everywhere |
| `summarize` errors or emits invalid requests | the iteration-005 failure recurred; fix, do not interpret |

## Pre-declared threats

- **`summarize` is documented as "run it alone (its own preset) — it restructures the whole
  transcript."** These arms deliberately violate that, because the deferral question *is* the
  interaction. Shape validity is therefore a monitored outcome, not an assumption: any 400 or
  shape violation invalidates the arm rather than counting as a result.
- **`summarize` + `cachesplit` remains untested** and is excluded from all three arms.
- **`extract_llm` full-body pays a cache-write.** Arm C is expected to be *more* expensive; the
  question is whether deferral repays it, and cost is reported per arm rather than assumed.
- **Trigger choice is a free parameter.** It is fixed here, in advance, from measured percentiles, and
  is identical across arms. It must not be retuned after seeing results.
- **`coref` acted on only 4.2% of requests at 64k**, and 32k contexts are smaller, so the deferral
  effect may be small. A null result is a real possible outcome and is pre-committed above.
