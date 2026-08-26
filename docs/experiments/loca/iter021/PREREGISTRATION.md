# Iteration 021 — pre-registration: what does `merged` add to the shipped product?

**Written before the run.** Iteration 020 got the mechanism working but had **no concurrent baseline**,
so its 22/75 cannot be attributed to the design. This iteration is the paired comparison, and the
question is deliberately narrow: **what does the merged adjudicator add on top of the pipeline the
product already ships?**

## The two arms

Identical but for one inserted component. Every other setting, the binary, the task set and the seeds
are shared.

| | pipeline |
|---|---|
| **A — baseline (the shipped product)** | `[format, toon, dedup, failed_run, cmdfilter, extract, cachesplit, summarize]` |
| **B — treatment** | `[format, toon, dedup, failed_run, cmdfilter, `**`extract_llm`**`, extract, cachesplit, summarize]` |

`extract_llm` is placed **before** `extract`, following `ns-full.yaml`'s existing order: the
deterministic extractor shrinks outputs below the model pass's floor if it runs first, which would
starve the batch — the failure mode iteration 020 spent five aborts on.

**`coref` is in NEITHER arm.** It is new on this branch, so it is not part of "what we had beforehand".
Its own value is a separate question and a later arm.

## Two assumptions that change what the result means — flagged, not buried

**1. The treatment arm runs with `economic_gate: false`, which is not a shippable configuration.** The
gate prices each candidate against the cost of a whole model call, which is correct for the per-output
loop and wrong for a design making one call per request; left on, it suppressed 1,497 candidates against
224 that got through and the arm does not run the design at all
(`docs/results/min-tokens-vs-economic-gate.md`). So arm B measures **merged with its cost gate
disabled**. Making the gate batch-aware is the real fix and is deliberately *not* being done first: a
new, untested cost model introduced immediately before freezing the binary is how a measurement gets
quietly distorted. If B shows value, the gate fix becomes worth building and the arm is re-run.

**2. Both arms carry the injected `context_guru_adjudicate` tool**, because the proxy injects it
unconditionally. Arm A cannot use it. That is deliberate — it keeps the two arms' `tools` arrays and
therefore their cache behaviour comparable — but it means arm A is not byte-identical to the shipped
product. Cost is a few hundred cached tokens per request, and `adjudicate_stray` was **0** across 2,303
requests in iteration 020, so the risk of the agent calling it is measured, not assumed.

## Frozen inputs — the thing that invalidated iterations 014, 016 and 018

Those three cannot be compared to each other because each ran a different binary. For this iteration:

* **one binary for both arms**, its git commit and SHA-256 recorded in `results.md` before launch;
* the same task config, `task-configs/final_128k_set_config.json` — 15 tasks × 5 seeds (`state0..state4`);
* arm configs committed under `deploy/harbor/` before launch;
* `INJECT_EXPAND=always`, `CACHE_MODE=on`, 128k declared window, LOCA clearing at 128k, `--max-workers 8`.

Any code change after launch invalidates both arms, not one.

## Primary endpoint, and the test declared in advance

Per-run accuracy is available per seed at `tasks/<Task>/state<N>/eval.json`, so the comparison is
**paired**: 75 (task, seed) pairs, each arm scored on the same seed.

Accuracy is not binary on this benchmark (observed values 0.0, 0.2, 0.6, 0.8, 1.0), so:

* **Primary — task-clustered.** Mean accuracy per task (15 clusters), paired Wilcoxon signed-rank,
  two-sided, α = 0.05. **The clustered test governs.** Five seeds of one task are correlated, not five
  free observations, and treating 75 as independent is the mistake this file exists to avoid.
* **Secondary — per-pair.** All 75 pairs, paired Wilcoxon signed-rank, plus explicit counts of
  **improved / worsened / unchanged** pairs. Reported as a sensitivity check, never as the headline.
* **Harm.** Clopper-Pearson upper bound on the proportion of worsened pairs. Declared **before** the
  run, per iteration 007's failure: **a harm upper bound above 25% blocks any positive claim**, whatever
  the point estimate does.

**Margin:** no minimum effect size is claimed as a win. The honest reading of a null result at this n is
"underpowered", not "no effect" — previous harm bounds reached ±39%.

## Pre-registered readings

| outcome | conclusion | next |
|---|---|---|
| clustered p < 0.05, direction positive, harm bound ≤25% | merged adds value over the shipped pipeline | make the economic gate batch-aware, re-run to confirm on a shippable config, then decompose (coref alone; per-output alone) |
| clustered null, per-pair positive | suggestive and underpowered; seeds are carrying it | do not claim; decide whether more seeds are worth the money |
| clustered null, both directions flat | no detectable marginal value at this n and this cost | close merged; keep the deterministic pipeline; report the ceiling honestly |
| clustered negative, or harm bound > 25% | merged harms the product | close it, and record which removals caused the harm |

## Secondary endpoints

* **Cost.** Reported, but iteration 012 established that per-arm LOCA cost cannot price a component.
  **CG model spend is the only attributable figure** — $33.07 in iteration 020, against $9.21 when the
  adjudicator ran on the cheap model. My prior, stated in advance: **merged costs parity to ~20% more**
  in total. CG arms have historically sent 32-35% fewer tokens and billed 12-14% more, because a removal
  inside a cached prefix invalidates everything after it. The case for this design is reward, not cost.
* **Deferral.** `summarize.acted / requests`. And it must not be read as quality: a badly-chosen removal
  defers `summarize` exactly as well as a well-chosen one.
* **Yield.** `saved_tokens_unique`, never `savings_pct` — the latter is inflated 3-8× by frozen replays
  re-crediting the same removal (iteration 012).
* **Mechanism health**, or the arm is uninterpretable regardless of its reward: `merged_offered` vs
  answered (65% in iteration 020), `merged_unparseable`, `merged_reply_truncated`,
  `merged_quote_not_verbatim`, `merged_drop_contradicts_obligation`, `prefix_ask_cache_read_ZERO`,
  `adjudicate_stray`, `expand_unresolved_missing`.

## Checkpoints

* **~200 requests, arm B:** offered-vs-answered ≥ 50%, `prefix_ask_cache_read_ZERO` ≈ 0,
  `merged_reply_truncated` = 0. If the mechanism is not running, **abort** — a completed arm that never
  ran the design is what iterations 014, 016 and 018 each produced.
* **Both arms:** one proxy only, bound port verified. The stale-proxy bug invalidated two arms in
  iteration 013 by running a previous arm's pipeline on a reused port.
* **Reminder from iteration 020:** LOCA prints `Overall Success`, which counts runs that did not error.
  It is **not** the solve count. The comparable metric is accuracy-weighted, and the two differed by 3×.

## Cost and scope

Two arms, n=75 each, ~$240 per arm, **~$480 total**. If arm B wins, the decomposition arms (`coref`
alone; per-output `extract_llm` alone) run on the **same binary and seeds** for another ~$480 without
re-running the baseline. Note in advance that a per-output arm cannot use prefix asks, so it would
conflate "merged vs per-output" with "prefix-ask vs none".
