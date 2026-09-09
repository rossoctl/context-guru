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

## AMENDMENT 1 — errored runs score ZERO (written before any solve count was read)

Arm A produced **17 upstream 400s in 3,347 requests (0.5%)**, all `"prompt is too long"`, on bodies of
2.6 MB to 14.8 MB. Diagnosed while arm A was still running and before its results existed.

> **CORRECTION (2026-09-01).** This section first said **16** 400s. Re-counting
> `capfail-i21A.jsonl` gives **19 records: 17 oversize-prompt 400s plus 2 transport failures**. The
> original 16 could not be reconstructed from any filter over that file. The digests record per-message
> STRUCTURE only — no content, no per-message sizes, `rig_seq` null throughout — so the related
> inference that those bodies had few enough lines for `collapse`'s line window to miss them
> **cannot be tested from this record in either direction**, and should not be relied on.

**Cause, and it is mine.** A single tool output arrives larger than any component can reduce: `extract`
matched no noise pattern (`acted` = 0, `no_obvious_noise` 16,891), `cmdfilter` matched nothing
(`acted` = 0), `toon` needs a uniform object array, `dedup` needs an exact duplicate — so the only real
compactor is `summarize`, which protects `keep_last: 3`, and a fresh oversized output sits in exactly
that protected tail. `extract_llm` would decline it too, by design: `over_model_context` leaves any
output that exceeds the compaction model's context verbatim.

The product has a component for precisely this — `collapse`, "the content-agnostic fallback for an
oversized tool output that no more specific component handled", which keeps a head/tail window and
stashes the original behind a marker. It is in the `general` and `codesafe` presets and **not** in
`codesmart`, which is what these arm configs descend from. So this is a rig-configuration error, not a
product defect — with the caveat that a user of the shipped `codesmart` preset has the same gap.

**Why the arms are NOT being restarted:** both lack `collapse`, so both take the same class of failure,
and the comparison stays fair in expectation. Restarting costs both arms.

**What is being fixed instead — the analysis plan, stated before any outcome is known.** Errors are the
one place where this omission could bias the result: arm B removes more, so it may error *less*, and
excluding errored runs would then compare arm B's survivors against arm A's. Iteration 014 hit this
exactly, with 15 errors against 8, and concluded that intent-to-treat is the reading that survives.

  * **Primary analysis is INTENT-TO-TREAT.** A run that errored, or has no `eval.json`, scores
    **accuracy 0**. Every one of the 75 pairs is scored.
  * **Per-protocol** (errored pairs dropped) is reported as a sensitivity check only, alongside the
    error count per arm, and never as the headline.
  * **The error counts themselves are a reported endpoint.** A large asymmetry is a finding about
    oversized-output handling, whichever direction it points.

Nothing else changes: the clustered test still governs, the 25% harm bound still blocks a positive
claim, and no minimum effect size is claimed as a win.

## AMENDMENT 2 — arm B ran BELOW its own coverage gate, and was continued anyway

At 477 requests, arm B's verdict coverage was **34%**. The checkpoint above requires **≥50%** and says
to abort below it. Recording the decision here rather than in the results, so it cannot be
retrospectively smoothed over.

**Continued, on the operator's call.** The gate was written to catch "the design never ran", and by every
other measure it is running: `prefix_ask_cache_read_ZERO` = 0, `merged_reply_truncated` = 0,
`adjudicate_stray` = 0, 44 real drops and 686k unique tokens removed by that point. What is happening is
PARTIAL ANSWERING — the model returns verdicts for about a third of a full 12-item batch — not a dead
mechanism. Coverage also rose through iteration 020 (46% → 51% → 65%), so 34% at 20% of the run may be
an early reading rather than the final one.

**The cost of continuing is stated, not hidden:** arm B's numbers are a **FLOOR** on what the design can
do, because it acted on roughly a third of the candidates it identified. A null result therefore cannot
be read as "merged does not help" — only as "merged at ~34% coverage does not help detectably".

**The honest alternative was to abort**, which would have preserved the gate's authority at the cost of
re-running arm B, and there is no tested coverage fix to re-run it *with*: the leading candidate is a
SMALLER batch, since a probe answered 6 of 6 on six items against ~34% on twelve, and that is untested.

Nothing else changes. The clustered test governs, ITT scores errored runs zero, and the 25% harm bound
still blocks a positive claim.

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
