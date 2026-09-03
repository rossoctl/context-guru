# LOCA-bench — iteration 006 (stage 1: what does `coref` add over lossless?)

**Date:** 2026-08-21 · **Status:** running. Design and reading pre-registered before results, as in
[iteration 004](../iter004/results.md), and for the same reason: I have made several wrong
diagnoses on this instrument, so committing the interpretation first is cheap insurance.

## The question changed, and that matters more than the design

Every measurement so far says the same thing: **`format` — a lossless JSON repack — does 92–99% of
all token saving**, in replay and live, at every band. The lossy components compete for what is left.

So the decision-relevant question is no longer *"does `coref` cost reward"* but:

> **Does any lossy component add enough over the *lossless* baseline to justify its risk and cost?**

That makes this an **ablation against `format`-alone**, not against passthrough. Measured against
passthrough, `codesmart` appears to save 17%; measured against `format`-alone it may add 1–3%. The
second number is the one that decides whether this line of work ships.

## Arms

75 tasks (the full `final_64k_set_config.json`), 64k band — the only band with both context pressure
and headroom ([why](../../../results/measurement-limits.md#6-benchmark-suitability)). 6 workers.
Binary: `cg-proxy-v5` (all fixes: cache-write pricing, `allow_cached_prefix`, `prefix_classes`, both
`summarize` shape fixes, atomic exchange boundaries).

| arm | pipeline | model calls |
|---|---|--:|
| `s1-format` | `[format]` — **the real baseline** | 0 |
| `s1-coref` | `[format, coref]` | 0 |
| `s1-off` | passthrough — the floor | 0 |

All three deterministic, so stage 1 costs ~**$220** and carries no LLM spend. Baseline **last**, so
the compaction arms cannot inherit its prompt cache
([why](../../../results/measurement-limits.md#5-cost-model-measured)).

Stage 2 (~$284, `+extract_llm` tail and prefix) runs **only if** stage 1 justifies it.

## Pre-registered reading

**Reward** (the gate). n=75 detects only ~20pp effects, so:

- **Zero discordant pairs** vs `format` → the strongest available evidence of safety, as in
  iteration 004b.
- **Any discordance** → report the count and p, and treat <6 discordant as **noise**, not signal.
- **`format` vs `off` is a first-class safety question**, not a formality: `format` rewrites *every*
  JSON tool result and is on by default in every preset. If it loses tasks, that is the most
  important finding available here.

**Yield** — three numbers, never one, or the result is uninterpretable
([why](../../../results/measurement-limits.md#3-eligible--acted--yield-measures-the-throttle-not-the-capability)):

| number | source |
|---|---|
| **eligible mass** | `coref`'s `class_*` gates — what the index judged dead |
| **acted mass** | what survived all four economic gates |
| **refused for economics** | `batch_too_small` + `break_even` + `rewrite_budget` |

**The decision rule, and it is not symmetric:**

- acted-yield over `format` is **material** and reward is undamaged → stage 2 is justified.
- acted-yield is **negligible and** refused-for-economics is **small** → `coref` genuinely has little
  to remove after a lossless pass. **Rethink, do not spend stage 2.**
- acted-yield is **negligible but** refused-for-economics is **large** → `coref` is *economically
  throttled*, not incapable. Different problem, and the lever is the
  TTL observation (`docs/proposals/coref-compaction.md`),
  not more arms.

**Also collected free:** `cache_opportunity.py` over all three arms, to see whether any prefix
rewrite was already free. Expected ~0 on LOCA (local mock tools, turns seconds apart), which is a
property of the benchmark rather than an answer.

## What this cannot show

- **Neither merged design.** This tests `coref`'s *deterministic* dropping. It does not test the
  merged call (`docs/proposals/coref-compaction.md`)
  — co-reference reasoning inside `extract_llm`'s prompt — which is untested and **not** what the
  selection experiment refuted. It also does not test the implemented prefix-reach-with-index-gate.
- **Generalisation.** LOCA is Tier-2/3-heavy, adverse to an exact-match detector. A null here cannot
  be generalised to Tier-1-rich long-horizon traffic, which **no available benchmark provides**.
- **Small effects.** ~20pp is the floor at n=75. If `coref`'s marginal contribution is a few percent
  of tokens, **no affordable experiment can resolve its reward impact** — which is itself a finding,
  and a reason to rethink rather than scale.

## Result

_Pending — checkpoint report to follow._

## Artifacts

`/tmp/cg-loca/out-s1.log`, `loca-s1-*.log`, `st-s1-*.json`, `ab-format.yaml`, `ab-coref.yaml`,
`task-configs/cg_64k_75.json`, `stage1.sh`, `s1arms.sh` on the eval box.
