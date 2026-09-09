# LOCA-bench — iteration 004b (reward, cleanly)

**Date:** 2026-08-21 · **Goal:** re-ask [iteration 004](../iter004/results.md)'s question with
`summarize` removed from the pipelines, since every task error there coincided exactly with a
`summarize` firing. **Cost: ~$37** for two arms (baseline reused from 004 — it had no `summarize`
and no errors).

Same 12 tasks, same 64k band, same chain (`LOCA → repair shim → cg-proxy → gateway`), same
deterministic GEM scorer. The only change is dropping `summarize`, which
[`components.md`](../../../components.md) says must **run alone**.

## Result

| arm | solved | errors | **/12** | cost | tokens removed | per-task |
|---|--:|--:|--:|--:|--:|---|
| **`off` — baseline** | **4** | 0 | **0.333** | $21.34 | 0% | `010000101010` |
| **`ns-det`** — `codesmart` − `extract_llm` | **4** | **0** | **0.333** | **$14.67** | 17.1% | `010000101010` |
| **`ns-full`** — + `coref` + prefix-reach `extract_llm` | **5** | **0** | 0.417 | $22.64 | 20.3% | `010001101100` |

For contrast, the same arms **with** `summarize` (iteration 004): 2/12 with 1 error, and 3/12 with
3 errors. Removing it took errors to **zero in both arms**, confirming the diagnosis.

Per component:

| component | `ns-det` | `ns-full` |
|---|--:|--:|
| **`format`** (lossless) | 672,240 (46) — **99%** of the arm's saving | 851,961 (81) |
| **`extract_llm`** | — | **494,180 (38)** |
| `coref` | — | 92,449 (17) |
| `dedup` | 7,734 (6) | 82,683 (13) |
| model calls | **0** | 11 |

## What this proves

**Reward parity, demonstrated more strongly than a matching average.** `ns-det`'s per-task outcome
string is **byte-identical to the baseline** — `010000101010` — the same four tasks solved and the
same eight failed, not merely the same mean. Removing 17.1% of content changed **nothing** about
which tasks succeeded, at **31% lower cost**, with **zero model calls**.

**The lossy components can act at this band without degrading reward.** `ns-full` ran
`extract_llm` 38 times and `coref` 17 times, removed 20.3%, and did not lose tasks.

**`format` remains the dominant lever** — 99% of `ns-det`'s saving from a lossless JSON repack. That
has now held in every configuration measured, replay and live, at every band.

## What this does NOT prove

- **`ns-full`'s +1 task is noise, and is reported as noise.** The [pre-registered
  reading](../iter004/results.md#pre-registered-reading) said a 1–2 task difference at n=12 would be
  treated as sampling variation, and it is: `ns-full` gained tasks 6 and 10 and **lost task 11**. Net
  +1 is not evidence that compaction helps.
- **n = 12, single trial, one band.** Enough for a gross regression check, not for a distribution.
  §8's guard applies: do not stop at first significance.
- **Cost is only partly interpretable.** `ns-det` ran *after* the baseline, so it inherits some of
  the prompt-cache confound described in [iteration 004's design](../iter004/results.md#design).
  Note the direction that is safe to read: **`ns-full` cost MORE than baseline** ($22.64 vs $21.34)
  despite removing 20.3% of tokens — 11 model calls plus pipeline overhead outweighed the saving.
  Removing tokens is not the same as saving money.
- **`summarize` remains untested on live traffic.** Dropping it made these arms valid and left the
  deferral question open — see the warning on [iteration 002](../iter002/results.md).

## Next levers

1. **Re-earn the deferral number with `summarize` isolated**, on live traffic that a provider
   validates. iter002's 71 → 20 came from a pipeline that 400s in production.
2. **More tasks / trials** — n=12 cannot separate `ns-full` from `ns-det`.
3. **A band sweep** — 64k is the only band tested where reward is partial; 32k and 96k are unknown.
4. A second benchmark on the long-horizon axis (UltraHorizon).

## Artifacts

`/tmp/cg-loca/out-arms64ns.log`, `loca-ns-*.log`, `st-ns-*.json`, `ns-det.yaml`, `ns-full.yaml`, and
LOCA's run dirs `/tmp/cg-loca/outputs/inf_claude_api_cg_64k_12_*` on the eval box.
