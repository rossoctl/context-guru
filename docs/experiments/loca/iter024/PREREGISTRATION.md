# Iteration 024 — pre-registration: the first honest per-component cost and latency measurement

**Written after launch but before any number is read.** Recorded here rather than backfilled: the run
started while the price fix was still being verified, and this file is committed with the frozen inputs
and the reading table before the first arm-seed completes. Any conclusion drawn from a number read
earlier than this file's commit is inadmissible.

## Why this iteration exists

Iterations 022 and 023 asked whether the mechanism works, and answered yes:

| | iter022 | iter023 |
|---|---|---|
| coverage | 100% at batch 4.4 | **99.8%** at batch 6.7 (643/644) |
| econ trigger | 30 evaluated, 30 repaid | 96 asks |
| tokens removed | 187,472 | 426,638 |
| affordability prunes | n/a | 5 |

**What was never measured is cost and latency**, because those counters were pooled at write time. The
`extract` nested map was process-global and BOTH `extract_llm` and `extract_sweep` wrote every field, so
`calls`, `avg_latency_ms` and the whole net-value block were two components with opposite economics summed
under one component's name. `extraction_cost_usd` was wider still, derived from a process-global figure
that also carried `summarize` and `agentdiet`, priced through the haiku card. **#178** fixed that
(per-component scoping, `cost_source`, a nullable net, `unpriced_components`) and is merged into this
branch as `be79553`.

**And a second defect, found while preparing this run.** `cheap_model_price_unconfigured` fired **once per
request in every arm** of iteration 023 — 333/389/413 — because the gate's rates come from
`CHEAP_MODEL_PRICE_*`, which the rig never set. So every allow/suppress decision `extract_llm`'s economic
gate made in 022 and 023 was taken against built-in LIST rates rather than the operator's card. Now
exported in `stage022.sh`; a probe confirms the gate at **0 over 6 requests**.

Both defects mean **no cost or latency figure from 022 or 023 is attributable**, in either direction. This
iteration measures them for the first time.

## Arms — two, not three

| arm | pipeline | differs by |
|---|---|---|
| **A — baseline** | `cfg-iter023-A-baseline.yaml` | — |
| **B — merged** | `cfg-iter023-B-merged.yaml` | `extract_llm_sweep: {evidence: true, econ_trigger: true}` |

The `coref` cutter arm is dropped. It scored 5.00/15 in both prior iterations and has no cost story
pending, so it is the least informative dollar in a run whose subject is cost. Its own question is not
closed — it is deferred.

**`extract_llm` stays IN, deliberately, and this is a reversal.** Iteration 023's plan was to remove it
because its 0/239/0 split looked like a confound. It is not one: the split runs through the **shared
extraction result cache** — the sweep's `putResult` populates it and the tail pass replays those verdicts
(2,291 replays, 2,291 `calls_avoided`, 10,538 lookups at a 66.8% hit rate, arm B only). That is the
treatment's own decisions persisting, which is what freezing exists to do. With #178's accounting fixed
and the rate card configured, that interaction can now be **measured** rather than designed around, and
`acted_fresh` vs `acted_replay` is the counter that separates a free replay from a paid extraction.

## Scope

* `final_64k_set_config.json`, **all five seeds** (42, 123, 456, 789, 2024) × 15 tasks × 2 arms = **150 runs**.
* Interleaved by arm within each seed, so drift over the run lands on both arms equally.
* Estimated **~$170** at iteration 008's measured $1.13/run. An extrapolation, not a measurement.
* `INJECT_EXPAND=always`, `CACHE_MODE=on`, 64k declared window, clearing at 64k, `--max-workers 8`.

**Five seeds is the point, not a luxury.** At n=15 the Clopper-Pearson floor is **21.8%** with *zero*
worsened pairs, so no single-seed design can ever clear the 25% harm gate — both prior iterations were
blocked by arithmetic rather than by evidence. And iteration 023 showed per-chunk variance swamping
everything at one seed: arm C scored 0.200 then 0.800 on identical config.

## Endpoints

**Primary — cost and latency, per component, which nothing has measured yet.**

1. `extraction_cost_usd` per component, **with `cost_source`**. This must read `component` for both
   components. If it reads `partial`, `host_total` or `unpriced`, the primary endpoint has failed and the
   run measures nothing about cost — `unpriced_components` then names the culprit. **Check this on the
   first completed arm-seed rather than at the end.**
2. `avg_latency_ms` per component. Iteration 023 reported 59,009 ms against `extract_llm` and it was
   probably the sweep's figure; this is the first run that can tell them apart.
3. `acted_fresh` vs `acted_replay` per component — separating free replays from paid extractions, which is
   what made `acted: 239` unreadable before.
4. Turns to completion and wall clock, **split by run outcome**. Iteration 023's finding was that arm B's
   turn saving sat on the *failure* path (solved runs 20.1 → 19.8, unsolved 21.1 → 18.5); at n=75 that
   split is worth re-testing rather than assuming.

**Secondary.** Coverage and batch size (does 99.8% hold at n=75?), `drop_unaffordable_pruned`, requests,
errored runs, `expand_unresolved_*`, total cost.

**Reward — a harm gate, and now a clearable one.** Paired per-task means over 15 clusters, two-sided exact
sign test, α = 0.05, with improved/worsened/unchanged counts. **A Clopper-Pearson 95% upper bound on the
proportion of worsened pairs above 25% blocks any positive claim.** At n=75 that bound is reachable, which
is new. No minimum effect size is claimed as a win; a null at this n is still "underpowered", not "no
effect".

## Pre-registered reading

| outcome | conclusion | next |
|---|---|---|
| `cost_source: component` throughout, and B's net value is positive | the mechanism pays, measured honestly for the first time | write it up as the branch's first cost result; take PR #80 out of draft |
| `cost_source: component`, B's net value negative | the mechanism works and does not pay at this band | report the ceiling; the deferral argument needs a band where removal is worth more |
| `cost_source` anything else | **the primary endpoint failed** — cost is still unattributable | fix the pricing path, re-run; draw no cost conclusion |
| latency separates and the sweep owns the 59s | the ask is the expensive leg, as suspected but never shown | the batch/latency trade becomes the next design question |
| harm bound > 25% | blocked | no positive claim, whatever else moved |

## Frozen inputs

| | |
|---|---|
| binary | `cg-i024-proxy-v01`, SHA-256 (first 32) `44e6d6a182a4aeda8bd2744299c5e78b` |
| code commit | `be79553` — `feat/coref-recut` with `origin/main` (#178) merged in |
| arm configs | `deploy/harbor/cfg-iter023-{A-baseline,B-merged}.yaml`, unchanged from iteration 023 |
| task configs | `i024-64k-s{1..5}.json`, one per seed, 15 tasks each |
| rig | `~/cg-loca` (off `/tmp`), `stage022.sh` + `run024.sh`, `CHEAP_MODEL_PRICE_*` exported |
| launched | 2026-09-02T16:52:53Z |

Any code change before all ten arm-seeds finish invalidates the whole run, not one arm.

## Limits, stated in advance

1. **Arm configs carry `min_inventory: 3` and sweep `min_tokens: 100`**, neither of which is the shipped
   default. Identical across arms so neither can bias B−A, but B is not measuring a shippable
   configuration. See iteration 022's Amendment 1 for the derivation.
2. **`extract_llm` is unpinned**, which fixes #120/#134's forcing but means arm A is not `housellm`
   verbatim. No comparison with iteration 022's arm A is admissible — that baseline differed.
3. **The 59 s question may not resolve.** Whether that mean is uniform or tail-driven needs the per-call
   record #177 added; this run has it, but if the tail pass fires rarely the sample may be too small.
4. **`/metrics` remains aggregate** (#180), so nothing here can be cross-checked against a Prometheus
   series. `/stats` carries the breakdown.
