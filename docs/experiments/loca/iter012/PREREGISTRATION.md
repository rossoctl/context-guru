# Iteration 012 — pre-registration: what does the FOLD add, without `summarize`?

**Written before the run.** Nothing had been launched when this was committed.

[Iteration 011](../iter011/results.md) is blocked: any arm containing `summarize` fails ~37% of runs
because `apply.rebuildCountChanged` drops the body message holding parallel `tool_result`s
(reproduced in `apply/parallel_wire_test.go`, `2ec2445`). Rather than idle on that, this iteration
tests a question that does **not** touch the broken path.

## Question

`coref` alone added ~1pp of removal over lossless and no reward effect
([iteration 010](../iter010/results.md)). Does adding **`extract_llm` with full-body reach** — the
"fold", where one LLM call both trims content and reaches past the cached prefix — add yield worth
paying for, and at what reward cost?

This is the arm the original selection experiment could not speak to: it measured *decision quality*
on captured traffic and explicitly could not speak to reward.

## Arms

| arm | pipeline | source |
|---|---|---|
| baseline | `[format]` | **reused** from iteration 010 (`…_144315`, n=75, $93.23) |
| `s4-fold` | `[format, coref, extract_llm]` | new, this iteration |

`extract_llm` config: `allow_cached_prefix: true` (full-body reach), `allow_on_caching_backend: true`
(required — these runs are `CACHE_MODE=on`, where the component is disabled by default),
`strategy: code`, `min_tokens: 3000`, `llm_max_per_request: 4`.

**The baseline is reused rather than re-run**, and that is a deliberate trade recorded up front: it
saves ~$93 and the config is byte-identical (`ab-format.yaml`, same 32k task set, same binary, same
`CACHE_MODE`/`INJECT_EXPAND`). The cost is that the arms ran at different times, so any drift in
gateway behaviour is not controlled. Pairing is still per `(task, seed)`, and the tasks are
deterministic given their seed, so the comparison remains paired. **If the baseline's error count or
solve rate differs materially from iteration 010's recorded 6 and 33/69, the reuse is invalid and a
fresh baseline is required** — that check runs before any conclusion.

## Endpoints, declared now

**Primary — yield, and whether it is repaid.** `extract_llm`'s acted count, tokens removed, model
calls, and **$/arm**. Iteration 010's central negative result was that removing *more* tokens cost
*more* money ($93.23 → $97.80 for +0.9pp). The fold spends real model calls on top, so the
pre-registered question is not "does it remove more" — it will — but **"is the removal repaid?"**
Reported as the yield triple: eligible / acted / refused-for-economics.

**Secondary — reward.** Paired, two-sided, via `deploy/harbor/reward_pairs.py`, quoted on the
**task-clustered** end. LOCA has 15 independent tasks, so the harm bound floors near ≤18% at zero
events and cannot be improved on this benchmark.

**Tertiary — churn.** Iteration 010's most informative number was that 23% of pairs flipped outcome
with no direction. The fold does strictly more rewriting, so churn is expected to rise; whether it
does is worth knowing independently of the totals, because an unpredictable agent is a cost even at
constant mean.

## How each outcome will be read

| outcome | reading |
|---|---|
| removal up **and** $/arm down | the first configuration in this work that actually saves money |
| removal up, $/arm up | consistent with iteration 010 — token removal is not cost saving under caching; the fold is not worth its calls at this band |
| reward harm at task level beyond the bound | the fold trades accuracy for tokens; requires pricing, not shipping |
| `extract_llm` never acts | the gate suppressed it; report the refusal reasons, and the arm says nothing about the fold itself |

## Pre-declared threats

- **The HTML-400 transport fault is still unattributed** (1/75 without a proxy, 6/75 with one). Errors
  are treatment-dependent, so **both per-protocol and intent-to-treat are reported**, per iteration
  010's amendment 2.
- **`extract_llm` pays a cache-write** when it reaches past the cached prefix. That is the cost the
  break-even in `prefix_econ.go` exists to price, and this arm is a live test of whether it holds.
- **Reused baseline** — validity check stated above, run before conclusions.
- A **null yield** result is a real possible outcome: `extract_llm` fired only 38 times at 64k, and 32k
  contexts are smaller.
